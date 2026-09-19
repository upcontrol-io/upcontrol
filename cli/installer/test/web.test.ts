import test from 'node:test';
import assert from 'node:assert/strict';
import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import { createServer } from 'node:http';
import { mkdtempSync, readFileSync, writeFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';
import type { AddressInfo } from 'node:net';
import { siteOrigins } from '../src/files.ts';

// The web command against a local stub: the mint body and the tag as the one
// line an agent consumes, reuse without a request, the website-card origin
// rule, and the secret key never in any output.

const execFileP = promisify(execFile);
const here = dirname(fileURLToPath(import.meta.url));
const cli = join(here, '..', 'dist', 'main.js');
const KEY = 'uc_live_deadbeefdeadbeefdeadbeefdeadbeef';
const PUB = 'uc_pub_deadbeefdeadbeefdeadbeef';

interface Seen {
  method: string;
  url: string;
  key: string | undefined;
  body: unknown;
}

interface WebMock {
  url: string;
  seen: Seen[];
  setMint(status: number, code?: string): void;
  close(): Promise<void>;
}

function mockWeb(): Promise<WebMock> {
  const seen: Seen[] = [];
  let mintStatus = 201;
  let mintCode = '';
  const server = createServer((req, res) => {
    const chunks: Buffer[] = [];
    req.on('data', (c: Buffer) => chunks.push(c));
    req.on('end', () => {
      const body = chunks.length ? JSON.parse(Buffer.concat(chunks).toString()) : undefined;
      seen.push({ method: req.method!, url: req.url!, key: req.headers['x-upcontrol-key'] as string | undefined, body });
      res.setHeader('content-type', 'application/json');
      if (req.url === '/v1/keys' && req.method === 'POST') {
        if (mintStatus !== 201) {
          res.writeHead(mintStatus).end(JSON.stringify({ error: { code: mintCode, message: 'stub refusal' } }));
        } else {
          const asked = body as { name?: string; origins?: string[] };
          res.writeHead(201).end(
            JSON.stringify({
              id: 'key_9',
              prefix: 'uc_pub_deadbeef',
              createdAt: '2026-09-19T00:00:00Z',
              name: asked.name,
              kind: 'public',
              origins: asked.origins,
              value: PUB,
            }),
          );
        }
      } else if (req.url === '/v1/install/status') {
        // A site-only install: page views, no marker, no log lines.
        res.writeHead(200).end(JSON.stringify({ verified: false, lines: 0, recent: [], web: { views: 2 } }));
      } else {
        res.writeHead(404).end();
      }
    });
  });
  return new Promise<WebMock>((resolve) => {
    server.listen(0, '127.0.0.1', () =>
      resolve({
        url: `http://127.0.0.1:${(server.address() as AddressInfo).port}`,
        seen,
        setMint: (s, c) => {
          mintStatus = s;
          mintCode = c ?? '';
        },
        close: () => new Promise<void>((r) => server.close(() => r())),
      }),
    );
  });
}

function webCwd(): string {
  const cwd = mkdtempSync(join(tmpdir(), 'uc-web-'));
  writeFileSync(join(cwd, '.env'), `UPCONTROL_API_KEY=${KEY}\n`);
  return cwd;
}

function childEnv(extra: Record<string, string> = {}): NodeJS.ProcessEnv {
  return { ...process.env, UPCONTROL_API_KEY: '', UPCONTROL_PUBLIC_KEY: '', AI_AGENT: 'claude', ...extra };
}

// execFile rejects on non-zero exits; the output rides on the rejection.
async function runWeb(
  cwd: string,
  args: string[],
  extraEnv: Record<string, string> = {},
): Promise<{ stdout: string; stderr: string; code: number }> {
  try {
    const { stdout, stderr } = await execFileP(process.execPath, [cli, ...args], {
      cwd,
      encoding: 'utf8',
      env: childEnv(extraEnv),
    });
    return { stdout, stderr, code: 0 };
  } catch (e) {
    const x = e as { stdout?: string; stderr?: string; code?: number };
    return { stdout: x.stdout ?? '', stderr: x.stderr ?? '', code: Number(x.code ?? 1) };
  }
}

test('siteOrigins is the website card rule: scheme default, www/apex twin, port kept, junk refused', () => {
  assert.deepEqual(siteOrigins('example.com'), ['https://example.com', 'https://www.example.com']);
  assert.deepEqual(siteOrigins('www.example.com'), ['https://www.example.com', 'https://example.com']);
  assert.deepEqual(siteOrigins('localhost:5173'), ['http://localhost:5173']);
  assert.deepEqual(siteOrigins('127.0.0.1:8080'), ['http://127.0.0.1:8080']);
  assert.deepEqual(siteOrigins('https://example.com:8443'), ['https://example.com:8443', 'https://www.example.com:8443']);
  assert.deepEqual(siteOrigins('http://[::1]:5173'), ['http://[::1]:5173']);
  assert.deepEqual(siteOrigins('ftp://example.com'), []);
  assert.deepEqual(siteOrigins('https://*.example.com'), []);
  assert.deepEqual(siteOrigins('not a site'), []);
  assert.deepEqual(siteOrigins(''), []);
});

test('a mint writes UPCONTROL_PUBLIC_KEY to .env, prints the tag JSON, never the secret key', async () => {
  const srv = await mockWeb();
  const cwd = webCwd();
  const { stdout, stderr, code } = await runWeb(cwd, ['web', 'example.com', '--endpoint', srv.url]);
  await srv.close();
  assert.equal(code, 0);
  const line = JSON.parse(stdout.trim()) as { ok: boolean; tag: string; origins: string[]; key: string; next: string };
  assert.equal(line.ok, true);
  assert.equal(line.key, 'minted');
  assert.deepEqual(line.origins, ['https://example.com', 'https://www.example.com']);
  assert.equal(line.tag, `<script defer src="${srv.url}/uc.js" data-key="${PUB}"></script>`);
  assert.match(line.tag, /data-key="uc_pub_/);
  assert.ok(line.next.includes('skills web'));
  assert.equal(srv.seen.length, 1);
  assert.equal(srv.seen[0].method, 'POST');
  assert.equal(srv.seen[0].key, KEY, 'the mint must carry the secret key');
  assert.deepEqual(srv.seen[0].body, {
    name: 'example.com',
    kind: 'public',
    origins: ['https://example.com', 'https://www.example.com'],
  });
  assert.match(readFileSync(join(cwd, '.env'), 'utf8'), new RegExp(`UPCONTROL_PUBLIC_KEY=${PUB}`));
  assert.ok(!stdout.includes('uc_live_') && !stderr.includes('uc_live_'), 'THE SECRET KEY MUST NEVER APPEAR IN OUTPUT');
  rmSync(cwd, { recursive: true, force: true });
});

test('a second run reuses the stored public key and makes no request', async () => {
  const srv = await mockWeb();
  const cwd = mkdtempSync(join(tmpdir(), 'uc-web-'));
  writeFileSync(join(cwd, '.env'), `UPCONTROL_API_KEY=${KEY}\nUPCONTROL_PUBLIC_KEY=${PUB}\n`);
  const { stdout, stderr, code } = await runWeb(cwd, ['web', 'example.com', '--endpoint', srv.url]);
  await srv.close();
  assert.equal(code, 0);
  const line = JSON.parse(stdout.trim()) as { key: string; tag: string; origins?: string[]; note?: string };
  assert.equal(line.key, 'reused');
  assert.equal(line.tag, `<script defer src="${srv.url}/uc.js" data-key="${PUB}"></script>`);
  // The key's scope is whatever minted it, never this run's arguments.
  assert.equal(line.origins, undefined, 'a reused key must not report this run as its origins');
  assert.match(line.note ?? '', /covers only the sites it was minted for.*Manage keys.*naming every address/);
  assert.match(stderr, /key: reused - the key in \.env covers only/, 'the reuse is said in agent mode too');
  assert.equal(srv.seen.length, 0, 'a reused key must not mint another');
  rmSync(cwd, { recursive: true, force: true });
});

test('verify passes on page views only with --web, and says so without it', async () => {
  const srv = await mockWeb();
  const cwd = webCwd();
  const web = await runWeb(cwd, ['verify', '--web', '--json', '--endpoint', srv.url]);
  assert.equal(web.code, 0);
  const pass = JSON.parse(web.stdout.trim()) as { verified: boolean; web: { views: number } };
  assert.equal(pass.verified, false, 'verified stays the SDK marker');
  assert.equal(pass.web.views, 2);

  const sdk = await runWeb(cwd, ['verify', '--json', '--timeout', '0.001', '--endpoint', srv.url]);
  await srv.close();
  assert.equal(sdk.code, 4, 'page views must not hide an SDK that never connected');
  const fail = JSON.parse(sdk.stdout.trim()) as { verified: boolean; error: string; hint: string };
  assert.equal(fail.verified, false);
  assert.equal(fail.error, 'timeout');
  assert.match(fail.hint, /page views are arriving \(2\).*verify --web/);
  rmSync(cwd, { recursive: true, force: true });
});

test('localhost stays local and alone', async () => {
  const srv = await mockWeb();
  const cwd = webCwd();
  const { stdout, code } = await runWeb(cwd, ['web', 'localhost:5173', '--endpoint', srv.url]);
  await srv.close();
  assert.equal(code, 0);
  assert.deepEqual((JSON.parse(stdout.trim()) as { origins: string[] }).origins, ['http://localhost:5173']);
  rmSync(cwd, { recursive: true, force: true });
});

test('a 409 key_limit exits 3 with the revoke line', async () => {
  const srv = await mockWeb();
  srv.setMint(409, 'key_limit');
  const cwd = webCwd();
  const { stdout, stderr, code } = await runWeb(cwd, ['web', 'example.com', '--endpoint', srv.url]);
  await srv.close();
  assert.equal(code, 3);
  assert.match(stderr, /revoke one in the app/);
  assert.ok(!stdout.includes('uc_live_') && !stderr.includes('uc_live_'), 'THE SECRET KEY MUST NEVER APPEAR IN OUTPUT');
  rmSync(cwd, { recursive: true, force: true });
});

test('a refused secret key exits 3 and sends the reader back to init', async () => {
  const srv = await mockWeb();
  srv.setMint(401, 'bad_key');
  const cwd = webCwd();
  const { stderr, code } = await runWeb(cwd, ['web', 'example.com', '--endpoint', srv.url]);
  await srv.close();
  assert.equal(code, 3);
  assert.match(stderr, /was refused \(revoked, or retiring after a rotation\)/);
  rmSync(cwd, { recursive: true, force: true });
});

test('an unreadable address exits 2 naming it, and no args is usage', async () => {
  const srv = await mockWeb();
  const cwd = webCwd();
  const bad = await runWeb(cwd, ['web', 'not a site', '--endpoint', srv.url]);
  assert.equal(bad.code, 2);
  assert.match(bad.stderr, /not a site address: not a site/);
  const none = await runWeb(cwd, ['web', '--endpoint', srv.url]);
  assert.equal(none.code, 2);
  assert.match(none.stderr, /usage: npx upcontrol web/);
  await srv.close();
  assert.equal(srv.seen.length, 0, 'a usage error must not reach the server');
  rmSync(cwd, { recursive: true, force: true });
});

test('no key exits 2 and stdout stays empty', async () => {
  const cwd = mkdtempSync(join(tmpdir(), 'uc-web-'));
  const { stdout, stderr, code } = await runWeb(cwd, ['web', 'example.com']);
  assert.equal(code, 2, 'no key must exit 2, the code verify uses');
  assert.equal(stdout, '');
  assert.match(stderr, /no key/);
  assert.ok(!stdout.includes('uc_live_') && !stderr.includes('uc_live_'), 'THE SECRET KEY MUST NEVER APPEAR IN OUTPUT');
  rmSync(cwd, { recursive: true, force: true });
});
