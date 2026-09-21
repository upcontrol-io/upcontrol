// The CLI's API calls. The endpoint default is the product origin; the
// env var and --endpoint exist for self-hosted stacks and local development.

const DEFAULT_ENDPOINT = 'https://upcontrol.io';
export const CLI_VERSION = '0.4.0';

export function endpointFrom(env: NodeJS.ProcessEnv, flag?: string): string {
  return (flag || env.UPCONTROL_ENDPOINT || DEFAULT_ENDPOINT).replace(/\/+$/, '');
}

interface MintResult {
  ok: boolean;
  status?: number;
  key?: string;
  claimUrl?: string;
}

export async function mintAnonymousProject(endpoint: string, agent: string | null): Promise<MintResult> {
  try {
    const res = await request(endpoint + '/v1/projects/anonymous', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        cli_version: CLI_VERSION,
        agent_version: agent ?? '',
        platform: process.platform,
        arch: process.arch,
      }),
    });
    if (!res.ok) return { ok: false, status: res.status };
    const body = parseJSON<{ key?: string; claimUrl?: string }>(res.text);
    if (!body?.key) return { ok: false, status: res.status };
    return { ok: true, key: body.key, claimUrl: body.claimUrl };
  } catch {
    return { ok: false };
  }
}

interface RedeemResult {
  ok: boolean;
  // The status separates a throttle from a dead token: a 429 leaves the
  // one-time token unburned, so the same command works again in a moment.
  status?: number;
  key?: string;
  error?: string;
}

// redeemInstallToken burns the one-time dashboard token and receives the
// project key exactly once; the caller writes it to .env, never prints it.
export async function redeemInstallToken(endpoint: string, token: string): Promise<RedeemResult> {
  try {
    const res = await request(endpoint + '/v1/install/redeem', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ token }),
    });
    if (!res.ok) return { ok: false, status: res.status, error: 'refused' };
    const body = parseJSON<{ key?: string }>(res.text);
    if (!body?.key) return { ok: false, error: 'malformed' };
    return { ok: true, key: body.key };
  } catch {
    return { ok: false, error: 'unreachable' };
  }
}

interface InstallStatus {
  ok: boolean;
  status?: number;
  verified?: boolean;
  verifiedAt?: string;
  lines?: number;
  web?: { views: number; lastAt?: string };
  recent?: Array<{ name: string; count: number; lastAt: string }>;
  error?: string;
}

export async function fetchInstallStatus(endpoint: string, key: string): Promise<InstallStatus> {
  try {
    const res = await request(endpoint + '/v1/install/status', {
      headers: { 'X-Upcontrol-Key': key },
    });
    if (!res.ok) return { ok: false, status: res.status, error: 'refused' };
    const body = parseJSON<Omit<InstallStatus, 'ok' | 'status' | 'error'>>(res.text);
    if (!body) return { ok: false, status: res.status, error: 'malformed' };
    return { ...body, ok: true };
  } catch {
    return { ok: false, error: 'unreachable' };
  }
}

interface MintKeyResult {
  ok: boolean;
  status?: number;
  value?: string;
  code?: string;
  error?: string;
}

// mintPublicKey issues the origin-bound PUBLIC web key with the SECRET key in
// the header: that is how `npx upcontrol web` puts the tag on a site without
// anyone opening the app. `value` is the full uc_pub_ key, the one place it
// ever appears; a refusal carries its error code so the caller can word the fix.
export async function mintPublicKey(endpoint: string, key: string, name: string, origins: string[]): Promise<MintKeyResult> {
  try {
    const res = await request(endpoint + '/v1/keys', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', 'X-Upcontrol-Key': key },
      body: JSON.stringify({ name, kind: 'public', origins }),
    });
    if (!res.ok) {
      const code = parseJSON<{ error?: { code?: string } }>(res.text)?.error?.code;
      return { ok: false, status: res.status, code };
    }
    const body = parseJSON<{ value?: string }>(res.text);
    if (!body?.value) return { ok: false, status: res.status };
    return { ok: true, value: body.value };
  } catch {
    return { ok: false, error: 'unreachable' };
  }
}

// The board's error body names exactly what is wrong with a layout
// (`bad_layout` and a sentence); the status alone would cost a round trip. The
// code comes back too: a 404 that carries one is a board this core does not
// have, while a 404 with no error object at all is a core that has no
// /v1/dashboards to answer with.
function boardError(text: string): { code?: string; message?: string } {
  return parseJSON<{ error?: { code?: string; message?: string } }>(text)?.error ?? {};
}

/** One shape for every board door: they differ by one field each, not by kind.
 *  `status` separates 200 stored from 202 proposed; `text` is the read's body,
 *  `total` the widget count after an append and `board` the id a create or a
 *  proposal named. */
