import styles from './StatusDot.module.css';

type Status = 'ok' | 'check' | 'down' | 'nodata' | 'paused';

interface StatusDotProps {
  status: Status;
  label?: string;
  className?: string;
}

const DEFAULT_LABEL: Record<Status, string> = {
  ok: 'Operational',
  check: 'Checking',
  down: 'Down',
  nodata: 'No data',
  paused: 'Paused',
};

/** Status is never color alone: dot shape differs per state, and a label
 *  always accompanies it. */
export function StatusDot({ status, label, className }: StatusDotProps) {
  const text = label ?? DEFAULT_LABEL[status];
  return (
    <span className={[styles.wrap, className].filter(Boolean).join(' ')}>
      {/* `uc-pulse` is global (global.css) — a CSS Module cannot reference a
          keyframes name it does not itself declare. */}
      <span className={[styles.dot, styles[status], status === 'check' && 'uc-pulse'].filter(Boolean).join(' ')} aria-hidden="true" />
      {text}
    </span>
  );
}
