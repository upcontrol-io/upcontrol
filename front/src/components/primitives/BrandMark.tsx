import styles from './BrandMark.module.css';

export interface BrandMarkProps {
  size?: number;
  /** 'half-turn' is the header's sweep; 'chase' is the kit's 2c corner-chase
   *  (staggered quarter turns), used for waiting states. */
  variant?: 'half-turn' | 'chase';
  className?: string;
}

/**
 * The product mark: the Viewfinder — four corner brackets locked on a green
 * signal. It is the same 16x16 pixel drawing the favicon pack carries
 * (public/favicons), redrawn inline so the header and the browser tab can
 * never disagree. Inline SVG is the house rule anyway (brief §Assets: no icon
 * font, no icon library).
 *
 * Animated as the kit's half-turn: the brackets sweep half a circle around
 * the still signal and land in the identical pose — the drawing is 2-fold
 * symmetric, so the loop has no seam — and the signal answers the landing
 * with a double beat. Chosen by look (user decision, Aug 15, 2026): the
 * kit's README files this recipe under 2a-half-turn, while its "2b lock-on"
 * names a fly-in + ping ring, which was tried here and rejected. Ported
 * rather than shipped as the asset file: the pack's files hardcode hex where
 * the app needs currentColor for the brackets and var(--ok) for the green,
 * so one component sits on the landing hero's gradient, on both chrome
 * themes and inside the station art unchanged (`.artMark` sets `color`).
 * The keyframes live in BrandMark.module.css.
 *
 * The brackets' corners sweep outside the 16-unit grid mid-turn (radius
 * ~9.9 from centre against the box's 8). The svg keeps the tight viewBox —
 * every call site sizes it by the resting mark — and un-clips with
 * `overflow: visible` instead; the kit's safe-viewbox asset family solves
 * the same problem with baked margins, which inline would shrink the mark
 * inside every existing header.
 *
 * No shape-rendering="crispEdges", deliberately: at the exact grid sizes
 * (16, 32) integer edges rasterise crisp on their own, at the in-between
 * header size (18) crispEdges would snap the arms to uneven widths, and the
 * pose is fractional for the whole sweep anyway.
 *
 * Ported second from the same kit: `variant="chase"` is the 2c corner-chase
 * (owner decision, Aug 18, 2026) — each corner bracket takes a quarter turn
 * about the mark centre, 90ms behind the one before it, while the signal
 * keeps its double beat on the same 3s cycle. It is the reading mark while
 * Explain waits for its answer: the one named exception to the app's
 * "never a spinner" rule, scoped to that single state.
 */
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
      {/* The corners sit in chase order (clockwise from top-left), each in its
          own group because 2c turns corners, not the whole ring. The wrappers
          carry no transform of their own, so the half-turn renders identically
          to the flat list it replaced. */}
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
