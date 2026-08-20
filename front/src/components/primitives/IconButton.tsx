import { forwardRef, type ButtonHTMLAttributes, type ReactNode } from 'react';
import styles from './IconButton.module.css';

export interface IconButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  size?: 'sm' | 'md';
  icon: ReactNode;
  /** Required — an icon-only button is invisible to screen readers without it. */
  'aria-label': string;
}

export const IconButton = forwardRef<HTMLButtonElement, IconButtonProps>(function IconButton(
  { size = 'md', icon, className, ...rest },
  ref,
) {
  // uc-tap-inline: keep the visual chip at its control size on the phone and
  // grow the hit area instead (global.css) — the box itself is positioned in
  // IconButton.module.css, per that rule's contract.
  const classes = [styles.button, styles[size], 'uc-tap-inline', className].filter(Boolean).join(' ');
  return (
    <button ref={ref} className={classes} {...rest}>
      {icon}
    </button>
  );
});
