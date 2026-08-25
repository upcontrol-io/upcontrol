import type { ReactNode } from 'react';
import { Link } from 'react-router-dom';
import styles from './PageHeader.module.css';

export interface PageHeaderProps {
  title: ReactNode;
  /** One sentence saying what the screen is for. Omitted when the title says it. */
  description?: ReactNode;
  /** The screen's primary control, right-aligned above 700px and stacked below. */
  action?: ReactNode;
  /** A detail screen's way back to its list. */
  back?: { to: string; label: string };
}

/** The one header grammar the shell screens share. The action sits on the
 *  title's line while it fits: a wrapping flex row so long titles reflow. */
export function PageHeader({ title, description, action, back }: PageHeaderProps) {
  return (
    <header className={styles.header}>
      {back && (
        <Link to={back.to} className={styles.back}>
          ← {back.label}
        </Link>
      )}
      <div className={styles.row}>
        <div className={styles.text}>
          <h1 className={styles.title}>{title}</h1>
          {description && <p className={styles.description}>{description}</p>}
        </div>
        {action && <div className={styles.action}>{action}</div>}
      </div>
    </header>
  );
}
