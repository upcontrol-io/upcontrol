// The CLI's API calls. The endpoint default is the product origin; the
// env var and --endpoint exist for self-hosted stacks and local development.

const DEFAULT_ENDPOINT = 'https://upcontrol.io';
export const CLI_VERSION = '0.1.5';

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
    if (!res.ok) return { ok: false, error: 'refused' };
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

// One answer, body already consumed: a live Response can be dropped unread,
// which leaks an undici handle and aborts the process during teardown.
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