interface BoardResult {
  ok: boolean;
  status?: number;
  text?: string;
  total?: number;
  board?: string;
  code?: string;
  message?: string;
  error?: string;
}

// Every board call is the same shape: the key header, the server's own sentence
// carried back on a refusal, and an unreachable endpoint answered rather than thrown.
async function boardCall(url: string, key: string, init?: RequestInit): Promise<BoardResult> {
  const headers: Record<string, string> = { 'X-Upcontrol-Key': key };
  if (init?.body) headers['Content-Type'] = 'application/json';
  try {
    const res = await request(url, { ...init, headers });
    if (!res.ok) return { ok: false, status: res.status, ...boardError(res.text) };
    return { ok: true, status: res.status, text: res.text };
  } catch {
    return { ok: false, error: 'unreachable' };
  }
}

// Without a board named, the legacy path: it is the one a core older than
// several boards answers, and on a new core it is the alias of the first board.
function boardPath(board: string | undefined, suffix = ''): string {
  return board ? `/v1/dashboards/${encodeURIComponent(board)}${suffix}` : `/v1/dashboard${suffix}`;
}

export function listBoards(endpoint: string, key: string): Promise<BoardResult> {
  return boardCall(endpoint + '/v1/dashboards', key);
}

// The list read is the only way a NAME becomes an id; a body that is not a list
// is an empty list, and the caller says the name was not found.
export function boardsFrom(text: string | undefined): Array<{ id: string; name: string }> {
  return parseJSON<{ boards?: Array<{ id: string; name: string }> }>(text ?? '')?.boards ?? [];
}

export async function createBoard(endpoint: string, key: string, name: string, layout?: unknown): Promise<BoardResult> {
  const r = await boardCall(endpoint + '/v1/dashboards', key, {
    method: 'POST',
    body: JSON.stringify(layout === undefined ? { name } : { name, layout }),
  });
  if (!r.ok) return r;
  const body = parseJSON<{ id?: string }>(r.text ?? '');
  if (!body?.id) return { ok: false, status: r.status, error: 'malformed' };
  return { ...r, board: body.id };
}

export function readBoard(endpoint: string, key: string, board?: string): Promise<BoardResult> {
  return boardCall(endpoint + boardPath(board), key);
}

export async function applyBoard(endpoint: string, key: string, layout: unknown, board?: string): Promise<BoardResult> {
  const r = await boardCall(endpoint + boardPath(board), key, {
    method: 'PUT',
    body: JSON.stringify(layout),
  });
  // A proposal names the board it waits on, which a caller that addressed the
  // alias does not otherwise know; a core older than several boards sends none.
  if (r.ok && r.status === 202) return { ...r, board: parseJSON<{ id?: string }>(r.text ?? '')?.id };
  return r;
}

export async function appendBoard(endpoint: string, key: string, widgets: unknown[], board?: string): Promise<BoardResult> {
  const r = await boardCall(endpoint + boardPath(board, '/widgets'), key, {
    method: 'POST',
    body: JSON.stringify({ widgets }),
  });
  if (!r.ok) return r;
  const body = parseJSON<{ widgets?: unknown[] }>(r.text ?? '');
  if (!body?.widgets) return { ok: false, status: r.status, error: 'malformed' };
  return { ...r, total: body.widgets.length };
}
interface HttpAnswer {
  ok: boolean;
  status: number;
  text: string;
}

// One timeout for every call: init, status and redeem all wait the same.
const TIMEOUT_MS = 10_000;

async function request(url: string, init: RequestInit): Promise<HttpAnswer> {
  const ctrl = new AbortController();
  const kill = setTimeout(() => ctrl.abort(), TIMEOUT_MS);
  (kill as { unref?: () => void }).unref?.();
  try {
    const res = await fetch(url, {
      ...init,
      // A pooled connection survives the last response as a live handle the
      // runtime waits on; with exitCode in main.ts this is what keeps exit fast.
      headers: { connection: 'close', ...init.headers },
      signal: ctrl.signal,
    });
    // Always, on every status: an unread body is the handle that crashes the
    // teardown. A body that fails mid-read is still a status we can report.
    let text = '';
    try {
      text = await res.text();
    } catch {
      text = '';
    }
    return { ok: res.ok, status: res.status, text };
  } finally {
    clearTimeout(kill);
  }
}

// parseJSON never throws: a body that is not JSON is the same fact as a body
// that is missing a field: the caller reports "malformed" either way.
function parseJSON<T>(text: string): T | null {
  try {
    return JSON.parse(text) as T;
  } catch {
    return null;
  }
}
