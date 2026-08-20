import { Skeleton } from './Skeleton';
import styles from './SkeletonPanel.module.css';

/**
 * What an /app panel shows while its first read is still on the wire.
 *
 * The alternative was what /app used to do: render `mockData` immediately and
 * swap it for the account's own data when the response landed — so every reload
 * flashed somebody else's monitors, sources and incident for a few hundred
 * milliseconds. Mock is the fallback for an *unreachable* backend (the shell
 * says so in a Callout); it is not a placeholder for an answer that is simply
 * still coming. A skeleton claims nothing, which is the whole point.
 *
 * No spinner, by the design brief: page-level loading is skeletons only.
 */
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
