// Who a feed counts: the identity machinery every feed shares. A string id is the caller's
// own stable identifier and passes through as `uc.actor`; a request is fingerprinted from
// its address and user-agent with a per-process salt, so a raw address never reaches the
// wire. The counting is the server's now — it reads DISTINCT actors off the events it
// stores — so nothing is deduped here, the salt need not survive a restart, and there is
// no state file.

import { createHash, randomBytes } from 'node:crypto';

type HeaderBag = { get(name: string): string | null } | Record<string, string | string[] | undefined>;

/** The shape every Node request has: `http.IncomingMessage`, Express's `req` (`ip`), a
 *  fetch `Request` (`headers.get`). Only the address and the user-agent are read. */
export interface RequestLike {
  headers: HeaderBag;
  socket?: { remoteAddress?: string };
  ip?: string;
}

export type Who = string | RequestLike;

/** What a feed needs: the shared client its events go through. */
export interface FeedDeps {
  client: { enqueue(fields: Record<string, unknown>, level: string): void; service?: string };
}

const warned = new Set<string>();

export function warnOnce(key: string, text: string): void {
  if (warned.has(key)) return;
  warned.add(key);
  process.stderr.write(text + '\n');
}

// Per-process and unpersisted on purpose: it exists only so a raw address never leaves, and
// nothing local is deduped against it.
const SALT = randomBytes(16).toString('hex');

function header(bag: HeaderBag, name: string): string {
  const get = (bag as { get?: unknown }).get;
  const value =
    typeof get === 'function'
      ? (bag as { get(n: string): string | null }).get(name)
      : (bag as Record<string, string | string[] | undefined>)[name];
  const one = Array.isArray(value) ? value[0] : value;
  return typeof one === 'string' ? one.trim() : '';
}

function hash(value: string): string {
  return createHash('sha256').update(SALT + '\n' + value).digest('hex').slice(0, 16);
}

function fingerprint(who: RequestLike): string | null {
  // The framework's own address first (Express, Fastify and Koa apply the app's trust-proxy
  // policy to it), then the proxy header a bare Node request behind a proxy has nothing
  // else for, then the socket. A forged x-forwarded-for miscounts a feed and reaches
  // nothing else.
  const ip =
    (typeof who.ip === 'string' ? who.ip.trim() : '') ||
    header(who.headers, 'x-forwarded-for').split(',')[0].trim() ||
    who.socket?.remoteAddress?.trim() ||
    '';
  if (ip === '') {
    // Without an address every visitor on one browser would be one person, and that number
    // is the denominator of every share the feeds draw.
    warnOnce(
      'noaddress',
      "upcontrol: a feed got a request with no address; it is not counted (pass the framework's request, or set the proxy's x-forwarded-for)",
    );
    return null;
  }
  const ua = header(who.headers, 'user-agent');
  if (/bot|crawl|spider|slurp|headless/i.test(ua)) return null;
  return hash(ip + '\n' + ua);
}

/** The `uc.actor` for a Who: the caller's string as-is, a request as its fingerprint, and
 *  null when there is no person to name (there is then nothing to send). */
export function actor(who: Who | null | undefined): string | null {
  if (typeof who === 'string') {
    const value = who.trim();
    return value === '' ? null : value;
  }
  if (typeof who === 'object' && who !== null && who.headers) return fingerprint(who);
  return null;
}
