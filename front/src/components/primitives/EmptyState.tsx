import type { ReactNode } from 'react';
import styles from './EmptyState.module.css';

export interface EmptyStateProps {
  icon?: ReactNode;
  title: string;
  body: string;
  action?: ReactNode;
  /**
   * Draw the card (border + raised background), as in the component reference.
   * Pass `false` inside a panel that already draws one: a bordered box floating
   * in the corner of a bordered panel is a card in a card, which is exactly what
   * the logs panel looked like.
   */
  framed?: boolean;
}

/** An invitation to act, never a bare "no data" message (brief §2.1). */
export function EmptyState({ icon, title, body, action, framed = true }: EmptyStateProps) {
  return (
    <div className={[styles.wrap, !framed && styles.bare].filter(Boolean).join(' ')}>
      {icon && <span className={styles.icon}>{icon}</span>}
      <p className={styles.title}>{title}</p>
      <p className={styles.body}>{body}</p>
      {action}
    </div>
  );
}
