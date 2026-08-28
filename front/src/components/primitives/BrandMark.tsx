import styles from './BrandMark.module.css';

interface BrandMarkProps {
  size?: number;
  className?: string;
}

/** The product mark: the Viewfinder, four corner brackets on a green signal,
 *  inline so header and favicon can never disagree. Keyframes in the module. */
export function BrandMark({ size = 18, className }: BrandMarkProps) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 16 16"
      fill="none"
      aria-hidden="true"
      className={[styles.mark, className].filter(Boolean).join(' ')}
    >
      {/* The four corner brackets, clockwise from top-left. One group: the
          ring sweeps as a whole. */}
      <g fill="currentColor" className={styles.brackets}>
        <rect x="1" y="1" width="5" height="2" />
        <rect x="1" y="3" width="2" height="3" />
        <rect x="10" y="1" width="5" height="2" />
        <rect x="13" y="3" width="2" height="3" />
        <rect x="10" y="13" width="5" height="2" />
        <rect x="13" y="10" width="2" height="3" />
        <rect x="1" y="13" width="5" height="2" />
        <rect x="1" y="10" width="2" height="3" />
      </g>
      <g className={styles.signal}>
        <rect x="7" y="5" width="2" height="1" />
        <rect x="6" y="6" width="4" height="1" />
        <rect x="5" y="7" width="6" height="2" />
        <rect x="6" y="9" width="4" height="1" />
        <rect x="7" y="10" width="2" height="1" />
      </g>
    </svg>
  );
}
