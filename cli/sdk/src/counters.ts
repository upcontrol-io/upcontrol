// The counter machinery every feed runs on: named buckets of people, each person counted
// once, persisted to one state file and reported every minute as counters that only grow.
// The feeds (funnel, experiment, retention, breakdown) own their labels and their validation;
// this file owns the counting, the persistence and the timer.

import { createHash, randomBytes } from 'node:crypto';
import { mkdirSync, readFileSync } from 'node:fs';
import { rename, writeFile } from 'node:fs/promises';
import { dirname, join } from 'node:path';

// ponytail: one id per person per bucket, about 80 MB of memory per million, and a twelve-step
// funnel at the ceiling is twelve times that. Past it the oldest visitor is forgotten and
// counts again. Upgrade path when a customer gets there: a sketch (HyperLogLog) or dedup on
// the server.
const MAX_IDS = 1_000_000;
const EVERY_MS = 60_000;
const SLOW_SAVE_BYTES = 4 * 1024 * 1024;
const SLOW_SAVE_MS = 10 * EVERY_MS;

type HeaderBag = { get(name: string): string | null } | Record<string, string | string[] | undefined>;

/** The shape every Node request has: `http.IncomingMessage`, Express's `req` (`ip`), a
 *  fetch `Request` (`headers.get`). Only the address and the user-agent are read. */
export interface RequestLike {
  headers: HeaderBag;
  socket?: { remoteAddress?: string };
  ip?: string;
}

export type Who = string | RequestLike;

export interface CounterDeps {
  client: { enqueue(fields: Record<string, unknown>, level: string): void };
  env: NodeJS.ProcessEnv;
  everyMs?: number;
  maxIds?: number;
}

/** One declared thing that counts people into named buckets and reports them every minute. */
export interface Counters {
  /** Count `who` once in `bucket`. A `who` of null counts the event, not a person (no dedup). */
  bump(bucket: string, who: Who | null): void;
  /** The bucket's running total, for callers that derive one bucket from another. */
  seen(bucket: string): number;
  /** Buckets in insertion order, for the report. */
  entries(): [string, number][];
  /** Drop a bucket entirely (a retention cohort that has aged out). */
  drop(bucket: string): void;
  /** The cohort already remembered for this person, or `cohort` remembered now. The id is
   *  hashed in here with the same salt bump() dedups by, so a raw id never reaches the state. */
  rememberFirst(who: string, cohort: string): string;
  /** Forget every id remembered in `cohort` (its buckets are being dropped). */
  forgetCohort(cohort: string): void;
  report(): void;
  stop(): Promise<void>;
}

interface BucketState {
  seen: number;
  ids: Set<string>;
}

interface DeclState {
  buckets: Map<string, BucketState>;
  /** Per-declaration id hash to cohort, retention's first-seen map; empty for the other feeds. */
  first: Map<string, string>;
}

interface State {
  file: string;
  salt: string;
  counters: Map<string, DeclState>;
  dirty: boolean;
  /** The save in flight, so a tick never starts a second one and stop() can wait for it. */
  pending: Promise<void> | null;
  lastSavedAt: number;
  lastBytes: number;
}

const warned = new Set<string>();
const states = new Map<string, State>();
const declarations = new Map<string, Counters>();

export function warnOnce(key: string, text: string): void {
  if (warned.has(key)) return;
  warned.add(key);
  process.stderr.write(text + '\n');
}

function stateFile(deps: CounterDeps): string {
  const dir = deps.env.UPCONTROL_STATE_DIR?.trim() || join(process.cwd(), 'node_modules', '.cache', 'upcontrol');
  return join(dir, 'funnels.json');
}

function parseBuckets(raw: unknown): Map<string, BucketState> | null {
  if (typeof raw !== 'object' || raw === null) return null;
  const buckets = new Map<string, BucketState>();
  for (const [bucket, rawBucket] of Object.entries(raw as Record<string, unknown>)) {
    const s = rawBucket as { seen?: unknown; ids?: unknown };
    if (typeof s?.seen !== 'number' || !Array.isArray(s.ids)) return null;
    const ids = s.ids.filter((id): id is string => typeof id === 'string');
    buckets.set(bucket, { seen: s.seen, ids: new Set(ids) });
  }
  return buckets;
}

