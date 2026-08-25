import type { ReactNode } from 'react';
import styles from './Badge.module.css';

export type BadgeTone = 'neutral' | 'new' | 'beta' | 'deprecated' | 'plan' | 'ok' | 'check' | 'down';

interface BadgeProps {
  tone?: BadgeTone;
  shape?: 'rect' | 'pill';
  children: ReactNode;
  className?: string;
}

/** Also used as LevelBadge (feature-tier tags like "No code", "Indie+"). */
export function Badge({ tone = 'neutral', shape = 'rect', children, className }: BadgeProps) {
  return <span className={[styles.badge, styles[shape], styles[tone], className].filter(Boolean).join(' ')}>{children}</span>;
}
