import styles from './StatusDot.module.css';

type Status = 'ok' | 'down' | 'nodata';

interface StatusDotProps {
  status: Status;
  label?: string;
  className?: string;
}

const DEFAULT_LABEL: Record<Status, string> = {
  ok: 'Operational',
  down: 'Down',
  nodata: 'No data',
};

/** Status is never color alone: dot shape differs per state, and a label
 *  always accompanies it. */
export function StatusDot({ status, label, className }: StatusDotProps) {
  const text = label ?? DEFAULT_LABEL[status];
  return (
    <span className={[styles.wrap, className].filter(Boolean).join(' ')}>
      <span className={[styles.dot, styles[status]].filter(Boolean).join(' ')} aria-hidden="true" />
      {text}
    </span>
  );
}
