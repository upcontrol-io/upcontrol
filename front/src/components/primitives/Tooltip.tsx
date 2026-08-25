import { useCallback, useRef, useState, type ReactNode } from 'react';
import { createPortal } from 'react-dom';
import styles from './Tooltip.module.css';

export interface TooltipProps {
  content: ReactNode;
  children: ReactNode;
  /** Applied to the trigger, so a caller can hide the whole tooltip at a breakpoint. */
  className?: string;
  /** The child takes focus on its own (button, link), so the wrapper stops
   *  being a tab stop; graphic triggers keep the wrapper as their only path. */
  interactiveChild?: boolean;
}

const GAP = 8;

/** Fixed-position tooltip, clamped to the viewport: near an edge on a narrow
 *  screen the bubble would otherwise go off-screen. */
export function Tooltip({ content, children, className, interactiveChild = false }: TooltipProps) {
  const triggerRef = useRef<HTMLSpanElement>(null);
  const [open, setOpen] = useState(false);

  /** Position in the ref callback, never in state: state would paint once at
   *  the previous trigger's coordinates before the correction landed. */
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
