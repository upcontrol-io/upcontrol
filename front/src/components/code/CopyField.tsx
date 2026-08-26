import { CopyButton } from './CopyButton';
import styles from './CopyField.module.css';

interface CopyFieldProps {
  /** What lands in the clipboard. */
  text: string;
  /** What the field shows — defaults to `text` (e.g. a URL without its scheme). */
  display?: string;
  className?: string;
  /** Fires after the value went to the clipboard (see CopyButton.onCopied). */
  onCopied?: () => void;
}

/** A read-only value in a field with the copy control inside its right edge:
 *  one grammar for every copyable thing, button inside the field. */
export function CopyField({ text, display, className, onCopied }: CopyFieldProps) {
  return (
    <div className={[styles.field, className].filter(Boolean).join(' ')}>
      <code className={styles.value}>{display ?? text}</code>
      <CopyButton text={text} className={styles.copy} onCopied={onCopied} />
    </div>
  );
}
