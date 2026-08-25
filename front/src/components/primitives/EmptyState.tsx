import type { ReactNode } from 'react';
import styles from './EmptyState.module.css';

interface EmptyStateProps {
  icon?: ReactNode;
  title: string;
  body: string;
  action?: ReactNode;
  /** Pass false inside a panel that already draws one: a bordered box in a
   * bordered panel is a card in a card. */
  framed?: boolean;
}

/** An invitation to act, never a bare "no data" message. */
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
