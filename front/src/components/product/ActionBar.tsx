import { useState, type ReactNode } from 'react';
import { Button, ConfirmPanel, Tooltip, type ButtonVariant } from '@/components/primitives';
import styles from './ActionBar.module.css';

export interface ActionBarItem {
  id: string;
  label: string;
  variant: ButtonVariant;
  onFire: () => void;
  icon?: ReactNode;
  /** Says what the button actually does before it is pressed — never a native title (brief §2.1). */
  tooltip?: string;
  /** Danger actions always double-confirm with a PIN before firing (brief §5 / §2.4). */
  confirmExplanation?: string;
}

export function ActionBar({ actions }: { actions: ActionBarItem[] }) {
  const [confirmingId, setConfirmingId] = useState<string | null>(null);
  const confirming = actions.find((a) => a.id === confirmingId);

  return (
    <div className={styles.wrap}>
      <div className={styles.row}>
        {actions.map((action) => {
          const button = (
            <Button
              size="sm"
              variant={action.variant}
              iconLeft={action.icon}
              className={styles.actionButton}
              onClick={() => (action.variant === 'danger' ? setConfirmingId(action.id) : action.onFire())}
            >
              {action.label}
            </Button>
          );
          return action.tooltip ? (
            <Tooltip key={action.id} content={action.tooltip} interactiveChild>
              {button}
            </Tooltip>
          ) : (
            <span key={action.id}>{button}</span>
          );
        })}
      </div>
      {confirming && (
        <ConfirmPanel
          explanation={confirming.confirmExplanation ?? `This will ${confirming.label.toLowerCase()} immediately.`}
          confirmLabel={confirming.label}
          onConfirm={() => {
            confirming.onFire();
            setConfirmingId(null);
          }}
          onCancel={() => setConfirmingId(null)}
        />
      )}
    </div>
  );
}
