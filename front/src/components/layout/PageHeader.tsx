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

/**
 * The one header grammar the shell screens share. Before it, eight screens wore
 * four different shapes: most had an `<h1>`, Logs had none and hid its title as
 * small bold text inside the panel chrome, Monitors wedged its description into
 * the action row, Sources had a title and nothing else. Worse, the three screens
 * with loading and failed branches repeated their own `<h1>` in each one, so the
 * heading was declared eleven times across seven files.
 *
 * The action sits on the title's line while it fits and drops below it on a
 * phone, which is why this is a wrapping flex row rather than a grid: a long
 * title and a long button label should reflow, not collide.
 */
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
