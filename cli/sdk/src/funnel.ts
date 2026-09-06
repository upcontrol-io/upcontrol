// The funnel feed: who reached each step of a named journey, counted once per person on this
// server and reported every minute as counters that only grow. People never leave the
// process: a request is a salted hash of address + user-agent, an id is a salted hash too.

import { createHash, randomBytes } from 'node:crypto';
import { mkdirSync, readFileSync } from 'node:fs';
import { rename, writeFile } from 'node:fs/promises';
import { dirname, join } from 'node:path';

// ponytail: one id per person per step, about 80 MB of memory per million, and a twelve-step
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

export interface Funnel {
  /** step counts `who` once at the step `name`. Never throws. */
  step(name: string, who: Who): void;
}

export interface FunnelDeps {
  client: { enqueue(fields: Record<string, unknown>, level: string): void };
  env: NodeJS.ProcessEnv;
  now?: () => number;
  everyMs?: number;
  maxIds?: number;
}

type Declared = Funnel & { report(): void; stop(): Promise<void> };

interface StepState {
  seen: number;
  ids: Set<string>;
}

interface State {
  file: string;
  salt: string;
  funnels: Map<string, Map<string, StepState>>;
  dirty: boolean;
  /** The save in flight, so a tick never starts a second one and stop() can wait for it. */
  pending: Promise<void> | null;
  lastSavedAt: number;
  lastBytes: number;
}

const warned = new Set<string>();
const states = new Map<string, State>();
const declarations = new Map<string, { steps: string[]; funnel: Declared }>();

const NOOP: Declared = { step() {}, report() {}, stop: () => Promise.resolve() };

function warnOnce(key: string, text: string): void {
  if (warned.has(key)) return;
  warned.add(key);
  process.stderr.write(text + '\n');
}

function stateFile(deps: FunnelDeps): string {
  const dir = deps.env.UPCONTROL_STATE_DIR?.trim() || join(process.cwd(), 'node_modules', '.cache', 'upcontrol');
  return join(dir, 'funnels.json');
}

function parseState(raw: string): Pick<State, 'salt' | 'funnels'> | null {
  const doc: unknown = JSON.parse(raw);
  if (typeof doc !== 'object' || doc === null) return null;
  const d = doc as { v?: unknown; salt?: unknown; funnels?: unknown };
  if (d.v !== 1 || typeof d.salt !== 'string' || d.salt === '') return null;
  if (typeof d.funnels !== 'object' || d.funnels === null) return null;
  const funnels = new Map<string, Map<string, StepState>>();
  for (const [name, rawSteps] of Object.entries(d.funnels as Record<string, unknown>)) {
    if (typeof rawSteps !== 'object' || rawSteps === null) return null;
    const steps = new Map<string, StepState>();
    for (const [step, rawStep] of Object.entries(rawSteps as Record<string, unknown>)) {
      const s = rawStep as { seen?: unknown; ids?: unknown };
      if (typeof s?.seen !== 'number' || !Array.isArray(s.ids)) return null;
      const ids = s.ids.filter((id): id is string => typeof id === 'string');
      steps.set(step, { seen: s.seen, ids: new Set(ids) });
    }
    funnels.set(name, steps);
  }
  return { salt: d.salt, funnels };
}

function loadState(file: string): State {
  const cached = states.get(file);
  if (cached) return cached;
  const state: State = { file, salt: '', funnels: new Map(), dirty: false, pending: null, lastSavedAt: 0, lastBytes: 0 };
  let raw: string | null = null;
  try {
    raw = readFileSync(file, 'utf8');
  } catch {
    // No file yet: the counts start here.
  }
  if (raw !== null) {
    let parsed: Pick<State, 'salt' | 'funnels'> | null = null;
    try {
      parsed = parseState(raw);
    } catch {
      parsed = null;
    }
    if (parsed) {
      state.salt = parsed.salt;
      state.funnels = parsed.funnels;
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
    const funnels: Record<string, Record<string, { seen: number; ids: string[] }>> = {};
    for (const [name, steps] of state.funnels) {
      const out: Record<string, { seen: number; ids: string[] }> = {};
      for (const [step, s] of steps) out[step] = { seen: s.seen, ids: [...s.ids] };
      funnels[name] = out;
    }
    // ponytail: the whole id set is serialised on every save, synchronously, about 100 ms per
    // million ids; the write itself is off the event loop. Upgrade path: an append-only log,
    // or dedup on the server so the ids never reach the disk at all.
    const json = JSON.stringify({ v: 1, salt: state.salt, funnels });
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
      `upcontrol: cannot write ${state.file} (${why}): funnel counts restart with the process`,
    );
  }
}

function save(state: State, at: number): Promise<void> {
  if (state.pending) return state.pending;
  // Cleared before the write: a step that arrives while the file is being written marks the
  // state dirty again, and the next tick saves it.
  state.dirty = false;
  state.pending = writeState(state, at).finally(() => {
    state.pending = null;
  });
  return state.pending;
}

