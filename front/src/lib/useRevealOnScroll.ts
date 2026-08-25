import { useEffect, useRef } from 'react';

/** Hidden state is applied by JS, so without JS everything stays visible; the
 *  armed WeakSet (not a DOM attribute) lets a StrictMode remount re-arm. */
export function useRevealOnScroll(): void {
  const ioRef = useRef<IntersectionObserver | null>(null);
  const armedRef = useRef<WeakSet<Element> | null>(null);

  useEffect(() => {
    const reduce = window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)').matches;
    if (reduce || typeof IntersectionObserver === 'undefined') return;
    const ease = 'cubic-bezier(.2,0,0,1)';
    if (!ioRef.current) {
      armedRef.current = new WeakSet();
      ioRef.current = new IntersectionObserver(
        (entries) => {
          entries.forEach((entry) => {
            if (!entry.isIntersecting) return;
            const el = entry.target as HTMLElement;
            const d = `${parseInt(el.getAttribute('data-reveal-delay') ?? '0', 10) || 0}ms`;
            el.style.transitionDelay = `${d}, ${d}`;
            if (el.hasAttribute('data-grow')) {
              el.style.transform = 'scaleY(1)';
            } else {
              el.style.opacity = '1';
              el.style.transform = 'none';
            }
            ioRef.current?.unobserve(el);
          });
        },
        { threshold: 0.12, rootMargin: '0px 0px -6% 0px' },
      );
    }
    document.querySelectorAll<HTMLElement>('[data-reveal],[data-grow]').forEach((el) => {
      if (armedRef.current!.has(el)) return;
      armedRef.current!.add(el);
      if (el.hasAttribute('data-grow')) {
        el.style.transformOrigin = 'bottom';
        el.style.transform = 'scaleY(0)';
        el.style.transition = `transform 240ms ${ease}`;
      } else {
        el.style.opacity = '0';
        el.style.transform = 'translateY(10px)';
        el.style.transition = `opacity 240ms ${ease}, transform 240ms ${ease}`;
      }
      ioRef.current?.observe(el);
    });
  });

  useEffect(
    () => () => {
      ioRef.current?.disconnect();
      ioRef.current = null;
      armedRef.current = null;
    },
    [],
  );
}
