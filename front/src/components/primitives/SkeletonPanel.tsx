import { Skeleton } from './Skeleton';
import styles from './SkeletonPanel.module.css';

/** What a panel shows while its first read is on the wire: a skeleton claims
 *  nothing, where mock data would flash somebody else's account. */
export function SkeletonPanel({
  rows = 3,
  label = 'Loading',
}: {
  rows?: number;
  /** Announced to a screen reader, which sees no shape at all. */
  label?: string;
}) {
  return (
    <div className={styles.panel} role="status" aria-busy="true">
      <span className="visually-hidden">{label}</span>
      {Array.from({ length: rows }, (_, index) => (
        <div key={index} className={styles.row} aria-hidden="true">
          <Skeleton width="34%" height={12} />
          <Skeleton width="18%" height={10} />
        </div>
      ))}
    </div>
  );
}
