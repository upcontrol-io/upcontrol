import { forwardRef, useId, type InputHTMLAttributes } from 'react';
import styles from './Field.module.css';

interface InputProps extends InputHTMLAttributes<HTMLInputElement> {
  label?: string;
  error?: string;
}

export const Input = forwardRef<HTMLInputElement, InputProps>(function Input({ label, error, id, className, ...rest }, ref) {
  const generatedId = useId();
  const inputId = id ?? generatedId;

  return (
    <label className={styles.field} htmlFor={inputId}>
      {label}
      <input
        ref={ref}
        id={inputId}
        className={[styles.control, error && styles.controlInvalid, className].filter(Boolean).join(' ')}
        aria-invalid={Boolean(error) || undefined}
        {...rest}
      />
      {error && <span className={styles.error}>{error}</span>}
    </label>
  );
});
