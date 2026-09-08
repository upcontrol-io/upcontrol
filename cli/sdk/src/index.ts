// @upcontrol/sdk - the push library. Public surface: track(), flush(), the analytics feeds
// (funnel, experiment, retention, breakdown), and the logger bridges.
// Configuration is environment-only; track() never throws and without a key is a warned no-op.

import { Client, HOST, scrubFields, sendEvent, type Attrs } from './client.js';
import { createFunnel, type Funnel } from './funnel.js';
import {
  createBreakdown,
  createExperiment,
  createRetention,
  type Breakdown,
  type Experiment,
  type Retention,
} from './analytics.js';

export type { Attrs };
export type { Funnel, RequestLike } from './funnel.js';
export type { Breakdown, Experiment, Retention } from './analytics.js';
export { SDK_VERSION } from './client.js';

const client = new Client();
const service = client.service;

/** track sends one event or log line (a canonical or free name). Never throws, never blocks. */
export function track(event: string, attrs?: Attrs): void {
  sendEvent(client, event, attrs);
}

/** funnel declares a journey by name and steps, in order. Its `step()` sends one event
 *  named by the step: a request is fingerprinted here and never sent, a string id passes
 *  through as `uc.actor`. The server counts a person once per step. Never throws, never
 *  blocks. */
export function funnel(name: string, steps: string[]): Funnel {
  try {
    return createFunnel(name, steps, { client });
  } catch {
    return { step() {} };
  }
}

/** experiment declares an A/B test by name and variants, control first. `expose()` and
 *  `convert()` each send one event naming the test, the variant and the stat; the server
 *  counts a person once per variant per stat. Never throws, never blocks. */
export function experiment(name: string, variants: string[]): Experiment {
  try {
    return createExperiment(name, variants, { client });
  } catch {
    return { expose() {}, convert() {} };
  }
}

/** retention declares a retention feed by name. `seen(userId)` sends one event with the id
 *  as `uc.actor`; the server owns the cohort math. Never throws, never blocks. */
export function retention(name: string): Retention {
  try {
    return createRetention(name, { client });
  } catch {
    return { seen() {} };
  }
}

/** breakdown declares a dimension by name. `value(v)` counts events carrying the value;
 *  `value(v, who)` counts DISTINCT PEOPLE carrying it — the person rides as `uc.actor`, a
 *  request fingerprinted, and the server does the deduping. Never throws, never blocks. */
export function breakdown(name: string): Breakdown {
  try {
    return createBreakdown(name, { client });
  } catch {
    return { value() {} };
  }
}

/** flush sends everything buffered. Resolves (never rejects) when the buffer
 * is empty or the backend is unreachable. Call before a planned exit. */
export function flush(): Promise<void> {
  return client.flush();
}

/** upcontrolLine mirrors one log line from an existing logger: level plus msg/extra. */
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
    if (HOST) fields.host = HOST;
    if (service && fields.service === undefined) fields.service = service;
    client.enqueue(scrubFields(fields), String(fields.level));
  } catch {
    /* never throws */
  }
}

/** mirrorConsole tees console.log/warn/error into upcontrol; original output is untouched. */
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
    case 'trace':
      return 'trace';
    case 'debug':
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
