import type { ReactNode } from 'react';
import { CheckEventIcon, DeployIcon, ErrorIcon, PaymentIcon } from '@/icons';
import type { TimelineEntry, TimelineEventKind } from '@/lib/types';
import styles from './TimelineEvent.module.css';

// The "people/reach" event uses the hand-off arrow glyph from 05-dashboard's
// glyph map ("who to hand it to"), not a person silhouette.
function ReachIcon() {
  return (
    <svg width={14} height={14} viewBox="0 0 32 32" fill="none" aria-hidden="true">
      <path d="M4 16H27M19 8L27 16L19 24" stroke="currentColor" strokeWidth={2.5} strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  );
}

const ICONS: Record<TimelineEventKind, ReactNode> = {
  deploy: <DeployIcon width={14} height={14} />,
  error: <ErrorIcon width={14} height={14} />,
  payment: <PaymentIcon width={14} height={14} />,
  check: <CheckEventIcon width={14} height={14} />,
  people: <ReachIcon />,
};

/** Distinct icon shape per event type — never color-coded (brief §2.4). */
export function TimelineEvent({ entry }: { entry: TimelineEntry }) {
  return (
    <div className={styles.row}>
      <span className={styles.time}>
        <span className={styles.timeAbs}>{entry.time}</span>
        <span className={styles.ago}>{entry.ago}</span>
      </span>
      <span className={styles.glyph} aria-label={entry.kind}>
        {ICONS[entry.kind]}
      </span>
      <span className={[styles.text, entry.dim && styles.textDim].filter(Boolean).join(' ')}>{entry.text}</span>
    </div>
  );
}
