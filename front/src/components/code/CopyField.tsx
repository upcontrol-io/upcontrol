import { CopyButton } from './CopyButton';
import styles from './CopyField.module.css';

export interface CopyFieldProps {
  /** What lands in the clipboard. */
  text: string;
  /** What the field shows — defaults to `text` (e.g. a URL without its scheme). */
  display?: string;
  className?: string;
  /** Fires after the value went to the clipboard (see CopyButton.onCopied). */
  onCopied?: () => void;
}

/**
 * A read-only value in a field with its copy control inside the right edge
 * (user decision, Aug 14, 2026). One grammar for every copyable thing — a
 * webhook URL, an install command, a public status address: a bordered button
 * next to a bordered field read as two fields, and a blob with one big Copy
 * under it made the reader copy directions along with the command.
 */
export function CopyField({ text, display, className, onCopied }: CopyFieldProps) {
  return (
    <div className={[styles.field, className].filter(Boolean).join(' ')}>
      <code className={styles.value}>{display ?? text}</code>
      <CopyButton text={text} className={styles.copy} onCopied={onCopied} />
    </div>
  );
}
