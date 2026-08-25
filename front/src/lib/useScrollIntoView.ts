import { useEffect, useRef } from 'react';

/** Brings a just-appeared panel into view. `block: 'nearest'` scrolls the
 *  minimum and nothing when already on screen, so asking twice never jumps. */
export function useScrollIntoView<T extends HTMLElement>(trigger: unknown) {
  const ref = useRef<T>(null);

  useEffect(() => {
    if (!trigger || !ref.current) return;
    const reduce = window.matchMedia?.('(prefers-reduced-motion: reduce)').matches;
    ref.current.scrollIntoView({ block: 'nearest', behavior: reduce ? 'auto' : 'smooth' });
  }, [trigger]);

  return ref;
}
