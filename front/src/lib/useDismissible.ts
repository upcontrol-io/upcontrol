import { useEffect, useRef } from 'react';

/**
 * The two things every dismissible overlay in the product owes the user:
 * Escape closes it, and the page behind it stops scrolling while it is up.
 *
 * Shared by `Modal` and `useDrawer` — they had drifted to different fidelity
 * (the drawer locked scroll, the modal didn't) writing this by hand twice.
 */
export function useDismissible(active: boolean, onDismiss: () => void) {
  // Held in a ref so an inline `onClose={() => …}` can't re-run the effect and
  // thrash the body style on every render of the host.
  const dismiss = useRef(onDismiss);
  dismiss.current = onDismiss;

  useEffect(() => {
    if (!active) return;

    function onKeyDown(event: KeyboardEvent) {
      if (event.key === 'Escape') dismiss.current();
    }
    document.addEventListener('keydown', onKeyDown);

    const previousOverflow = document.body.style.overflow;
    document.body.style.overflow = 'hidden';

    return () => {
      document.removeEventListener('keydown', onKeyDown);
      document.body.style.overflow = previousOverflow;
    };
  }, [active]);
}