function parseState(raw: string): Pick<State, 'salt' | 'counters'> | null {
  const doc: unknown = JSON.parse(raw);
  if (typeof doc !== 'object' || doc === null) return null;
  const d = doc as { v?: unknown; salt?: unknown; funnels?: unknown; counters?: unknown };
  if (typeof d.salt !== 'string' || d.salt === '') return null;
  const counters = new Map<string, DeclState>();
  // v1 is what the funnel alone wrote through 0.3.0; its counts load under the funnel kind,
  // so an upgrade keeps its people and never double-counts them.
  if (d.v === 1) {
    if (typeof d.funnels !== 'object' || d.funnels === null) return null;
    for (const [name, rawSteps] of Object.entries(d.funnels as Record<string, unknown>)) {
      const buckets = parseBuckets(rawSteps);
      if (!buckets) return null;
      counters.set(`funnel\n${name}`, { buckets, first: new Map() });
    }
  } else if (d.v === 2) {
    if (typeof d.counters !== 'object' || d.counters === null) return null;
    for (const [key, rawDecl] of Object.entries(d.counters as Record<string, unknown>)) {
      if (typeof rawDecl !== 'object' || rawDecl === null) return null;
      // The first-seen map rides under a key no bucket can have: every feed's bucket keys
      // either embed a newline as a separator or (breakdown alone) are caller values, which
      // are trimmed and can never end in one.
      const { 'first\n': rawFirst, ...rawBuckets } = rawDecl as Record<string, unknown>;
      const buckets = parseBuckets(rawBuckets);
      if (!buckets) return null;
      const first = new Map<string, string>();
      if (rawFirst !== undefined) {
        if (typeof rawFirst !== 'object' || rawFirst === null) return null;
        for (const [id, cohort] of Object.entries(rawFirst as Record<string, unknown>)) {
          if (typeof cohort !== 'string') return null;
          first.set(id, cohort);
        }
      }
      counters.set(key, { buckets, first });
    }
  } else {
    return null;
  }
  return { salt: d.salt, counters };
}

function loadState(file: string): State {
  const cached = states.get(file);
  if (cached) return cached;
  const state: State = { file, salt: '', counters: new Map(), dirty: false, pending: null, lastSavedAt: 0, lastBytes: 0 };
  let raw: string | null = null;
  try {
    raw = readFileSync(file, 'utf8');
  } catch {
    // No file yet: the counts start here.
  }
  if (raw !== null) {
    let parsed: Pick<State, 'salt' | 'counters'> | null = null;
    try {
      parsed = parseState(raw);
    } catch {
      parsed = null;
    }
    if (parsed) {
      state.salt = parsed.salt;
      state.counters = parsed.counters;
      state.lastBytes = raw.length;
    } else {
      warnOnce('bad:' + file, `upcontrol: ${file} is not a funnel state file; starting the counts again`);
    }
  }
  if (!state.salt) state.salt = randomBytes(16).toString('hex');
  states.set(file, state);
  return state;
}

async function writeState(state: State, at: number): Promise<void> {
  try {
    const counters: Record<string, Record<string, unknown>> = {};
    for (const [key, decl] of state.counters) {
      const out: Record<string, unknown> = {};
      for (const [bucket, s] of decl.buckets) out[bucket] = { seen: s.seen, ids: [...s.ids] };
      if (decl.first.size) out['first\n'] = Object.fromEntries(decl.first);
      counters[key] = out;
    }
    // ponytail: the whole id set is serialised on every save, synchronously, about 100 ms per
    // million ids; the write itself is off the event loop. Upgrade path: an append-only log,
    // or dedup on the server so the ids never reach the disk at all.
    const json = JSON.stringify({ v: 2, salt: state.salt, counters });
    // The pid in the temp name: two processes sharing a state dir must never rename each
    // other's half-written file into place. Each still counts its own people (the README says).
    const tmp = `${state.file}.${process.pid}.tmp`;
    mkdirSync(dirname(state.file), { recursive: true });
    await writeFile(tmp, json);
    await rename(tmp, state.file);
    state.lastBytes = json.length;
    state.lastSavedAt = at;
  } catch (err) {
    state.dirty = true;
    const why = (err as { code?: string })?.code || (err as Error)?.message || 'unknown';
    warnOnce(
      'save:' + state.file,
      `upcontrol: cannot write ${state.file} (${why}): the counts restart with the process`,
    );
  }
}

function save(state: State, at: number): Promise<void> {
  if (state.pending) return state.pending;
  // Cleared before the write: a bump that arrives while the file is being written marks the
  // state dirty again, and the next tick saves it.
  state.dirty = false;
  state.pending = writeState(state, at).finally(() => {
    state.pending = null;
  });
  return state.pending;
}

function maybeSave(state: State, at: number): void {
  if (!state.dirty) return;
  // A million ids is ~30 MB of JSON per bucket: past 4 MiB the file is written every ten
  // minutes instead of every one.
  if (state.lastBytes >= SLOW_SAVE_BYTES && at - state.lastSavedAt < SLOW_SAVE_MS) return;
  void save(state, at);
}

function header(bag: HeaderBag, name: string): string {
  const get = (bag as { get?: unknown }).get;
  const value =
    typeof get === 'function'
      ? (bag as { get(n: string): string | null }).get(name)
      : (bag as Record<string, string | string[] | undefined>)[name];
  const one = Array.isArray(value) ? value[0] : value;
  return typeof one === 'string' ? one.trim() : '';
}

