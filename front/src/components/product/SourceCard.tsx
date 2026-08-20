import { StatusDot } from '@/components/primitives';
import type { HealthStatus, Source } from '@/lib/types';
import { SourceIcon } from './SourceIcon';
import styles from './SourceCard.module.css';

// Status ships as a human word next to the dot, never the raw enum (brief §2.4).
const STATUS_WORD: Record<HealthStatus, string> = { ok: 'up', check: 'checking', down: 'down', nodata: 'no data' };

/**
 * Resolves a source to what it looks like right now, incident overlay included.
 * It lives beside the card rather than in each screen because the Dashboard and
 * the Sources tab both render the same row: when they each derived the label
 * themselves, Stripe read "up · 1 min ago" on one and "down for 40 minutes" on
 * the other, about the same connection at the same moment.
 */
export function sourceNow(source: Source, incidentOpen: boolean): { status: HealthStatus; label: string } {
  if (source.paused) return { status: source.status, label: 'paused by you' };
  if (incidentOpen && source.duringIncident) return source.duringIncident;
  return { status: source.status, label: source.lastSignal };
}

export function SourceCard({ source, statusLabel }: { source: Source; statusLabel?: string }) {
  const label = statusLabel ?? (source.paused ? `paused · ${source.lastSignal}` : `${STATUS_WORD[source.status]} · ${source.lastSignal}`);
  return (
    <div className={styles.card}>
      <SourceIcon source={source} className={styles.mark} />
      <div className={styles.info}>
        <span className={styles.name}>{source.name}</span>
        <StatusDot status={source.paused ? 'paused' : source.status} label={label} />
      </div>
    </div>
  );
}
