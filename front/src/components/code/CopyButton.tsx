import { Button, type ButtonSize } from '@/components/primitives';
import { CopyIcon, CheckIcon } from '@/icons';
import { useCopyToClipboard } from '@/lib/useCopyToClipboard';

export interface CopyButtonProps {
  text: string;
  size?: ButtonSize;
  className?: string;
  /** Idle-state label — e.g. "Copy selected". The confirmation stays "Copied!". */
  label?: string;
  /** Fires after the text went to the clipboard — for callers where copying
      means something beyond the clipboard (e.g. activating a hook). */
  onCopied?: () => void;
}

/** Copy → Copied! for 2s → revert. Every copy affordance in the product goes through this one component. */
export function CopyButton({ text, size = 'sm', className, label = 'Copy', onCopied }: CopyButtonProps) {
  const [copied, copy] = useCopyToClipboard();

  return (
    <Button
      size={size}
      variant="secondary"
      className={className}
      iconLeft={copied ? <CheckIcon width={13} height={13} /> : <CopyIcon width={13} height={13} />}
      onClick={() => {
        copy(text);
        onCopied?.();
      }}
    >
      {copied ? 'Copied!' : label}
    </Button>
  );
}
