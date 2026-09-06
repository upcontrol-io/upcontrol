// Three feeds on the counter machinery: an A/B test, a weekly retention cohort and a
// dimension breakdown. The funnel's laws bind all three: never throw, never block, a bad
// declaration warns once and returns a no-op, and nothing reaches the wire but counts.

import { declare, warnOnce, type CounterDeps, type Counters, type Who } from './counters.js';

export interface Experiment {
  /** expose counts `who` once in the variant they landed in. Never throws. */
  expose(variant: string, who: Who): void;
  /** convert counts `who` once as having reached the goal in that variant. Never throws. */
  convert(variant: string, who: Who): void;
}

export interface Retention {
  /** seen counts a signed-in user into the Monday-week cohort they were first seen in. */
  seen(userId: string): void;
}

export interface Breakdown {
  /** value counts one event carrying the value. There is no `who`: events, not people. */
  value(v: string): void;
}

type Declared<T> = T & { report(): void; stop(): Promise<void> };

const NOOP_EXPERIMENT: Declared<Experiment> = { expose() {}, convert() {}, report() {}, stop: () => Promise.resolve() };
const NOOP_RETENTION: Declared<Retention> = { seen() {}, report() {}, stop: () => Promise.resolve() };
const NOOP_BREAKDOWN: Declared<Breakdown> = { value() {}, report() {}, stop: () => Promise.resolve() };

const DAY_MS = 86_400_000;
const MAX_COHORTS = 12;
const MAX_VALUES = 200;

function nameProblem(name: string): string | null {
  if (typeof name !== 'string' || name.trim() === '') return 'the name is empty';
  if (name.trim().length > 60) return 'the name is over 60 characters';
  return null;
}

function experimentProblem(name: string, variants: string[]): string | null {
  const np = nameProblem(name);
  if (np !== null) return np;
  if (!Array.isArray(variants) || variants.length < 2 || variants.length > 8 || variants.some((v) => typeof v !== 'string'))
    return 'it needs 2 to 8 variants';
  const seen = new Set<string>();
  for (const raw of variants) {
    const v = raw.trim();
    if (v === '' || v.length > 40) return `variant "${v}" is empty or over 40 characters`;
    if (seen.has(v)) return `variant "${v}" repeats`;
    seen.add(v);
  }
  return null;
}

export function createExperiment(name: string, variants: string[], deps: CounterDeps): Declared<Experiment> {
  const label = typeof name === 'string' ? name.trim() : String(name);
  const problem = experimentProblem(name, variants);
  if (problem !== null) {
    warnOnce(`ignored:${label}:${problem}`, `upcontrol: A/B test "${label}" is ignored: ${problem}`);
    return NOOP_EXPERIMENT;
  }
  const list = variants.map((v) => v.trim());

  const counters = declare('experiment', label, deps, (bucket, value, ts) => {
    const [variant, stat] = bucket.split('\n');
    deps.client.enqueue(
      {
        ts,
        metric: 'experiment',
        value,
        // Zero-padded and control first: the board orders the card's rows by `i`, which is
        // the variant's 1-based declaration index.
        labels: { experiment: label, variant, i: String(list.indexOf(variant) + 1).padStart(2, '0'), stat },
      },
      'metric',
    );
  },
    // A declared arm reports both its counts from the first minute, at 0 until someone
    // lands in it. Without the seed an arm nobody reached is absent from the readings, so
    // the catalog never lists it and the card silently drops a variant the test is running.
    list.flatMap((variant) => [`${variant}\nexposed`, `${variant}\nconverted`]),
  );
  if (!counters) return NOOP_EXPERIMENT;
  const on = counters;

  function count(stat: 'exposed' | 'converted', variant: string, who: Who): void {
    try {
      const v = typeof variant === 'string' ? variant.trim() : '';
      if (!list.includes(v)) {
        warnOnce(`novariant:${label}:${String(variant)}`, `upcontrol: A/B test "${label}" has no variant "${String(variant)}"`);
        return;
      }
      // A null who is the breakdown's anonymous event, not a person; a test counts people.
      if (who === null) return;
      on.bump(`${v}\n${stat}`, who);
    } catch {
      /* never throws */
    }
  }

  return {
    expose: (variant, who) => count('exposed', variant, who),
    convert: (variant, who) => count('converted', variant, who),
    report: on.report,
    stop: on.stop,
  };
}

