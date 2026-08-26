import type { ReactNode } from 'react';
import { CopyButton } from './CopyButton';
import styles from './CodeBlock.module.css';

interface CodeBlockProps {
  code: string;
  language?: string;
  showLineNumbers?: boolean;
  className?: string;
  /** Rendered inside a container that already draws the frame and carries its
   *  own copy button (CodeTabs' shared header bar). */
  embedded?: boolean;
}

interface Grammar {
  comment: RegExp;
  string: RegExp;
  kw: RegExp;
}

// Muted, grammar-aware highlighting: comments faint, strings recede, keywords
// weighted; achromatic on purpose: saturated color is reserved for status.
const GRAMMARS: Record<string, Grammar> = {
  cURL: { comment: /#.*/, string: /"[^"]*"|'[^']*'/, kw: /\b(curl|export|if|then|fi|echo|set)\b/ },
};

const KEY_RE = /uc_live_[A-Za-z0-9]+|<YOUR_KEY>|https?:\/\/[^\s"'\\]+/;
const NUM_RE = /\b\d+\b/;

const TOKEN_CLASS: Record<string, string> = {
  comment: styles.tokComment,
  key: styles.tokKey,
  string: styles.tokString,
  kw: styles.tokKw,
  num: styles.tokNum,
};

function highlightLine(line: string, grammar: Grammar): ReactNode[] {
  const order: [string, RegExp][] = [
    ['comment', grammar.comment],
    ['key', KEY_RE],
    ['string', grammar.string],
    ['kw', grammar.kw],
    ['num', NUM_RE],
  ];
  const out: ReactNode[] = [];
  let rest = line;
  let guard = 0;
  while (rest.length && guard++ < 400) {
    let best: { type: string; index: number; text: string } | null = null;
    for (const [type, re] of order) {
      const match = rest.match(re);
      if (match && match.index != null && (!best || match.index < best.index)) {
        best = { type, index: match.index, text: match[0] };
      }
    }
    if (!best) {
      out.push(rest);
      break;
    }
    if (best.index > 0) out.push(rest.slice(0, best.index));
    out.push(
      <span key={out.length} className={TOKEN_CLASS[best.type]}>
        {best.text}
      </span>,
    );
    rest = rest.slice(best.index + best.text.length);
  }
  return out;
}

/** Achromatic grammar highlighting: saturated color is reserved for status. */
export function CodeBlock({
  code,
  language,
  showLineNumbers = true,
  className,
  embedded = false,
}: CodeBlockProps) {
  const lines = code.split('\n');
  const grammar = GRAMMARS[language ?? ''] ?? GRAMMARS.cURL;

  return (
    <div className={[styles.wrap, embedded && styles.embedded, className].filter(Boolean).join(' ')}>
      {!embedded && <CopyButton text={code} className={styles.copy} />}
      <div className={styles.body}>
        {showLineNumbers && (
          <div className={styles.gutter} aria-hidden="true">
            {lines.map((_, index) => (
              <div key={index}>{index + 1}</div>
            ))}
          </div>
        )}
        <pre className={styles.pre}>
          {lines.map((line, index) => {
            const tokens = highlightLine(line, grammar);
            return (
              <div key={index} className={styles.line}>
                {tokens.length ? tokens : ' '}
              </div>
            );
          })}
        </pre>
      </div>
    </div>
  );
}
