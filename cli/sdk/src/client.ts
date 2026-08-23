// The push client (cli/SPEC.md §5.3): batch 1.5s / 64 KB, in-memory ring
// capped at 8 MB with an explicit drop event, exponential backoff with jitter,
// idempotent batches (a failed batch is retried byte-identical - the server
// content-addresses bodies, so a replay cannot double-write), honors the
// receipt's sampling instruction. Nothing here ever throws to the caller and
// nothing holds the event loop open (all timers are unref'd).

import { scrub } from './scrub.js';

export const SDK_VERSION = '0.1.0';

const MAX_BUFFER_BYTES = 8 * 1024 * 1024;
const FLUSH_AFTER_MS = 1500;
const FLUSH_AT_BYTES = 64 * 1024;
const MAX_BATCH_BYTES = 1024 * 1024;
const MAX_BACKOFF_MS = 30_000;
const REQUEST_TIMEOUT_MS = 10_000;

export type Attrs = Record<string, string | number | boolean>;

interface Line {
  text: string;
  level: string;
}

export class Client {
  private key: string | undefined;
  private endpoint: string;
  private lines: Line[] = [];
  private bytes = 0;
  private dropped = 0;
  private pendingBatch: string | null = null;
  private pendingCount = 0;
  private timer: ReturnType<typeof setTimeout> | null = null;
  private inflight: Promise<void> | null = null;
  private attempt = 0;
  private backoffUntil = 0;
  private keep: Map<string, number> = new Map();
  private warned = false;
  private sentInstallVerified = false;

  constructor(env: NodeJS.ProcessEnv = process.env) {
    this.key = env.UPCONTROL_API_KEY?.trim() || undefined;
    this.endpoint = (env.UPCONTROL_ENDPOINT?.trim() || 'https://upcontrol.io').replace(/\/+$/, '');
  }

  get hasKey(): boolean {
    return this.key !== undefined;
  }

  enqueue(fields: Record<string, unknown>, level: string): void {
    try {
      if (!this.key) {
        if (!this.warned) {
          this.warned = true;
          process.stderr.write('upcontrol: UPCONTROL_API_KEY is not set - track() is a no-op\n');
        }
        return;
      }
      const keepRate = this.keep.get(level);
      if (keepRate !== undefined && Math.random() > keepRate) return;
      const text = JSON.stringify(fields);
      this.push({ text, level });
      this.schedule();
    } catch {
      // track() never throws - a serialization failure loses one line, never
      // the caller's request.
    }
  }

  private push(line: Line): void {
    this.lines.push(line);
    this.bytes += line.text.length + 1;
    while (this.bytes > MAX_BUFFER_BYTES && this.lines.length > 1) {
      const evicted = this.lines.shift()!;
      this.bytes -= evicted.text.length + 1;
      this.dropped++;
    }
  }

  private schedule(): void {
    const now = Date.now();
    if (this.inflight) return;
    if (now < this.backoffUntil) {
      if (!this.timer) {
        this.timer = setTimeout(() => {
          this.timer = null;
          void this.drain();
        }, this.backoffUntil - now);
        this.timer.unref?.();
      }
      return;
    }
    if (this.bytes >= FLUSH_AT_BYTES || this.pendingBatch) {
      void this.drain();
      return;
    }
    if (!this.timer) {
      this.timer = setTimeout(() => {
        this.timer = null;
        void this.drain();
      }, FLUSH_AFTER_MS);
      this.timer.unref?.();
    }
  }

  // flush sends everything currently buffered. Public API - resolves when the
  // buffer is empty or the current attempt failed (it never rejects).
  async flush(): Promise<void> {
    try {
      while (this.pendingBatch || this.lines.length > 0) {
        const before = this.pendingCount + this.lines.length;
        await this.drain();
        const after = this.pendingCount + this.lines.length;
        if (after >= before && after > 0) return; // no progress - backend down
      }
    } catch {
      // never throws
    }
  }

  private drain(): Promise<void> {
    if (this.inflight) return this.inflight;
    this.inflight = this.sendOnce().finally(() => {
      this.inflight = null;
      if (this.lines.length > 0 || this.pendingBatch) this.schedule();
    });
    return this.inflight;
  }

  private takeBatch(): string | null {
    if (this.pendingBatch) return this.pendingBatch; // retry byte-identical
    if (this.lines.length === 0) return null;
    const parts: string[] = [];
    let size = 0;
    if (this.dropped > 0) {
      parts.push(
        JSON.stringify({
          ts: new Date().toISOString(),
          level: 'warn',
          msg: 'upcontrol_buffer_dropped',
          dropped: this.dropped,
        }),
      );
      this.dropped = 0;
    }
    if (!this.sentInstallVerified) {
      // First batch of this process carries the chain proof (SPEC §8.1). If the
      // batch fails it is retried whole, so the event cannot be lost.
      parts.push(
        JSON.stringify({
          ts: new Date().toISOString(),
          level: 'info',
          msg: 'install_verified',
          version: SDK_VERSION,
          env: process.env.NODE_ENV || 'production',
        }),
      );
    }
    let count = 0;
    while (this.lines.length > 0 && size < MAX_BATCH_BYTES) {
      const line = this.lines.shift()!;
      this.bytes -= line.text.length + 1;
      parts.push(line.text);
      size += line.text.length + 1;
      count++;
    }
    this.pendingBatch = parts.join('\n');
    this.pendingCount = count;
    return this.pendingBatch;
  }

  private async sendOnce(): Promise<void> {
    const body = this.takeBatch();
    if (!body || !this.key) return;
    const ctrl = new AbortController();
    const kill = setTimeout(() => ctrl.abort(), REQUEST_TIMEOUT_MS);
    kill.unref?.();
    try {
      const res = await fetch(this.endpoint + '/i', {
        method: 'POST',
        headers: {
          'Content-Type': 'application/x-ndjson',
          'X-UpControl-Key': this.key,
          'X-UpControl-Sdk': 'js/' + SDK_VERSION,
        },
        body,
        signal: ctrl.signal,
      });
      if (res.status === 401) {
        // A bad key never fixes itself mid-process: drop the batch, warn once.
        this.pendingBatch = null;
        this.pendingCount = 0;
        if (!this.warned) {
          this.warned = true;
          process.stderr.write('upcontrol: the API key was rejected (401) - sending disabled\n');
        }
        this.key = undefined;
        return;
      }
      if (!res.ok) throw new Error('status ' + res.status);
      this.pendingBatch = null;
      this.pendingCount = 0;
      this.attempt = 0;
      this.backoffUntil = 0;
      this.sentInstallVerified = true;
      const receipt = (await res.json().catch(() => null)) as {
        sampling?: { level?: string; keep?: number };
      } | null;
      if (receipt?.sampling?.level && typeof receipt.sampling.keep === 'number') {
        this.keep.set(receipt.sampling.level, receipt.sampling.keep);
      }
    } catch {
      this.attempt++;
      const base = Math.min(MAX_BACKOFF_MS, 500 * 2 ** this.attempt);
      this.backoffUntil = Date.now() + base / 2 + Math.random() * (base / 2);
    } finally {
      clearTimeout(kill);
    }
  }
}

// scrubFields cleans every string field of an outgoing line in place.
export function scrubFields(fields: Record<string, unknown>): Record<string, unknown> {
  for (const k of Object.keys(fields)) {
    const v = fields[k];
    if (typeof v === 'string' && v.length > 0) {
      const r = scrub(v);
      if (r.counts && Object.keys(r.counts).length > 0) fields[k] = r.cleaned;
    }
  }
  return fields;
}
