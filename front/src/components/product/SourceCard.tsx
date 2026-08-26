import type { HealthStatus, Source } from '@/lib/types';
/** Resolves a source to what it looks like right now, incident overlay
 *  included; shared so every screen renders the same row the same way. */
export function sourceNow(source: Source, incidentOpen: boolean): { status: HealthStatus; label: string } {
  if (source.paused) return { status: source.status, label: 'paused by you' };
  if (incidentOpen && source.duringIncident) return source.duringIncident;
  return { status: source.status, label: source.lastSignal };
}
