import type { ReactNode } from 'react';
import styles from './Callout.module.css';

export type CalloutTone = 'note' | 'tip' | 'warning' | 'danger';

export interface CalloutProps {
  tone?: CalloutTone;
  title: string;
  children: ReactNode;
}

/** Left border accent only — no background fill, no icon (brief §2.1). */
export function Callout({ tone = 'note', title, children }: CalloutProps) {
  return (
    <div className={[styles.callout, styles[tone]].join(' ')}>
      <p className={styles.title}>{title}</p>
      <p className={styles.body}>{children}</p>
    </div>
  );
}
