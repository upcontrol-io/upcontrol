// The funnel feed: one event per step, named by the step, the person as `uc.actor` and the
// journey as `uc.funnel`. The server counts DISTINCT people per step from the events it
// stores, so the SDK does not dedup — the same person stepping twice is two events, by
// design. The identity machinery (the request fingerprint, the salt that keeps a raw
// address off the wire) lives in counters.ts, shared with the other feeds.

import { sendEvent } from './client.js';
import { actor, warnOnce, type FeedDeps, type Who } from './counters.js';

export type { RequestLike, Who } from './counters.js';
export type FunnelDeps = FeedDeps;

export interface Funnel {
  /** step records `who` at the step `name`. Never throws. */
  step(name: string, who: Who): void;
}

const NOOP: Funnel = { step() {} };

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

export function createFunnel(name: string, steps: string[], deps: FunnelDeps): Funnel {
  const label = typeof name === 'string' ? name.trim() : String(name);
  const problem = problemWith(name, steps);
  if (problem !== null) {
    warnOnce(`ignored:${label}:${problem}`, `upcontrol: funnel "${label}" is ignored: ${problem}`);
    return NOOP;
  }
  const names = new Set(steps.map((s) => s.trim()));

  function step(stepName: string, who: Who): void {
    try {
      const s = typeof stepName === 'string' ? stepName.trim() : '';
      if (!names.has(s)) {
        warnOnce(`nostep:${label}:${String(stepName)}`, `upcontrol: funnel "${label}" has no step "${String(stepName)}"`);
        return;
      }
      // The funnel counts people, not events: a who it cannot name (a null, an unusable id,
      // a request with no address) sends nothing.
      const id = actor(who);
      if (id === null) return;
      sendEvent(deps.client, s, { 'uc.actor': id, 'uc.funnel': label });
    } catch {
      /* step never throws */
    }
  }

  return { step };
}
