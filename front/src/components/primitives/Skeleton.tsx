import styles from './Skeleton.module.css';

/** Flat bar, no shimmer, no spinner: the only loading affordance for
 *  page-level content. */
export function Skeleton({ width = '100%', height = 12 }: { width?: string | number; height?: number }) {
  return <span className={styles.bar} style={{ width, height }} />;
}
