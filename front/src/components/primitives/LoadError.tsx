import { Button } from './Button';
import { EmptyState } from './EmptyState';
import { StatusDot } from './StatusDot';
import styles from './LoadError.module.css';

interface LoadErrorProps {
  /** What could not be read, in the reader's words: "your checks", "your alert channels". */
  what: string;
  /** Re-runs the read. Wire it to `invalidateApiData(<the same keys the screen reads>)`. */
  onRetry: () => void;
  /** Inside a panel that already draws a frame — same rule as EmptyState's. */
  framed?: boolean;
}

/** A read that settled without an answer, never an EmptyState: "you have
 *  none" on the strength of a failed request is a lie. Composes EmptyState. */
export function LoadError({ what, onRetry, framed = true }: LoadErrorProps) {
  return (
    <div role="alert" className={styles.wrap}>
      <EmptyState
        framed={framed}
        icon={<StatusDot status="down" label="" className={styles.dot} />}
        title={`Could not load ${what}`}
        body="The request did not get through, so we could not ask. Nothing here is missing from your account. Try again, or check that the backend is running."
        action={
          <Button size="sm" onClick={onRetry}>
            Try again
          </Button>
        }
      />
    </div>
  );
}
