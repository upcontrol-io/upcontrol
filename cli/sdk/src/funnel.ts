// The funnel feed: who reached each step of a named journey, counted once per person on this
// server and reported every minute as counters that only grow. People never leave the
// process: a request is a salted hash of address + user-agent, an id is a salted hash too.
// The counting, the persistence and the timer live in counters.ts, shared with the other
// feeds; this file is the funnel's validation and its labels.

import { declare, warnOnce, type CounterDeps, type Counters, type Who } from './counters.js';

export type { RequestLike, Who } from './counters.js';
export type FunnelDeps = CounterDeps;

export interface Funnel {
  /** step counts `who` once at the step `name`. Never throws. */
  step(name: string, who: Who): void;
}

type Declared = Funnel & { report(): void; stop(): Promise<void> };

const NOOP: Declared = { step() {}, report() {}, stop: () => Promise.resolve() };

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

// The wrapper is cached with its declaration, so a second createFunnel of the same name
// returns the same object (the first declaration wins, whatever the second says).
const wrappers = new WeakMap<Counters, Declared>();

export function createFunnel(name: string, steps: string[], deps: FunnelDeps): Declared {
  const label = typeof name === 'string' ? name.trim() : String(name);
  const problem = problemWith(name, steps);
  if (problem !== null) {
    warnOnce(`ignored:${label}:${problem}`, `upcontrol: funnel "${label}" is ignored: ${problem}`);
    return NOOP;
  }
  const names = steps.map((s) => s.trim());

  const counters = declare(
    'funnel',
    label,
    deps,
    (step, value, ts) => {
      // Level 'metric' on purpose: the client applies server-sent keep rates by level and the
      // server names only log levels, so a reading is never sampled away.
      // Zero-padded: the server keeps labels as strings, and a reader sorting twelve steps
      // by `i` must not put 10 before 2.
      deps.client.enqueue(
        { ts, metric: 'funnel', value, labels: { funnel: label, step, i: String(names.indexOf(step) + 1).padStart(2, '0') } },
        'metric',
      );
    },
    names,
  );
  if (!counters) return NOOP;
  const on = counters;
  const cached = wrappers.get(counters);
  if (cached) return cached;

  function step(stepName: string, who: Who): void {
    try {
      const s = typeof stepName === 'string' ? stepName.trim() : '';
      if (!names.includes(s)) {
        warnOnce(`nostep:${label}:${String(stepName)}`, `upcontrol: funnel "${label}" has no step "${String(stepName)}"`);
        return;
      }
      // A null who is the breakdown's anonymous event, not a person; the funnel has no such
      // thing, so it is not counted (nor is anything that is not a string or a request).
      if (who === null) return;
      on.bump(s, who);
    } catch {
      /* step never throws */
    }
  }

  const funnel: Declared = { step, report: on.report, stop: on.stop };
  wrappers.set(counters, funnel);
  return funnel;
}
