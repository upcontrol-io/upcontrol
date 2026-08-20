import { type ReactNode } from 'react';
import styles from './LogMessage.module.css';

/**
 * A log line, with any JSON in it coloured so it can be read at a glance.
 *
 * Structured loggers emit an object per line, and unhighlighted that is a wall
 * of quotes and braces where the eye cannot find the one field that matters.
 * Keys, strings, numbers and literals each get a tone, punctuation drops back.
 *
 * Deliberately not `CodeBlock`: that renders a `<pre>` with a gutter and a copy
 * button, which is a block, and this has to be a `<span>` inside a 20px grid row
 * next to a timestamp. The *colours* are the shared part and they come from the
 * same tokens (`--accent`, `--code-string`, `--code-number`, `--code-comment`),
 * so the two stay in step without sharing a renderer.
 *
 * Only the JSON span is tokenised. A line is usually `some text {json}` or plain
 * prose, and the prose half must not be syntax-coloured.
 */

/** The outermost balanced {...} in the line, or null. Brace counting rather than
 *  a regex because nested objects (`"pool":{"in_use":20}`) are the common case
 *  and a lazy regex stops at the first inner brace. Quote-aware, so a brace
 *  inside a string value cannot unbalance it. */
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

/** True when the line carries JSON worth colouring — used to decide whether a
 *  row offers the pretty-print control. */
export function hasJson(text: string): boolean {
  return findJsonSpan(text) !== null;
}

/** Re-indents the JSON in a line. Falls back to the original text when the
 *  object does not actually parse, so a truncated line still shows something. */
export function prettyJson(text: string): string {
  const span = findJsonSpan(text);
  if (!span) return text;
  const [start, end] = span;
  try {
    const parsed: unknown = JSON.parse(text.slice(start, end));
    return text.slice(0, start) + JSON.stringify(parsed, null, 2) + text.slice(end);
  } catch {
    return text;
  }
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
