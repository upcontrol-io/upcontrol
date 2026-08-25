import { type ReactNode } from 'react';
import styles from './LogMessage.module.css';

/** A log line with its JSON coloured: a span, not CodeBlock, but the same
 *  colour tokens; only the JSON span is tokenised, never the prose. */

/** The outermost balanced {...} in the line, or null: brace counting (a lazy
 *  regex stops at the first inner brace), quote-aware. */
function findJsonSpan(text: string): [number, number] | null {
  const start = text.indexOf('{');
  if (start === -1) return null;
  let depth = 0;
  let inString = false;
  for (let i = start; i < text.length; i++) {
    const ch = text[i];
    if (inString) {
      if (ch === '\\') i++;
      else if (ch === '"') inString = false;
      continue;
    }
    if (ch === '"') inString = true;
    else if (ch === '{') depth++;
    else if (ch === '}') {
      depth--;
      if (depth === 0) return [start, i + 1];
    }
  }
  return null;
}

/** Splits JSON source into coloured spans. A key is a string followed by a colon,
 *  which is the only thing that separates it from a string value. */
function tokenizeJson(source: string): ReactNode[] {
  const out: ReactNode[] = [];
  // One pass: strings (with the following colon captured to tell key from value),
  // numbers, literals, everything else falls through as punctuation.
  const re = /("(?:[^"\\]|\\.)*")(\s*:)?|(-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?)|\b(true|false|null)\b/g;
  let last = 0;
  let match: RegExpExecArray | null;
  let index = 0;

  while ((match = re.exec(source))) {
    if (match.index > last) {
      out.push(
        <span key={index++} className={styles.punct}>
          {source.slice(last, match.index)}
        </span>,
      );
    }
    if (match[1] !== undefined) {
      const isKey = match[2] !== undefined;
      out.push(
        <span key={index++} className={isKey ? styles.key : styles.string}>
          {match[1]}
        </span>,
      );
      if (isKey) {
        out.push(
          <span key={index++} className={styles.punct}>
            {match[2]}
          </span>,
        );
      }
    } else if (match[3] !== undefined) {
      out.push(
        <span key={index++} className={styles.number}>
          {match[3]}
        </span>,
      );
    } else {
      out.push(
        <span key={index++} className={styles.literal}>
          {match[4]}
        </span>,
      );
    }
    last = re.lastIndex;
  }

  if (last < source.length) {
    out.push(
      <span key={index++} className={styles.punct}>
        {source.slice(last)}
      </span>,
    );
  }
  return out;
}

export function LogMessage({ text }: { text: string }) {
  const span = findJsonSpan(text);
  if (!span) return <>{text}</>;
  const [start, end] = span;
  return (
    <>
      {text.slice(0, start)}
      {tokenizeJson(text.slice(start, end))}
      {text.slice(end)}
    </>
  );
}
