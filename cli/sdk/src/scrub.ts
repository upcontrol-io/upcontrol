// Scrubs secrets into typed markers before the wire; hand-written because regex backtracking
// is catastrophic on multi-MB log lines. The server scrubs again as a second layer.

interface ScrubResult {
  cleaned: string;
  counts: Record<string, number>;
}

const B64URL = (c: number) =>
  (c >= 48 && c <= 57) || (c >= 65 && c <= 90) || (c >= 97 && c <= 122) || c === 45 || c === 95;
const B64 = (c: number) => B64URL(c) || c === 43 || c === 47 || c === 61;
const DIGIT = (c: number) => c >= 48 && c <= 57;
const ALNUM = (c: number) =>
  (c >= 48 && c <= 57) || (c >= 65 && c <= 90) || (c >= 97 && c <= 122);
const UPNUM = (c: number) => (c >= 48 && c <= 57) || (c >= 65 && c <= 90);

// Known secret prefixes: everything after the prefix up to a boundary is the
// secret. Order matters only for overlapping prefixes (longest first).
const PREFIXES: Array<[string, string]> = [
  ['github_pat_', 'github-token'],
  ['sk_live_', 'stripe-key'],
  ['sk_test_', 'stripe-key'],
  ['rk_live_', 'stripe-key'],
  ['whsec_', 'stripe-key'],
  ['ghp_', 'github-token'],
  ['gho_', 'github-token'],
  ['ghu_', 'github-token'],
  ['ghs_', 'github-token'],
  ['xoxb-', 'slack-token'],
  ['xoxp-', 'slack-token'],
  ['xapp-', 'slack-token'],
  ['AIza', 'gcp-key'],
  ['uc_live_', 'upcontrol-key'],
];

function isBoundary(c: number): boolean {
  return !(ALNUM(c) || c === 45 || c === 95);
}

function luhn(digits: string): boolean {
  let sum = 0;
  let dbl = false;
  for (let i = digits.length - 1; i >= 0; i--) {
    let d = digits.charCodeAt(i) - 48;
    if (dbl) {
      d *= 2;
      if (d > 9) d -= 9;
    }
    sum += d;
    dbl = !dbl;
  }
  return sum % 10 === 0;
}

interface Hit {
  start: number;
  end: number;
  type: string;
}

// scrub scans s once per detector family and returns the cleaned string plus
// per-type counts. Overlapping hits are resolved leftmost-longest.
export function scrub(s: string): ScrubResult {
  if (s.length === 0) return { cleaned: s, counts: {} };
  const hits: Hit[] = [];

  scanPem(s, hits);
  scanPrefixes(s, hits);
  scanAwsKey(s, hits);
  scanBearer(s, hits);
  scanJwt(s, hits);
  scanDbUrl(s, hits);
  scanCookies(s, hits);
  scanCards(s, hits);
  scanEmails(s, hits);

  if (hits.length === 0) return { cleaned: s, counts: {} };

  hits.sort((a, b) => a.start - b.start || b.end - a.end);
  const counts: Record<string, number> = {};
  let out = '';
  let pos = 0;
  for (const h of hits) {
    if (h.start < pos) continue; // swallowed by an earlier, longer hit
    out += s.slice(pos, h.start) + `[redacted:${h.type}:${h.end - h.start}]`;
    counts[h.type] = (counts[h.type] ?? 0) + 1;
    pos = h.end;
  }
  out += s.slice(pos);
  return { cleaned: out, counts };
}

function scanPem(s: string, hits: Hit[]): void {
  let i = 0;
  for (;;) {
    const start = s.indexOf('-----BEGIN ', i);
    if (start < 0) return;
    const endMark = s.indexOf('-----END ', start);
    if (endMark < 0) {
      hits.push({ start, end: s.length, type: 'pem' });
      return;
    }
    const close = s.indexOf('-----', endMark + 9);
    const end = close < 0 ? s.length : close + 5;
    hits.push({ start, end, type: 'pem' });
    i = end;
  }
}

function scanPrefixes(s: string, hits: Hit[]): void {
  for (const [prefix, type] of PREFIXES) {
    let i = 0;
    for (;;) {
      const start = s.indexOf(prefix, i);
      if (start < 0) break;
      if (start > 0 && !isBoundary(s.charCodeAt(start - 1))) {
        i = start + prefix.length;
        continue;
      }
      let end = start + prefix.length;
      while (end < s.length && !isBoundary(s.charCodeAt(end))) end++;
      // A bare prefix with nothing after it is prose, not a secret.
      if (end - start >= prefix.length + 8) hits.push({ start, end, type });
      i = end;
    }
  }
}

function scanAwsKey(s: string, hits: Hit[]): void {
  let i = 0;
  for (;;) {
    const start = s.indexOf('AKIA', i);
    if (start < 0) return;
    if (start > 0 && !isBoundary(s.charCodeAt(start - 1))) {
      i = start + 4;
      continue;
    }
    let end = start + 4;
    while (end < s.length && UPNUM(s.charCodeAt(end))) end++;
    if (end - start === 20) hits.push({ start, end, type: 'aws-key' });
    i = end;
  }
}

function scanBearer(s: string, hits: Hit[]): void {
  let i = 0;
  for (;;) {
    const start = s.indexOf('Bearer ', i);
    if (start < 0) return;
    let t = start + 7;
    while (t < s.length && s.charCodeAt(t) === 32) t++;
    let end = t;
    while (end < s.length && (B64(s.charCodeAt(end)) || s.charCodeAt(end) === 46)) end++;
    if (end - t >= 16) hits.push({ start: t, end, type: 'bearer' });
    i = end > start + 7 ? end : start + 7;
  }
}

