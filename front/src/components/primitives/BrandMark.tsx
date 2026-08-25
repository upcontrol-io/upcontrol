import styles from './BrandMark.module.css';

interface BrandMarkProps {
  size?: number;
  /** 'half-turn' is the header's sweep; 'chase' is the kit's 2c corner-chase
   *  (staggered quarter turns), used for waiting states. */
  variant?: 'half-turn' | 'chase';
  className?: string;
}

/** The product mark: the Viewfinder, four corner brackets on a green signal,
 *  inline so header and favicon can never disagree. Keyframes in the module. */
export function BrandMark({
  size = 18,
  variant = 'half-turn',
  className,
}: BrandMarkProps) {
  // The half-turn animates the bracket ring as one group; the chase rotates
  // each corner separately, so the ring itself carries no animation at all.
  const chase = variant === 'chase';
  const cornerClass = chase ? styles.chaseCorner : undefined;
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 16 16"
      fill="none"
      aria-hidden="true"
      className={[styles.mark, className].filter(Boolean).join(' ')}
    >
      {/* Each corner in its own group: the chase turns corners, not the ring;
          untransformed wrappers keep the half-turn identical. */}
      <g fill="currentColor" className={chase ? undefined : styles.brackets}>
        <g className={cornerClass}>
          <rect x="1" y="1" width="5" height="2" />
          <rect x="1" y="3" width="2" height="3" />
        </g>
        <g className={cornerClass}>
          <rect x="10" y="1" width="5" height="2" />
          <rect x="13" y="3" width="2" height="3" />
        </g>
        <g className={cornerClass}>
          <rect x="10" y="13" width="5" height="2" />
          <rect x="13" y="10" width="2" height="3" />
        </g>
        <g className={cornerClass}>
          <rect x="1" y="13" width="5" height="2" />
          <rect x="1" y="10" width="2" height="3" />
        </g>
      </g>
      <g
        className={[styles.signal, chase && styles.chaseSignal]
          .filter(Boolean)
          .join(' ')}
      >
        <rect x="7" y="5" width="2" height="1" />
        <rect x="6" y="6" width="4" height="1" />
        <rect x="5" y="7" width="6" height="2" />
        <rect x="6" y="9" width="4" height="1" />
        <rect x="7" y="10" width="2" height="1" />
      </g>
    </svg>
  );
}
