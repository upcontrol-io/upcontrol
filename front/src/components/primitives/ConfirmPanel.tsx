import { useState } from 'react';
import { Button } from './Button';
import { Input } from './Input';
import styles from './ConfirmPanel.module.css';

interface ConfirmPanelProps {
  explanation: string;
  /** If set, the user must type this exact phrase; otherwise a 4-digit PIN is required. */
  typedConfirmation?: string;
  confirmLabel: string;
  onConfirm: () => void;
  onCancel: () => void;
}

/** Inline double-confirmation for destructive actions, never confirm(). */
export function ConfirmPanel({ explanation, typedConfirmation, confirmLabel, onConfirm, onCancel }: ConfirmPanelProps) {
  const [value, setValue] = useState('');
  const ready = typedConfirmation ? value === typedConfirmation : value.length === 4;

  return (
    <div className={styles.panel}>
      <p className={styles.explanation}>{explanation}</p>
      <Input
        label={typedConfirmation ? `Type "${typedConfirmation}" to confirm` : 'Enter your PIN'}
        value={value}
        onChange={(event) => setValue(event.target.value)}
        inputMode={typedConfirmation ? 'text' : 'numeric'}
        maxLength={typedConfirmation ? undefined : 4}
        autoFocus
      />
      <div className={styles.actions}>
        <Button variant="ghost" size="sm" onClick={onCancel}>
          Cancel
        </Button>
        <Button variant="danger" size="sm" disabled={!ready} onClick={onConfirm}>
          {confirmLabel}
        </Button>
      </div>
    </div>
  );
}