function scanJwt(s: string, hits: Hit[]): void {
  let i = 0;
  for (;;) {
    const start = s.indexOf('eyJ', i);
    if (start < 0) return;
    if (start > 0 && !isBoundary(s.charCodeAt(start - 1)) && s.charCodeAt(start - 1) !== 46) {
      i = start + 3;
      continue;
    }
    let p = start;
    let segments = 0;
    let end = start;
    for (;;) {
      let segEnd = p;
      while (segEnd < s.length && B64URL(s.charCodeAt(segEnd))) segEnd++;
      if (segEnd - p < 8) break;
      segments++;
      end = segEnd;
      if (segEnd < s.length && s.charCodeAt(segEnd) === 46) p = segEnd + 1;
      else break;
    }
    if (segments >= 3) hits.push({ start, end, type: 'jwt' });
    i = Math.max(end, start + 3);
  }
}

// scheme://user:password@host - redact the password span only, so the line
// stays diagnosable (host and user survive).
function scanDbUrl(s: string, hits: Hit[]): void {
  let i = 0;
  for (;;) {
    const scheme = s.indexOf('://', i);
    if (scheme < 0) return;
    const at = s.indexOf('@', scheme + 3);
    if (at < 0) {
      i = scheme + 3;
      continue;
    }
    const colon = s.indexOf(':', scheme + 3);
    if (colon > scheme && colon < at) {
      let ok = true;
      for (let j = scheme + 3; j < at; j++) {
        const c = s.charCodeAt(j);
        if (c === 32 || c === 9 || c === 10 || c === 47) {
          ok = false;
          break;
        }
      }
      if (ok && at - colon > 1) hits.push({ start: colon + 1, end: at, type: 'db-password' });
    }
    i = at + 1;
  }
}

function scanCookies(s: string, hits: Hit[]): void {
  for (const marker of ['session=', 'Set-Cookie:', 'set-cookie:']) {
    let i = 0;
    for (;;) {
      const start = s.indexOf(marker, i);
      if (start < 0) break;
      let v = start + marker.length;
      while (v < s.length && s.charCodeAt(v) === 32) v++;
      let end = v;
      while (end < s.length) {
        const c = s.charCodeAt(end);
        if (c === 59 || c === 10 || c === 13 || c === 32 || c === 34 || c === 44) break;
        end++;
      }
      if (end - v >= 8) hits.push({ start: v, end, type: 'cookie' });
      i = Math.max(end, start + marker.length);
    }
  }
}

// Card numbers: 13-19 digits with uniform separator grouping, passing Luhn,
// so timestamps and IDs do not false-positive.
function scanCards(s: string, hits: Hit[]): void {
  let i = 0;
  const n = s.length;
  while (i < n) {
    if (!DIGIT(s.charCodeAt(i))) {
      i++;
      continue;
    }
    if (i > 0) {
      const prev = s.charCodeAt(i - 1);
      if (!isBoundary(prev) || prev === 46) {
        while (i < n && DIGIT(s.charCodeAt(i))) i++;
        continue;
      }
    }
    let j = i;
    let digits = '';
    let lastWasSep = false;
    while (j < n) {
      const c = s.charCodeAt(j);
      if (DIGIT(c)) {
        digits += s[j];
        lastWasSep = false;
        j++;
      } else if ((c === 32 || c === 45) && !lastWasSep && digits.length > 0 && j + 1 < n && DIGIT(s.charCodeAt(j + 1))) {
        lastWasSep = true;
        j++;
      } else break;
    }
    const after = j < n ? s.charCodeAt(j) : 0;
    if (digits.length >= 13 && digits.length <= 19 && (j >= n || (isBoundary(after) && after !== 46)) && luhn(digits)) {
      hits.push({ start: i, end: j, type: 'card' });
    }
    i = j > i ? j : i + 1;
  }
}

// An `@` inside a URL authority is userinfo, not an email: the db-password
// scanner owns that span, and mislabeling it would swallow the host too.
function isUrlUserinfo(s: string, start: number): boolean {
  for (let j = start - 1; j >= 0 && start - j <= 80; j--) {
    const c = s.charCodeAt(j);
    if (c === 32 || c === 9 || c === 10 || c === 13) return false;
    if (c === 47 && j >= 2 && s.charCodeAt(j - 1) === 47 && s.charCodeAt(j - 2) === 58) return true;
  }
  return false;
}

function scanEmails(s: string, hits: Hit[]): void {
  let i = 0;
  for (;;) {
    const at = s.indexOf('@', i);
    if (at <= 0) return;
    let start = at;
    while (start > 0) {
      const c = s.charCodeAt(start - 1);
      if (ALNUM(c) || c === 46 || c === 45 || c === 95 || c === 43) start--;
      else break;
    }
    let end = at + 1;
    let lastDot = -1;
    while (end < s.length) {
      const c = s.charCodeAt(end);
      if (ALNUM(c) || c === 45) end++;
      else if (c === 46 && end + 1 < s.length && ALNUM(s.charCodeAt(end + 1))) {
        lastDot = end;
        end++;
      } else break;
    }
    if (start < at && lastDot > at && end - lastDot > 2 && !isUrlUserinfo(s, start)) {
      hits.push({ start, end, type: 'email' });
    }
    i = at + 1;
  }
}
