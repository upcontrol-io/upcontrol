import { useEffect, useRef } from 'react';

/**
 * Brings a panel that just appeared into view.
 *
 * Both AI reads in the product (the incident card's triage, the log panel's
 * explain) render *below* the button that asks for them. On a laptop that is
 * fine, because the answer lands in the same screenful as the button. On a
 * phone it is the whole bug: the button sits low in a tall card, so you tap
 * Explain, the answer renders off the bottom of the screen, and nothing appears
 * to have happened. The tap needs to move the page, or it reads as broken.
 *
 * `block: 'nearest'` is what makes it safe to call more than once: it scrolls
 * the minimum needed and does nothing at all when the panel is already on
 * screen, so the skeleton can scroll into view and the finished text can ask
 * again without the page jumping a second time. It also honours the panel's own
 * `scroll-margin`, which is how the result clears the sticky header and the
 * floating tab bar.
 *
 * Pass the value that identifies the reveal (a boolean, or the answer itself so
 * a second Explain re-triggers). Falsy never scrolls.
 */
export function useScrollIntoView<T extends HTMLElement>(trigger: unknown) {
  const ref = useRef<T>(null);

  useEffect(() => {
    if (!trigger || !ref.current) return;
    const reduce = window.matchMedia?.('(prefers-reduced-motion: reduce)').matches;
    ref.current.scrollIntoView({ block: 'nearest', behavior: reduce ? 'auto' : 'smooth' });
  }, [trigger]);

  return ref;
}