/** The ISO date (YYYY-MM-DD) of the Monday of the week `date` falls in, in UTC so the cohort
 *  does not move with the server's timezone. */
function mondayOf(date: Date): string {
  const midnight = Date.UTC(date.getUTCFullYear(), date.getUTCMonth(), date.getUTCDate());
  return new Date(midnight - ((date.getUTCDay() + 6) % 7) * DAY_MS).toISOString().slice(0, 10);
}

export function createRetention(name: string, deps: CounterDeps): Declared<Retention> {
  const label = typeof name === 'string' ? name.trim() : String(name);
  const problem = nameProblem(name);
  if (problem !== null) {
    warnOnce(`ignored:${label}:${problem}`, `upcontrol: retention "${label}" is ignored: ${problem}`);
    return NOOP_RETENTION;
  }

  const counters = declare('retention', label, deps, (bucket, value, ts) => {
    const [cohort, week] = bucket.split('\n');
    deps.client.enqueue(
      { ts, metric: 'retention', value, labels: { retention: label, cohort, week } },
      'metric',
    );
  });
  if (!counters) return NOOP_RETENTION;
  const on = counters;

  function trimCohorts(): void {
    const cohorts = new Set<string>();
    for (const [bucket] of on.entries()) cohorts.add(bucket.split('\n')[0]);
    while (cohorts.size > MAX_COHORTS) {
      // ISO Mondays sort lexicographically, so the first is the oldest.
      const oldest = [...cohorts].sort()[0];
      for (const [bucket] of on.entries()) {
        if (bucket.split('\n')[0] === oldest) on.drop(bucket);
      }
      on.forgetCohort(oldest);
      cohorts.delete(oldest);
      warnOnce(
        `cohorts:${label}`,
        `upcontrol: retention "${label}" keeps the last ${MAX_COHORTS} weekly cohorts; the oldest one stopped being counted`,
      );
    }
  }

  function seen(userId: string): void {
    try {
      if (typeof userId !== 'string') return;
      const id = userId.trim();
      if (id === '') return;
      const now = mondayOf(new Date());
      const cohort = on.rememberFirst(id, now);
      const week = Math.floor((Date.parse(now) - Date.parse(cohort)) / (7 * DAY_MS));
      on.bump(`${cohort}\n${String(week).padStart(2, '0')}`, id);
      trimCohorts();
    } catch {
      /* never throws */
    }
  }

  return { seen, report: on.report, stop: on.stop };
}

export function createBreakdown(name: string, deps: CounterDeps): Declared<Breakdown> {
  const label = typeof name === 'string' ? name.trim() : String(name);
  const problem = nameProblem(name);
  if (problem !== null) {
    warnOnce(`ignored:${label}:${problem}`, `upcontrol: breakdown "${label}" is ignored: ${problem}`);
    return NOOP_BREAKDOWN;
  }

  const counters = declare('breakdown', label, deps, (value, count, ts) => {
    deps.client.enqueue(
      { ts, metric: 'breakdown', value: count, labels: { dimension: label, value } },
      'metric',
    );
  });
  if (!counters) return NOOP_BREAKDOWN;
  const on = counters;

  function feed(v: string): void {
    try {
      if (typeof v !== 'string') return;
      const value = v.trim();
      if (value === '' || value.length > 80) return;
      // A bucket always has seen >= 1, so a zero reading means the value is new. Past the
      // cap a new value is ignored: a dimension fed a request id would grow without limit.
      if (on.seen(value) === 0 && on.entries().length >= MAX_VALUES) {
        warnOnce(
          `cap:${label}`,
          `upcontrol: breakdown "${label}" keeps its first ${MAX_VALUES} values; a new one is ignored`,
        );
        return;
      }
      on.bump(value, null);
    } catch {
      /* never throws */
    }
  }

  return { value: feed, report: on.report, stop: on.stop };
}
