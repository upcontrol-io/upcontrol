import type { ReactNode } from 'react';
import { Link } from 'react-router-dom';
import buttonStyles from './Button.module.css';
import type { ButtonSize, ButtonVariant } from './Button';

interface LinkButtonProps {
  variant?: ButtonVariant;
  size?: ButtonSize;
  to: string;
  className?: string;
  children: ReactNode;
}

/** Button's visual language for links: buttons never navigate, links do. */
export function LinkButton({ variant = 'secondary', size = 'md', to, className, children }: LinkButtonProps) {
  const classes = [buttonStyles.button, buttonStyles[variant], buttonStyles[size], className].filter(Boolean).join(' ');

  return (
    <Link to={to} className={classes}>
      {children}
    </Link>
  );
}
