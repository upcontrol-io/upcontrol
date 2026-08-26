import { type ReactNode } from 'react';
import { createPortal } from 'react-dom';
import { IconButton } from './IconButton';
import { CloseIcon } from '@/icons';
import { useDismissible } from '@/lib/useDismissible';
import styles from './Modal.module.css';

interface ModalProps {
  open: boolean;
  onClose: () => void;
  title: string;
  children: ReactNode;
}

/** Reserved for destructive confirmations and the Upgrade modal, not
 *  general-purpose chrome. */
export function Modal({ open, onClose, title, children }: ModalProps) {
  useDismissible(open, onClose);

  if (!open) return null;

  return createPortal(
    <div className={styles.overlay} onClick={onClose}>
      <div
        className={styles.panel}
        style={{ maxWidth: 480 }}
        role="dialog"
        aria-modal="true"
        aria-label={title}
        onClick={(event) => event.stopPropagation()}
      >
        <div className={styles.header}>
          <h3 className={styles.title}>{title}</h3>
          <IconButton aria-label="Close" icon={<CloseIcon width={14} height={14} />} size="sm" onClick={onClose} />
        </div>
        {children}
      </div>
    </div>,
    document.body,
  );
}