export function declare(
  kind: string,
  name: string,
  deps: CounterDeps,
  emit: (bucket: string, value: number, ts: string) => void,
  seed?: string[],
): Counters | null {
  if (typeof kind !== 'string' || kind === '' || typeof name !== 'string' || name.trim() === '') return null;
  const label = name.trim();
  const file = stateFile(deps);
  const key = `${file}\n${kind}\n${label}`;
  // Declared twice in one process (a module evaluated again): the first declaration is the
  // one, and there is one timer.
  const already = declarations.get(key);
  if (already) return already;

  const state = loadState(file);
  const stored = state.counters.get(`${kind}\n${label}`);
  const buckets = new Map<string, BucketState>();
  if (seed) {
    // A seeded declaration owns its buckets: a name it no longer lists is gone, so the next
    // save drops it. An unseeded one (retention, breakdown) adopts whatever was stored.
    for (const b of seed) buckets.set(b, stored?.buckets.get(b) ?? { seen: 0, ids: new Set<string>() });
  } else if (stored) {
    for (const [b, s] of stored.buckets) buckets.set(b, s);
  }
  const decl: DeclState = { buckets, first: stored?.first ?? new Map() };
  state.counters.set(`${kind}\n${label}`, decl);

  const everyMs = deps.everyMs ?? EVERY_MS;
  const maxIds = deps.maxIds ?? MAX_IDS;

  const hash = (hkind: string, value: string): string =>
    createHash('sha256')
      .update(state.salt + '\n' + hkind + '\n' + value)
      .digest('hex')
      .slice(0, 16);

  function fingerprint(who: RequestLike): string | null {
    // The framework's own address first (Express, Fastify and Koa apply the app's trust-proxy
    // policy to it), then the proxy header a bare Node request behind a proxy has nothing
    // else for, then the socket. A forged x-forwarded-for miscounts a feed and reaches
    // nothing else.
    const ip =
      (typeof who.ip === 'string' ? who.ip.trim() : '') ||
      header(who.headers, 'x-forwarded-for').split(',')[0].trim() ||
      who.socket?.remoteAddress?.trim() ||
      '';
    if (ip === '') {
      // Without an address every visitor on one browser would be one person, and that number
      // is the denominator of every share the feeds draw.
      warnOnce(
        `noaddress:${key}`,
        `upcontrol: ${kind} "${label}" got a request with no address; it is not counted (pass the framework's request, or set the proxy's x-forwarded-for)`,
      );
      return null;
    }
    const ua = header(who.headers, 'user-agent');
    if (/bot|crawl|spider|slurp|headless/i.test(ua)) return null;
    return hash('r', ip + '\n' + ua);
  }

  function bump(bucket: string, who: Who | null): void {
    try {
      let s = buckets.get(bucket);
      if (!s) buckets.set(bucket, (s = { seen: 0, ids: new Set<string>() }));
      if (who !== null) {
        let id: string | null;
        if (typeof who === 'string') {
          const value = who.trim();
          if (value === '') return;
          id = hash('u', value);
        } else if (typeof who === 'object' && who.headers) {
          id = fingerprint(who);
        } else {
          return;
        }
        if (id === null || s.ids.has(id)) return;
        s.ids.add(id);
        if (s.ids.size > maxIds) {
          const oldest = s.ids.values().next().value;
          if (oldest !== undefined) s.ids.delete(oldest);
          warnOnce(
            `cap:${key}:${bucket}`,
            `upcontrol: ${kind} "${label}" bucket "${bucket}" keeps the last ${maxIds} people; an older visitor who returns counts again`,
          );
        }
      }
      s.seen += 1;
      state.dirty = true;
    } catch {
      /* bump never throws */
    }
  }

  function report(): void {
    try {
      const ts = new Date().toISOString();
      for (const [bucket, s] of buckets) emit(bucket, s.seen, ts);
      maybeSave(state, Date.now());
    } catch {
      /* reporting never throws */
    }
  }

  const timer = setInterval(report, everyMs);
  timer.unref?.();

  function stop(): Promise<void> {
    clearInterval(timer);
    // A save already in flight finishes first, then whatever arrived during it.
    return (state.pending ?? Promise.resolve()).then(() => (state.dirty ? save(state, Date.now()) : undefined));
  }

  const counters: Counters = {
    bump,
    seen: (bucket) => buckets.get(bucket)?.seen ?? 0,
    entries: () => [...buckets].map(([bucket, s]) => [bucket, s.seen] as [string, number]),
    drop(bucket) {
      if (buckets.delete(bucket)) state.dirty = true;
    },
    rememberFirst(who, cohort) {
      const id = hash('u', who.trim());
      const got = decl.first.get(id);
      if (got !== undefined) return got;
      decl.first.set(id, cohort);
      state.dirty = true;
      return cohort;
    },
    forgetCohort(cohort) {
      for (const [id, c] of decl.first) if (c === cohort) decl.first.delete(id);
      state.dirty = true;
    },
    report,
    stop,
  };
  declarations.set(key, counters);
  report();
  return counters;
}
