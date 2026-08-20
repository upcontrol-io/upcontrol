import type { InputHTMLAttributes, ReactNode } from 'react';
import { CheckIcon } from '@/icons';
import styles from './Checkbox.module.css';

export interface CheckboxProps extends Omit<InputHTMLAttributes<HTMLInputElement>, 'type'> {
  /** A node, not just a string: rich rows (title + description) stay clickable as one label. */
  label?: ReactNode;
}

export function Checkbox({ label, className, ...rest }: CheckboxProps) {
  return (
    <label className={[styles.wrap, className].filter(Boolean).join(' ')}>
      <input type="checkbox" className={styles.input} {...rest} />
      <span className={styles.box}>
        <CheckIcon className={styles.check} width={9} height={9} />
      </span>
      {label}
    </label>
  );
}
