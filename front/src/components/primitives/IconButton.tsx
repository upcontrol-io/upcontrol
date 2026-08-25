import { forwardRef, type ButtonHTMLAttributes, type ReactNode } from 'react';
import styles from './IconButton.module.css';

interface IconButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  size?: 'sm' | 'md';
  icon: ReactNode;
  /** Required — an icon-only button is invisible to screen readers without it. */
  'aria-label': string;
}

export const IconButton = forwardRef<HTMLButtonElement, IconButtonProps>(function IconButton(
  { size = 'md', icon, className, ...rest },
  ref,
) {
  // uc-tap-inline: keep the chip at control size on the phone and grow the
  // hit area instead (global.css).
  const classes = [styles.button, styles[size], 'uc-tap-inline', className].filter(Boolean).join(' ');
  return (
    <button ref={ref} className={classes} {...rest}>
      {icon}
    </button>
  );
});
