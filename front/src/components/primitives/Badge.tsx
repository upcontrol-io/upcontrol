import type { ReactNode } from 'react';
import styles from './Badge.module.css';

export type BadgeTone = 'neutral' | 'ok' | 'check' | 'down';

interface BadgeProps {
  tone?: BadgeTone;
  children: ReactNode;
  className?: string;
}

/** Also used as LevelBadge (feature-tier tags like "No code", "Indie+"). */
export function Badge({ tone = 'neutral', children, className }: BadgeProps) {
  return <span className={[styles.badge, styles.rect, styles[tone], className].filter(Boolean).join(' ')}>{children}</span>;
}
