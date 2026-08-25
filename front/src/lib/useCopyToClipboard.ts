import { useCallback, useEffect, useRef, useState } from 'react';

const REVERT_DELAY_MS = 2000;

/** Shared "copy, flash a checkmark for 2s, revert" for every copy button. */
export function useCopyToClipboard(): [copied: boolean, copy: (text: string) => void] {
  const [copied, setCopied] = useState(false);
  const timeoutRef = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);

  useEffect(() => () => clearTimeout(timeoutRef.current), []);

  const copy = useCallback((text: string) => {
    void navigator.clipboard.writeText(text);
    setCopied(true);
    clearTimeout(timeoutRef.current);
    timeoutRef.current = setTimeout(() => setCopied(false), REVERT_DELAY_MS);
  }, []);

  return [copied, copy];
}
