// @upcontrol/sdk - the push library (cli/SPEC.md §5). The public surface is
// minimal and frozen: track() and flush(), plus the two logger bridges the
// skill's recipes reference. Configuration is environment-only:
// UPCONTROL_API_KEY and UPCONTROL_ENDPOINT. track() never throws, never blocks,
// and without a key it is a warned no-op.

import { hostname } from 'node:os';
import { Client, scrubFields, type Attrs } from './client.js';

export type { Attrs };
export { SDK_VERSION } from './client.js';

const client = new Client();
const host = safeHostname();

function safeHostname(): string {
  try {
    return hostname();
  } catch {
    return '';
  }
}

/**
 * track sends one event or log line. `event` is either a canonical name from
 * the upcontrol dictionary (`npx upcontrol skills dictionary`) or any free
 * name, which lands as an ordinary log line. Never throws, never blocks.
 */
export function track(event: string, attrs?: Attrs): void {
  try {
    const fields: Record<string, unknown> = {
      ts: new Date().toISOString(),
      level: 'info',
      msg: String(event),
      ...attrs,
    };
    if (host) fields.host = host;
    client.enqueue(scrubFields(fields), 'info');
  } catch {
    /* track never throws */
  }
}

/** flush sends everything buffered. Resolves (never rejects) when the buffer
 * is empty or the backend is unreachable. Call before a planned exit. */
export function flush(): Promise<void> {
  return client.flush();
}

/**
 * upcontrolLine mirrors one log line from an existing logger. `level` is the
 * logger's level name; `msg` and `extra` are whatever the logger got - objects
 * are serialized, message strings pass through.
 */
export function upcontrolLine(level: string, msg: unknown, extra?: unknown): void {
  try {
    const fields: Record<string, unknown> = {
      ts: new Date().toISOString(),
      level: normalizeLevel(level),
    };
    if (typeof msg === 'string') {
      fields.msg = msg;
      if (extra !== undefined && typeof extra === 'object' && extra !== null) Object.assign(fields, flatten(extra));
    } else if (typeof msg === 'object' && msg !== null) {
      Object.assign(fields, flatten(msg));
      if (typeof extra === 'string') fields.msg = extra;
      if (fields.msg === undefined) fields.msg = safeJson(msg);
    } else {
      fields.msg = String(msg);
    }
    if (host) fields.host = host;
    client.enqueue(scrubFields(fields), String(fields.level));
  } catch {
    /* never throws */
  }
}

/**
 * mirrorConsole tees console.log/warn/error into upcontrol. The original
 * console output is untouched. Calling it twice is a no-op.
 */
export function mirrorConsole(): void {
  const c = console as Console & { __upcontrol?: boolean };
  if (c.__upcontrol) return;
  c.__upcontrol = true;
  for (const [method, level] of [
    ['log', 'info'],
    ['warn', 'warn'],
    ['error', 'error'],
  ] as const) {
    const original = console[method].bind(console);
    console[method] = (...args: unknown[]) => {
      try {
        upcontrolLine(
          level,
          args.map((a) => (typeof a === 'string' ? a : safeJson(a))).join(' '),
        );
      } catch {
        /* the mirror must never break console */
      }
      original(...args);
    };
  }
}

function normalizeLevel(level: unknown): string {
  const l = String(level ?? 'info').toLowerCase();
  switch (l) {
    case 'fatal':
    case 'error':
    case 'err':
      return 'error';
    case 'warn':
    case 'warning':
      return 'warn';
    case 'debug':
    case 'trace':
      return 'debug';
    default:
      return 'info';
  }
}

function flatten(obj: object): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const [k, v] of Object.entries(obj)) {
    if (v === null || v === undefined) continue;
    out[k] = typeof v === 'object' ? safeJson(v) : v;
  }
  return out;
}

function safeJson(v: unknown): string {
  try {
    return JSON.stringify(v) ?? String(v);
  } catch {
    return String(v);
  }
}

// Internal seam for the auto entry - not part of the public surface.
export function _internals(): { client: Client } {
  return { client };
}
