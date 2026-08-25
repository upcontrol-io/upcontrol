import test from 'node:test';
import assert from 'node:assert/strict';
import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import { createServer } from 'node:http';
import { mkdtempSync, readFileSync, writeFileSync, existsSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';
import type { AddressInfo } from 'node:net';

interface MockServer {
  url: string;
  close: () => Promise<void>;
}

const execFileP = promisify(execFile);

// The whole init flow in agent mode against a mock mint server: .gitignore
// fixed before the key lands in .env, and the key never in stdout.

const here = dirname(fileURLToPath(import.meta.url));
const cli = join(here, '..', 'dist', 'main.js');
const KEY = 'uc_live_deadbeefdeadbeefdeadbeefdeadbeef';

function mockMint(): Promise<MockServer> {
  const server = createServer((req, res) => {
    if (req.url === '/v1/projects/anonymous') {
      res.writeHead(200, { 'content-type': 'application/json' });
      res.end(JSON.stringify({ projectId: 'prj_test', key: KEY, claimToken: 'tok', claimUrl: 'http://x/claim/tok' }));
      return;
    }
    if (req.url === '/v1/install/status') {
      res.writeHead(200, { 'content-type': 'application/json' });
      res.end(JSON.stringify({ verified: true, verifiedAt: '2026-08-15T00:00:00Z', lines: 3, recent: [] }));
      return;
    }
    res.writeHead(404);
    res.end();
  });
  return new Promise<MockServer>((resolve) => {
    server.listen(0, '127.0.0.1', () =>
      resolve({ url: `http://127.0.0.1:${(server.address() as AddressInfo).port}`,
        close: () => new Promise<void>((r) => server.close(() => r())) }),
    );
  });
}

// Async on purpose: the mock server lives in THIS process, so a synchronous
// spawn would block the event loop the server needs to answer the child.
async function runCli(
  cwd: string,
  args: string[],
  extraEnv: Record<string, string> = {},
): Promise<string> {
  const { stdout } = await execFileP(process.execPath, [cli, ...args], {
    cwd,
    encoding: 'utf8',
    env: { ...process.env, UPCONTROL_API_KEY: '', AI_AGENT: 'claude', ...extraEnv },
  });
  return stdout;
}

// For runs that are EXPECTED to exit non-zero: execFile rejects on those, and
// the stdout (with the JSON result line) rides on the rejection.
async function runCliFail(
  cwd: string,
  args: string[],
  extraEnv: Record<string, string> = {},
): Promise<{ stdout: string; code: number }> {
  try {
    const stdout = await runCli(cwd, args, extraEnv);
    return { stdout, code: 0 };
  } catch (e) {
    const err = e as { stdout?: string; code?: number };
    return { stdout: err.stdout ?? '', code: Number(err.code ?? 1) };
  }
}

test('init in agent mode: skill + dep + minted key, key never in stdout', async () => {
  const srv = await mockMint();
  const cwd = mkdtempSync(join(tmpdir(), 'uc-init-'));
  writeFileSync(join(cwd, 'package.json'), JSON.stringify({ name: 'fixture', version: '1.0.0' }, null, 2));
  writeFileSync(join(cwd, '.gitignore'), 'node_modules/\n');

  const stdout = await runCli(cwd, ['init', '--endpoint', srv.url]);
  await srv.close();

  assert.ok(!stdout.includes(KEY), 'THE KEY MUST NEVER APPEAR IN OUTPUT');
  const result = JSON.parse(stdout.trim().split('\n').pop()!);
  assert.equal(result.success, true);
  assert.equal(result.mode, 'agent');
  assert.equal(result.agent, 'claude-code');
  assert.equal(result.key.source, 'minted');
  assert.equal(result.key.claimUrl, 'http://x/claim/tok');
  assert.equal(result.sdk.added, true);

  assert.ok(existsSync(join(cwd, '.claude', 'skills', 'upcontrol', 'SKILL.md')));
  assert.ok(existsSync(join(cwd, '.agents', 'skills', 'upcontrol', 'SKILL.md')));
  assert.equal(
    JSON.parse(readFileSync(join(cwd, 'package.json'), 'utf8')).dependencies['@upcontrol/sdk'],
    '0.1.0',
  );
  const env = readFileSync(join(cwd, '.env'), 'utf8');
  assert.ok(env.includes('UPCONTROL_API_KEY=' + KEY));
  const gi = readFileSync(join(cwd, '.gitignore'), 'utf8');
  assert.ok(gi.split(/\r?\n/).includes('.env'), '.gitignore must cover .env before the key is written');

  rmSync(cwd, { recursive: true, force: true });
});

test('init with unreachable backend installs the skill but reports failure honestly', async () => {
  const cwd = mkdtempSync(join(tmpdir(), 'uc-init-'));
  const { stdout, code } = await runCliFail(cwd, ['init', '--endpoint', 'http://127.0.0.1:9']);
  const result = JSON.parse(stdout.trim().split('\n').pop()!);
  // A run that never got a key may not say success:true: an agent reading
  // that wires an app that silently sends nothing. The skill/SDK half lands.
  assert.equal(result.success, false);
  assert.equal(code, 1, 'a keyless init must exit 1');
  assert.equal(result.key.source, 'none');
  assert.match(result.key.note, /unreachable/);
  assert.ok(existsSync(join(cwd, '.claude', 'skills', 'upcontrol', 'SKILL.md')));
  assert.ok(!existsSync(join(cwd, '.env')), 'no key - no .env write');
  rmSync(cwd, { recursive: true, force: true });
});

test('skills lists topics and prints one', async () => {
  const cwd = mkdtempSync(join(tmpdir(), 'uc-skl-'));
  const list = await runCli(cwd, ['skills']);
  for (const t of ['dictionary', 'rules', 'logs', 'funnel', 'jobs', 'uptime', 'key', 'verify']) {
    assert.ok(list.includes(t), `topic ${t} missing from list`);
  }
  const dict = await runCli(cwd, ['skills', 'dictionary']);
  assert.ok(dict.includes('payment_succeeded'));
  assert.ok(dict.includes('install_verified'));
  rmSync(cwd, { recursive: true, force: true });
});

test('verify exits 0 against a verified backend and 2 with no key', async () => {
  const srv = await mockMint();
  const cwd = mkdtempSync(join(tmpdir(), 'uc-vfy-'));
  writeFileSync(join(cwd, '.env'), `UPCONTROL_API_KEY=${KEY}\n`);
  const okOut = await runCli(cwd, ['verify', '--endpoint', srv.url, '--json']);
  assert.equal(JSON.parse(okOut.trim()).verified, true);
  await srv.close();

  rmSync(join(cwd, '.env'));
  let code = 0;
  try {
    await runCli(cwd, ['verify', '--json']);
  } catch (e) {
    code = Number((e as { code?: unknown }).code);
  }
  assert.equal(code, 2, 'no key must exit 2');
  rmSync(cwd, { recursive: true, force: true });
});

test('status reports key source and skill freshness', async () => {
  const srv = await mockMint();
  const cwd = mkdtempSync(join(tmpdir(), 'uc-st-'));
  await runCli(cwd, ['init', '--endpoint', srv.url]);
  const st = JSON.parse((await runCli(cwd, ['status', '--endpoint', srv.url])).trim());
  await srv.close();
  assert.equal(st.keySource, 'dotenv');
  assert.equal(st.skillFresh, true);
  assert.equal(st.verified, true);
  rmSync(cwd, { recursive: true, force: true });
});

test('init --token redeems and writes .env; a spent token never falls back to minting', async () => {
  let redeems = 0;
  const srv = await mockMint();
  // extend the mock: first redeem succeeds, later ones 404 (single-use)
  const server2 = (await import('node:http')).createServer((req, res) => {
    if (req.url === '/v1/install/redeem') {
      redeems++;
      if (redeems === 1) {
        res.writeHead(200, { 'content-type': 'application/json' });
        res.end(JSON.stringify({ key: KEY }));
      } else {
        res.writeHead(404, { 'content-type': 'application/json' });
        res.end(JSON.stringify({ error: 'invalid_token' }));
      }
      return;
    }
    res.writeHead(404);
    res.end();
  });
  await new Promise<void>((r) => server2.listen(0, '127.0.0.1', () => r()));
  const url2 = `http://127.0.0.1:${(server2.address() as AddressInfo).port}`;
  await srv.close();

  const cwd = mkdtempSync(join(tmpdir(), 'uc-tok-'));
  writeFileSync(join(cwd, '.gitignore'), 'node_modules/\n');
  const out1 = await runCli(cwd, ['init', '--token', 'uct_testtoken', '--endpoint', url2]);
  assert.ok(!out1.includes(KEY), 'THE KEY MUST NEVER APPEAR IN OUTPUT');
  const r1 = JSON.parse(out1.trim().split('\n').pop()!);
  assert.equal(r1.key.source, 'token');
  assert.ok(readFileSync(join(cwd, '.env'), 'utf8').includes('UPCONTROL_API_KEY=' + KEY));

  const cwd2 = mkdtempSync(join(tmpdir(), 'uc-tok2-'));
  const { stdout: out2, code: code2 } = await runCliFail(cwd2, ['init', '--token', 'uct_testtoken', '--endpoint', url2]);
  const r2 = JSON.parse(out2.trim().split('\n').pop()!);
  assert.equal(r2.key.source, 'none', 'a refused token must not fall back to the anonymous mint');
  assert.equal(r2.success, false, 'a refused token is a failed init, not a success with a footnote');
  assert.equal(code2, 1, 'a refused token must exit 1');
  assert.match(r2.key.note, /already used or expired/);
  assert.ok(!existsSync(join(cwd2, '.env')));

  await new Promise((r) => server2.close(r));
  rmSync(cwd, { recursive: true, force: true });
  rmSync(cwd2, { recursive: true, force: true });
});
