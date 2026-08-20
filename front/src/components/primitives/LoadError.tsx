import { Button } from './Button';
import { EmptyState } from './EmptyState';
import { StatusDot } from './StatusDot';
import styles from './LoadError.module.css';

export interface LoadErrorProps {
  /** What could not be read, in the reader's words: "your checks", "your alert channels". */
  what: string;
  /** Re-runs the read. Wire it to `invalidateApiData(<the same keys the screen reads>)`. */
  onRetry: () => void;
  /** Inside a panel that already draws a frame — same rule as EmptyState's. */
  framed?: boolean;
}

/**
 * A read that settled without an answer. Deliberately NOT an `EmptyState` on its
 * own: an empty state says "you have none", and saying that on the strength of a
 * failed request tells a customer with three checks that they have none. The two
 * shapes have to be distinguishable at a glance, so this one carries a `down`
 * dot, `role="alert"`, and the one action that can change the outcome.
 *
 * It composes EmptyState rather than re-drawing the box: the padding, the
 * measure and the `framed` rule are already decided there, and a second copy of
 * them is how the two drift apart.
 */
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
