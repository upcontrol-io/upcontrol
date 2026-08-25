import { forwardRef, type ButtonHTMLAttributes, type ReactNode } from 'react';
import styles from './Button.module.css';

export type ButtonVariant = 'primary' | 'secondary' | 'ghost' | 'danger';
export type ButtonSize = 'sm' | 'md' | 'lg';

interface ButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: ButtonVariant;
  size?: ButtonSize;
  iconLeft?: ReactNode;
  iconRight?: ReactNode;
  loading?: boolean;
}

/** The one Button in the product — every screen composes this, never a raw <button>. */
export const Button = forwardRef<HTMLButtonElement, ButtonProps>(function Button(
  { variant = 'secondary', size = 'md', iconLeft, iconRight, loading = false, disabled, className, children, ...rest },
  ref,
) {
  const classes = [styles.button, styles[variant], styles[size], className].filter(Boolean).join(' ');

  return (
    <button ref={ref} className={classes} disabled={disabled || loading} aria-busy={loading || undefined} {...rest}>
      {loading ? <span className={styles.spinner} aria-hidden="true" /> : iconLeft && <span className={styles.icon}>{iconLeft}</span>}
      {children}
      {!loading && iconRight && <span className={styles.icon}>{iconRight}</span>}
    </button>
  );
});
