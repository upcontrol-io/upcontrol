// Three feeds on the identity machinery: an A/B test, a retention feed and a dimension
// breakdown. Each call is one track()-shaped event and the server counts DISTINCT people
// from them. The funnel's laws bind all three: never throw, never block, a bad declaration
// warns once and returns a no-op, and nothing raw about a person reaches the wire.

import { sendEvent } from './client.js';
import { actor, warnOnce, type FeedDeps, type Who } from './counters.js';

export interface Experiment {
  /** expose records `who` in the variant they landed in. Never throws. */
  expose(variant: string, who: Who): void;
  /** convert records `who` as having reached the goal in that variant. Never throws. */
  convert(variant: string, who: Who): void;
}

export interface Retention {
  /** seen records a signed-in user; the server owns the cohort they belong to. */
  seen(userId: string): void;
}

export interface Breakdown {
  /** value records one event carrying the value; with `who` it records the person carrying
   *  it — distinct people, not events, once the server folds them. */
  value(v: string, who?: Who | null): void;
}

const NOOP_EXPERIMENT: Experiment = { expose() {}, convert() {} };
const NOOP_RETENTION: Retention = { seen() {} };
const NOOP_BREAKDOWN: Breakdown = { value() {} };

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

export function createExperiment(name: string, variants: string[], deps: FeedDeps): Experiment {
  const label = typeof name === 'string' ? name.trim() : String(name);
  const problem = experimentProblem(name, variants);
  if (problem !== null) {
    warnOnce(`ignored:${label}:${problem}`, `upcontrol: A/B test "${label}" is ignored: ${problem}`);
    return NOOP_EXPERIMENT;
  }
  const list = new Set(variants.map((v) => v.trim()));

  function record(stat: 'exposed' | 'converted', variant: string, who: Who): void {
    try {
      const v = typeof variant === 'string' ? variant.trim() : '';
      if (!list.has(v)) {
        warnOnce(`novariant:${label}:${String(variant)}`, `upcontrol: A/B test "${label}" has no variant "${String(variant)}"`);
        return;
      }
      // A test counts people: a who it cannot name sends nothing.
      const id = actor(who);
      if (id === null) return;
      sendEvent(deps.client, label, { 'uc.actor': id, 'uc.variant': v, 'uc.stat': stat });
    } catch {
      /* never throws */
    }
  }

  return {
    expose: (variant, who) => record('exposed', variant, who),
    convert: (variant, who) => record('converted', variant, who),
  };
}

export function createRetention(name: string, deps: FeedDeps): Retention {
  const label = typeof name === 'string' ? name.trim() : String(name);
  const problem = nameProblem(name);
  if (problem !== null) {
    warnOnce(`ignored:${label}:${problem}`, `upcontrol: retention "${label}" is ignored: ${problem}`);
    return NOOP_RETENTION;
  }

  function seen(userId: string): void {
    try {
      if (typeof userId !== 'string') return;
      const id = userId.trim();
      if (id === '') return;
      sendEvent(deps.client, label, { 'uc.actor': id });
    } catch {
      /* never throws */
    }
  }

  return { seen };
}

export function createBreakdown(name: string, deps: FeedDeps): Breakdown {
  const label = typeof name === 'string' ? name.trim() : String(name);
  const problem = nameProblem(name);
  if (problem !== null) {
    warnOnce(`ignored:${label}:${problem}`, `upcontrol: breakdown "${label}" is ignored: ${problem}`);
    return NOOP_BREAKDOWN;
  }

  function feed(v: string, who?: Who | null): void {
    try {
      if (typeof v !== 'string') return;
      const value = v.trim();
      if (value === '' || value.length > 80) return;
      // No who: an event, and the server counts events. A who that cannot be named sends
      // nothing rather than quietly degrading a people count into an event count.
      if (who === null || who === undefined) {
        sendEvent(deps.client, label, { value });
        return;
      }
      const id = actor(who);
      if (id === null) return;
      sendEvent(deps.client, label, { 'uc.actor': id, value });
    } catch {
      /* never throws */
    }
  }

  return { value: feed };
}
