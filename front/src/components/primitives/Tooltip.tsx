import { useCallback, useRef, useState, type ReactNode } from 'react';
import { createPortal } from 'react-dom';
import styles from './Tooltip.module.css';

export interface TooltipProps {
  content: ReactNode;
  children: ReactNode;
  /** Applied to the trigger, so a caller can hide the whole tooltip at a breakpoint. */
  className?: string;
  /**
   * The child already takes focus on its own (a button, a link). The wrapper
   * then stops being a tab stop — otherwise every tooltipped button costs two
   * presses of Tab and the first one lands on a span that announces nothing.
   * Graphic triggers — bars, dots — leave this off and are reached through the
   * wrapper, which is their only way to a keyboard.
   */
  interactiveChild?: boolean;
}

const GAP = 8;

/**
 * Custom tooltip — never the native `title` attribute (brief §2.1).
 * Fixed-position, placed below the trigger, but clamped to the viewport: on a
 * narrow screen a trigger near an edge (a health-line segment, a right-column
 * value) would otherwise push its bubble off-screen.
 */
export function Tooltip({ content, children, className, interactiveChild = false }: TooltipProps) {
  const triggerRef = useRef<HTMLSpanElement>(null);
  const [open, setOpen] = useState(false);

  /**
   * Ref callbacks run in the commit phase — the bubble is in the DOM but not
   * yet painted — so measuring and positioning here costs one paint and needs
   * no state. Position must not be state: it would paint once at the previous
   * trigger's coordinates before the correction landed.
   */
  const place = useCallback((bubble: HTMLDivElement | null) => {
    if (!bubble || !triggerRef.current) return;
    const anchor = triggerRef.current.getBoundingClientRect();
    const { width, height } = bubble.getBoundingClientRect();
    const below = anchor.bottom + GAP;
    // Flip above when there is no room underneath, so the bubble never sits off the fold.
    const noRoomBelow = below + height + GAP > window.innerHeight;
    bubble.style.left = `${Math.min(Math.max(GAP, anchor.left), Math.max(GAP, window.innerWidth - width - GAP))}px`;
    bubble.style.top = `${noRoomBelow ? Math.max(GAP, anchor.top - height - GAP) : below}px`;
  }, []);

  return (
    <span
      ref={triggerRef}
      className={[styles.trigger, className].filter(Boolean).join(' ')}
      onMouseEnter={() => setOpen(true)}
      onMouseLeave={() => setOpen(false)}
      // Focus bubbles here from the child, so the tooltip still opens on Tab
      // even when the wrapper itself is not a stop.
      onFocus={() => setOpen(true)}
      onBlur={() => setOpen(false)}
      tabIndex={interactiveChild ? undefined : 0}
    >
      {children}
      {open &&
        createPortal(
          <div ref={place} className={styles.bubble} role="tooltip">
            {content}
          </div>,
          document.body,
        )}
    </span>
  );
}
