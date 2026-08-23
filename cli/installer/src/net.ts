// The CLI's API calls. The endpoint default is the product origin; the
// env var and --endpoint exist for self-hosted stacks and local development.

import type { ProjectSpec } from './meta.js';

export const DEFAULT_ENDPOINT = 'https://upcontrol.io';
export const CLI_VERSION = '0.1.2';

export function endpointFrom(env: NodeJS.ProcessEnv, flag?: string): string {
  return (flag || env.UPCONTROL_ENDPOINT || DEFAULT_ENDPOINT).replace(/\/+$/, '');
}

export interface MintResult {
  ok: boolean;
  status?: number;
  key?: string;
  claimUrl?: string;
  projectId?: string;
  error?: string;
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
    if (!res.ok) return { ok: false, status: res.status, error: 'mint_refused' };
    const body = parseJSON<{ key?: string; claimUrl?: string; projectId?: string }>(res.text);
    if (!body?.key) return { ok: false, status: res.status, error: 'mint_malformed' };
    return { ok: true, key: body.key, claimUrl: body.claimUrl, projectId: body.projectId };
  } catch {
    return { ok: false, error: 'unreachable' };
  }
}

export interface RedeemResult {
  ok: boolean;
  status?: number;
  key?: string;
  error?: string;
}

// redeemInstallToken burns the one-time token the dashboard generated and
// receives the project key exactly once. The caller writes it to .env and
// never prints it.
export async function redeemInstallToken(endpoint: string, token: string): Promise<RedeemResult> {
  try {
    const res = await request(endpoint + '/v1/install/redeem', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ token }),
    });
    if (!res.ok) return { ok: false, status: res.status, error: 'refused' };
    const body = parseJSON<{ key?: string }>(res.text);
    if (!body?.key) return { ok: false, status: res.status, error: 'malformed' };
    return { ok: true, key: body.key };
  } catch {
    return { ok: false, error: 'unreachable' };
  }
}

export interface InstallStatus {
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

// META_TIMEOUT_MS is deliberately shorter than the 10s the other calls get.
// The spec upload is the one call whose result changes nothing the caller
// does, so a black-holed endpoint must not hold `init` open for ten seconds
// to deliver a value nobody reads.
const META_TIMEOUT_MS = 3_000;

// putProjectMeta uploads the installer-collected project spec (plan
// Decision 15b), key-authenticated like every other install call. Best
// effort by contract: the return value says whether the spec landed, and
// every caller is free to ignore it - a refused or unreachable upload must
// never fail the install.
export async function putProjectMeta(endpoint: string, key: string, spec: ProjectSpec): Promise<boolean> {
  try {
    const res = await request(
      endpoint + '/v1/project/meta',
      {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json', 'X-Upcontrol-Key': key },
        body: JSON.stringify(spec),
      },
      META_TIMEOUT_MS,
    );
    return res.ok;
  } catch {
    return false;
  }
}

// One answer, body already consumed. Returning a live Response was how the
// installer crashed: every caller that gave up on `!res.ok` returned without
// reading the body, undici kept the stream handle open, and Node tore the
// process down on top of it — `Assertion failed: !(handle->flags &
// UV_HANDLE_CLOSING)`, exit 0xC0000409, on the ordinary path where the server
// answers 400 with an error envelope. Reading the body inside the helper
// makes that unrepresentable: there is no undrained body to leak, whatever
// the caller decides to do with the status.
interface HttpAnswer {
  ok: boolean;
  status: number;
  text: string;
}

async function request(url: string, init: RequestInit, timeoutMs = 10_000): Promise<HttpAnswer> {
  const ctrl = new AbortController();
  const kill = setTimeout(() => ctrl.abort(), timeoutMs);
  (kill as { unref?: () => void }).unref?.();
  try {
    const res = await fetch(url, {
      ...init,
      // The CLI makes a handful of calls and exits, so a pooled connection has
      // nothing to be reused for - it only survives the last response as a live
      // handle the runtime then has to wait on. This line is what keeps init
      // fast, and it is NOT interchangeable with the process.exitCode fix in
      // main.ts: measured against a mock backend that refuses the upload,
      // exitCode alone exits cleanly but takes 5.7s (the socket's keep-alive
      // timeout), while both together exit in 0.14s. Removing either one
      // regresses a different half - crash, or stall.
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
// that is missing a field — the caller reports "malformed" either way.
function parseJSON<T>(text: string): T | null {
  try {
    return JSON.parse(text) as T;
  } catch {
    return null;
  }
}
