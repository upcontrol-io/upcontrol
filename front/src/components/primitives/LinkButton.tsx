import type { ReactNode } from 'react';
import { Link } from 'react-router-dom';
import buttonStyles from './Button.module.css';
import type { ButtonSize, ButtonVariant } from './Button';

interface LinkButtonProps {
  variant?: ButtonVariant;
  size?: ButtonSize;
  iconLeft?: ReactNode;
  iconRight?: ReactNode;
  to?: string;
  href?: string;
  className?: string;
  children: ReactNode;
}

/** Button's visual language for links: buttons never navigate, links do. */
export function LinkButton({ variant = 'secondary', size = 'md', iconLeft, iconRight, to, href, className, children }: LinkButtonProps) {
  const classes = [buttonStyles.button, buttonStyles[variant], buttonStyles[size], className].filter(Boolean).join(' ');
  const content = (
    <>
      {iconLeft && <span className={buttonStyles.icon}>{iconLeft}</span>}
      {children}
      {iconRight && <span className={buttonStyles.icon}>{iconRight}</span>}
    </>
  );

  if (to) {
    return (
      <Link to={to} className={classes}>
        {content}
      </Link>
    );
  }

  return (
    <a href={href} className={classes} target={href?.startsWith('http') ? '_blank' : undefined} rel="noreferrer">
      {content}
    </a>
  );
}