function maybeSave(state: State, at: number): void {
  if (!state.dirty) return;
  // A million ids is ~30 MB of JSON per step: past 4 MiB the file is written every ten
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

function problemWith(name: string, steps: string[]): string | null {
  if (typeof name !== 'string' || name.trim() === '') return 'the name is empty';
  if (name.trim().length > 60) return 'the name is over 60 characters';
  if (!Array.isArray(steps) || steps.length < 2 || steps.length > 12 || steps.some((s) => typeof s !== 'string'))
    return 'it needs 2 to 12 steps';
  const seen = new Set<string>();
  for (const raw of steps) {
    const s = raw.trim();
    if (s === '' || s.length > 40) return `step "${s}" is empty or over 40 characters`;
    if (seen.has(s)) return `step "${s}" repeats`;
    seen.add(s);
  }
  return null;
}

export function createFunnel(name: string, steps: string[], deps: FunnelDeps): Declared {
  const label = typeof name === 'string' ? name.trim() : String(name);
  const problem = problemWith(name, steps);
  if (problem !== null) {
    warnOnce(`ignored:${label}:${problem}`, `upcontrol: funnel "${label}" is ignored: ${problem}`);
    return NOOP;
  }
  const names = steps.map((s) => s.trim());

  const file = stateFile(deps);
  const key = `${file}\n${label}`;
  const already = declarations.get(key);
  if (already) {
    if (already.steps.join('\n') !== names.join('\n')) {
      warnOnce(
        'redeclared:' + key,
        `upcontrol: funnel "${label}" is already declared with different steps; the first declaration wins`,
      );
    }
    return already.funnel;
  }

  const state = loadState(file);
  const stored = state.funnels.get(label);
  const stepStates = new Map<string, StepState>();
  for (const s of names) stepStates.set(s, stored?.get(s) ?? { seen: 0, ids: new Set<string>() });
  // Steps the declaration no longer has are gone from here, so the next save drops them.
  state.funnels.set(label, stepStates);

  const now = deps.now ?? Date.now;
  const everyMs = deps.everyMs ?? EVERY_MS;
  const maxIds = deps.maxIds ?? MAX_IDS;

  const hash = (kind: string, value: string): string =>
    createHash('sha256')
      .update(state.salt + '\n' + kind + '\n' + value)
      .digest('hex')
      .slice(0, 16);

  function fingerprint(who: RequestLike): string | null {
    // The framework's own address first (Express, Fastify and Koa apply the app's trust-proxy
    // policy to it), then the proxy header a bare Node request behind a proxy has nothing
    // else for, then the socket. A forged x-forwarded-for miscounts this funnel and reaches
    // nothing else.
    const ip =
      (typeof who.ip === 'string' ? who.ip.trim() : '') ||
      header(who.headers, 'x-forwarded-for').split(',')[0].trim() ||
      who.socket?.remoteAddress?.trim() ||
      '';
    if (ip === '') {
      // Without an address every visitor on one browser would be one person, and that number
      // is the denominator of every share the funnel draws.
      warnOnce(
        `noaddress:${key}`,
        `upcontrol: funnel "${label}" got a request with no address; it is not counted (pass the framework's request, or set the proxy's x-forwarded-for)`,
      );
      return null;
    }
    const ua = header(who.headers, 'user-agent');
    if (/bot|crawl|spider|slurp|headless/i.test(ua)) return null;
    return hash('r', ip + '\n' + ua);
  }

  function step(stepName: string, who: Who): void {
    try {
      const s = stepStates.get(typeof stepName === 'string' ? stepName.trim() : '');
      if (!s) {
        warnOnce(`nostep:${key}:${String(stepName)}`, `upcontrol: funnel "${label}" has no step "${String(stepName)}"`);
        return;
      }
      let id: string | null;
      if (typeof who === 'string') {
        const value = who.trim();
        if (value === '') return;
        id = hash('u', value);
      } else if (typeof who === 'object' && who !== null && (who as RequestLike).headers) {
        id = fingerprint(who as RequestLike);
      } else {
        return;
      }
      if (id === null || s.ids.has(id)) return;
      s.ids.add(id);
      s.seen += 1;
      state.dirty = true;
      if (s.ids.size > maxIds) {
        const oldest = s.ids.values().next().value;
        if (oldest !== undefined) s.ids.delete(oldest);
        warnOnce(
          `cap:${key}:${stepName}`,
          `upcontrol: funnel "${label}" step "${stepName}" keeps the last ${maxIds} people; an older visitor who returns counts again`,
        );
      }
    } catch {
      /* step never throws */
    }
  }

  function report(): void {
    try {
      const ts = new Date(now()).toISOString();
      let index = 0;
      for (const [stepName, s] of stepStates) {
        index++;
        // Level 'metric' on purpose: the client applies server-sent keep rates by level and the
        // server names only log levels, so a reading is never sampled away.
        // Zero-padded: the server keeps labels as strings, and a reader sorting twelve steps
        // by `i` must not put 10 before 2.
        deps.client.enqueue(
          { ts, metric: 'funnel', value: s.seen, labels: { funnel: label, step: stepName, i: String(index).padStart(2, '0') } },
          'metric',
        );
      }
      maybeSave(state, now());
    } catch {
      /* reporting never throws */
    }
  }

  const timer = setInterval(report, everyMs);
  timer.unref?.();

  function stop(): Promise<void> {
    clearInterval(timer);
    // A save already in flight finishes first, then whatever arrived during it.
    return (state.pending ?? Promise.resolve()).then(() => (state.dirty ? save(state, now()) : undefined));
  }

  const funnel: Declared = { step, report, stop };
  declarations.set(key, { steps: names, funnel });
  report();
  return funnel;
}
