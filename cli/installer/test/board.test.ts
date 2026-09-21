import test from 'node:test';
import assert from 'node:assert/strict';
import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import { createServer } from 'node:http';
import { mkdtempSync, writeFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';
import type { AddressInfo } from 'node:net';

// The board command against a local stub: the exact JSON passthrough, the
// 200/202 fork on --apply, the server's refusal sentence carried verbatim,
// the several-boards flags, and the key never in stdout.

const execFileP = promisify(execFile);
const here = dirname(fileURLToPath(import.meta.url));
const cli = join(here, '..', 'dist', 'main.js');
const KEY = 'uc_live_deadbeefdeadbeefdeadbeefdeadbeef';

const LAYOUT = {
  version: 2,
  widgets: [
    { id: 'w1', kind: 'uptime', x: 0, y: 0, w: 6, h: 4 },
    { id: 'w2', kind: 'logs', x: 6, y: 0, w: 6, h: 4 },
  ],
};

const MAIN_ID = 'a'.repeat(32);
const PAY_ID = 'b'.repeat(32);
const NEW_ID = 'c'.repeat(32);
const LIST = {
  boards: [
    { id: MAIN_ID, name: 'Main', frozen: false, writtenBy: 'session', widgets: 2, proposed: false },
    { id: PAY_ID, name: 'Payments', frozen: false, writtenBy: 'key', widgets: 0, proposed: false },
  ],
  max: 3,
};
const CAP_402 = {
  error: {
    code: 'plan_limit_exceeded',
    message: 'Your plan carries 1 dashboard per project. Growth carries 3.',
    upgrade: { reason: 'Your plan carries 1 dashboard per project. Growth carries 3.', plan: 'growth' },
  },
};

interface Seen {
  method: string;
  url: string;
  key: string | undefined;
  body: unknown;
}

interface BoardMock {
  url: string;
  seen: Seen[];
  setPut(status: number): void;
  setCreate(status: number): void;
  close(): Promise<void>;
}

// `boards: false` is a core from before several boards: it answers the legacy
// path and nothing else, so every /v1/dashboards call falls to the bodyless 404
// an unrouted path gives - exactly what the CLI has to tell apart from a board
// that does not exist.
function mockBoard(opts: { boards?: boolean } = {}): Promise<BoardMock> {
  const seen: Seen[] = [];
  const several = opts.boards !== false;
  let putStatus = 200;
  let createStatus = 201;
  const server = createServer((req, res) => {
    const chunks: Buffer[] = [];
    req.on('data', (c: Buffer) => chunks.push(c));
    req.on('end', () => {
      const body = chunks.length ? JSON.parse(Buffer.concat(chunks).toString()) : undefined;
      const url = req.url!;
      seen.push({ method: req.method!, url, key: req.headers['x-upcontrol-key'] as string | undefined, body });
      res.setHeader('content-type', 'application/json');
      if (url === '/v1/dashboard' && req.method === 'GET') {
        res.end(JSON.stringify(LAYOUT));
      } else if (url === '/v1/dashboard' && req.method === 'PUT') {
        if (putStatus === 202) res.writeHead(202).end(JSON.stringify({ status: 'proposed' }));
        else if (putStatus === 400)
          res.writeHead(400).end(JSON.stringify({ error: { code: 'bad_layout', message: 'widget w1: x + w is 14, past the 12-column grid' } }));
        else res.writeHead(200).end(JSON.stringify(body));
      } else if (url === '/v1/dashboard/widgets' && req.method === 'POST') {
        const merged = [...LAYOUT.widgets, ...((body as { widgets: unknown[] }).widgets ?? [])];
        res.writeHead(200).end(JSON.stringify({ version: 2, widgets: merged }));
      } else if (several && url === '/v1/dashboards' && req.method === 'GET') {
        res.end(JSON.stringify(LIST));
      } else if (several && url === '/v1/dashboards' && req.method === 'POST') {
        if (createStatus === 402) res.writeHead(402).end(JSON.stringify(CAP_402));
        else res.writeHead(201).end(JSON.stringify({ id: NEW_ID, name: (body as { name: string }).name }));
      } else if (several && url === `/v1/dashboards/${PAY_ID}` && req.method === 'GET') {
        res.end(JSON.stringify(LAYOUT));
      } else if (several && url === `/v1/dashboards/${PAY_ID}` && req.method === 'PUT') {
        if (putStatus === 202) res.writeHead(202).end(JSON.stringify({ status: 'proposed', id: PAY_ID, name: 'Payments' }));
        else res.writeHead(200).end(JSON.stringify(body));
      } else if (several && url.startsWith('/v1/dashboards/')) {
        res.writeHead(404).end(JSON.stringify({ error: { code: 'unknown_board', message: 'board: no dashboard with that id' } }));
      } else {
        res.writeHead(404).end();
      }
    });
  });
  return new Promise<BoardMock>((resolve) => {
    server.listen(0, '127.0.0.1', () =>
      resolve({
        url: `http://127.0.0.1:${(server.address() as AddressInfo).port}`,
        seen,
        setPut: (s: number) => (putStatus = s),
        setCreate: (s: number) => (createStatus = s),
        close: () => new Promise<void>((r) => server.close(() => r())),
      }),
    );
  });
}

function boardCwd(): string {
  const cwd = mkdtempSync(join(tmpdir(), 'uc-brd-'));
  writeFileSync(join(cwd, '.env'), `UPCONTROL_API_KEY=${KEY}\n`);
  return cwd;
}

function childEnv(extra: Record<string, string> = {}): NodeJS.ProcessEnv {
  return { ...process.env, UPCONTROL_API_KEY: '', AI_AGENT: 'claude', ...extra };
}

// execFile rejects on non-zero exits; the output rides on the rejection.
async function runBoard(
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

test('plain board prints exactly the JSON the server returned, with the key header', async () => {
  const srv = await mockBoard();
  const cwd = boardCwd();
  const { stdout, code } = await runBoard(cwd, ['board', '--endpoint', srv.url]);
  await srv.close();
  assert.equal(code, 0);
  assert.equal(stdout.trim(), JSON.stringify(LAYOUT), 'stdout must be the server body, nothing else');
  assert.equal(srv.seen[0].method, 'GET');
  assert.equal(srv.seen[0].key, KEY, 'the request must carry the ingest key');
  assert.ok(!stdout.includes(KEY), 'THE KEY MUST NEVER APPEAR IN OUTPUT');
  rmSync(cwd, { recursive: true, force: true });
});

test('board --apply on a 200 prints the stored line and exits 0', async () => {
  const srv = await mockBoard();
  const cwd = boardCwd();
  const file = join(cwd, 'board.json');
  writeFileSync(file, JSON.stringify(LAYOUT));
  const { stdout, code } = await runBoard(cwd, ['board', '--apply', file, '--endpoint', srv.url]);
  await srv.close();
  assert.equal(code, 0);
  assert.equal(stdout.trim(), 'stored - 2 widgets.');
  assert.deepEqual(srv.seen[0].body, LAYOUT, 'PUT must send the parsed layout as the body');
  rmSync(cwd, { recursive: true, force: true });
});

test('board --apply on a 202 prints the proposal wording and still exits 0', async () => {
  const srv = await mockBoard();
  srv.setPut(202);
  const cwd = boardCwd();
  const file = join(cwd, 'board.json');
  writeFileSync(file, JSON.stringify(LAYOUT));
  const { stdout, code } = await runBoard(cwd, ['board', '--apply', file, '--endpoint', srv.url]);
  await srv.close();
  assert.equal(code, 0, 'a proposal is a success, not a failure');
  assert.match(stdout, /proposal/);
  assert.match(stdout, new RegExp(srv.url + '/app/dashboard'));
  assert.match(stdout, /Review/);
  rmSync(cwd, { recursive: true, force: true });
});

test('a refused layout prints the server message verbatim and exits 1', async () => {
  const srv = await mockBoard();
  srv.setPut(400);
  const cwd = boardCwd();
  const file = join(cwd, 'board.json');
  writeFileSync(file, JSON.stringify(LAYOUT));
  const { stderr, code } = await runBoard(cwd, ['board', '--apply', file, '--endpoint', srv.url]);
  await srv.close();
  assert.equal(code, 1);
  assert.equal(stderr.trim(), 'widget w1: x + w is 14, past the 12-column grid');
  rmSync(cwd, { recursive: true, force: true });
});

test('board --add appends and reports both counts', async () => {
  const srv = await mockBoard();
  const cwd = boardCwd();
  const file = join(cwd, 'more.json');
  writeFileSync(file, JSON.stringify({ widgets: [{ id: 'w3', kind: 'events', x: 0, y: 4, w: 12, h: 2 }] }));
  const { stdout, code } = await runBoard(cwd, ['board', '--add', file, '--endpoint', srv.url]);
  await srv.close();
  assert.equal(code, 0);
  assert.equal(stdout.trim(), 'added 1 widgets - the board now has 3.');
  assert.deepEqual(
    srv.seen[0].body,
    { widgets: [{ id: 'w3', kind: 'events', x: 0, y: 4, w: 12, h: 2 }] },
    'POST must wrap the widgets in a { widgets } body',
  );
  rmSync(cwd, { recursive: true, force: true });
});

test('board --add - reads the widgets from stdin', async () => {
  const srv = await mockBoard();
  const cwd = boardCwd();
  const { stdout, code } = await new Promise<{ stdout: string; code: number }>((resolve) => {
    execFile(
      process.execPath,
      [cli, 'board', '--add', '-', '--endpoint', srv.url],
      { cwd, encoding: 'utf8', env: childEnv() },
      (e, stdout) => resolve({ stdout, code: e ? Number((e as { code?: number }).code ?? 1) : 0 }),
    ).stdin!.end(JSON.stringify({ widgets: [{ id: 'w3', kind: 'events', x: 0, y: 4, w: 12, h: 2 }] }));
  });
  await srv.close();
  assert.equal(code, 0);
  assert.equal(stdout.trim(), 'added 1 widgets - the board now has 3.');
  rmSync(cwd, { recursive: true, force: true });
});

test('input that is not JSON names the file and exits 1', async () => {
  const cwd = boardCwd();
  const file = join(cwd, 'broken.json');
  writeFileSync(file, '{version: 2, widgets: [}');
  const { stderr, code } = await runBoard(cwd, ['board', '--apply', file, '--endpoint', 'http://127.0.0.1:1']);
  assert.equal(code, 1);
  assert.match(stderr, /broken\.json is not JSON/);
  rmSync(cwd, { recursive: true, force: true });
});

test('--apply and --add together is a usage error', async () => {
  const srv = await mockBoard();
  const cwd = boardCwd();
  const a = join(cwd, 'a.json');
  writeFileSync(a, JSON.stringify(LAYOUT));
  const { stderr, code } = await runBoard(cwd, ['board', '--apply', a, '--add', a, '--endpoint', srv.url]);
  await srv.close();
  assert.equal(code, 1);
  assert.match(stderr, /mutually exclusive/);
  assert.equal(srv.seen.length, 0, 'a usage error must not reach the server');
  rmSync(cwd, { recursive: true, force: true });
});

test('no key exits 2 and stdout stays empty', async () => {
  const cwd = mkdtempSync(join(tmpdir(), 'uc-brd-'));
  const { stdout, stderr, code } = await runBoard(cwd, ['board']);
  assert.equal(code, 2, 'no key must exit 2, the code verify uses');
  assert.equal(stdout, '');
  assert.ok(!stdout.includes('uc_live_'), 'THE KEY MUST NEVER APPEAR IN OUTPUT');
  assert.match(stderr, /no key found/);
  rmSync(cwd, { recursive: true, force: true });
});

test('unreachable endpoint exits 3, the code verify uses', async () => {
  const cwd = boardCwd();
  const { stderr, code } = await runBoard(cwd, ['board', '--endpoint', 'http://127.0.0.1:9']);
  assert.equal(code, 3);
  assert.match(stderr, /cannot reach/);
  rmSync(cwd, { recursive: true, force: true });
});

test('board --list prints the board list verbatim, key header and all', async () => {
  const srv = await mockBoard();
  const cwd = boardCwd();
  const { stdout, code } = await runBoard(cwd, ['board', '--list', '--endpoint', srv.url]);
  await srv.close();
  assert.equal(code, 0);
  assert.equal(stdout.trim(), JSON.stringify(LIST), 'stdout must be the server body, nothing else');
  assert.equal(srv.seen[0].url, '/v1/dashboards');
  assert.equal(srv.seen[0].key, KEY);
  assert.ok(!stdout.includes('uc_live_'), 'THE KEY MUST NEVER APPEAR IN OUTPUT');
  rmSync(cwd, { recursive: true, force: true });
});

test('a core that knows one board per project says so instead of "refused (HTTP 404)"', async () => {
  const srv = await mockBoard({ boards: false });
  const cwd = boardCwd();
  const { stdout, stderr, code } = await runBoard(cwd, ['board', '--list', '--endpoint', srv.url]);
  await srv.close();
  assert.notEqual(code, 0);
  assert.equal(stdout, '');
  assert.match(stderr, /one board per project/);
  rmSync(cwd, { recursive: true, force: true });
});

test('the bare board command keeps reading the legacy path, on any core', async () => {
  const srv = await mockBoard({ boards: false });
  const cwd = boardCwd();
  const { stdout, code } = await runBoard(cwd, ['board', '--endpoint', srv.url]);
  await srv.close();
  assert.equal(code, 0);
  assert.equal(stdout.trim(), JSON.stringify(LAYOUT));
  assert.equal(srv.seen[0].url, '/v1/dashboard', 'no --board means the alias every core answers');
  rmSync(cwd, { recursive: true, force: true });
});

test('--board takes a name through one list read and then the board itself', async () => {
  const srv = await mockBoard();
  const cwd = boardCwd();
  const { stdout, code } = await runBoard(cwd, ['board', '--board', 'payments', '--endpoint', srv.url]);
  await srv.close();
  assert.equal(code, 0);
  assert.equal(stdout.trim(), JSON.stringify(LAYOUT));
  assert.deepEqual(
    srv.seen.map((s) => s.url),
    ['/v1/dashboards', `/v1/dashboards/${PAY_ID}`],
    'the name is matched case-insensitively against the list, then the id is used',
  );
  rmSync(cwd, { recursive: true, force: true });
});

test('a name no board carries names itself and exits 1', async () => {
  const srv = await mockBoard();
  const cwd = boardCwd();
  const { stderr, code } = await runBoard(cwd, ['board', '--board', 'Billing', '--endpoint', srv.url]);
  await srv.close();
  assert.equal(code, 1);
  assert.match(stderr, /no dashboard named "Billing"/);
  assert.equal(srv.seen.length, 1, 'only the list read - a name that matched nothing is never sent on');
  rmSync(cwd, { recursive: true, force: true });
});

test('an id no board carries is the server sentence, not the old-core one', async () => {
  const srv = await mockBoard();
  const cwd = boardCwd();
  const { stderr, code } = await runBoard(cwd, ['board', '--board', 'd'.repeat(32), '--endpoint', srv.url]);
  await srv.close();
  assert.equal(code, 1);
  assert.equal(stderr.trim(), 'board: no dashboard with that id');
  assert.equal(srv.seen.length, 1, 'a 32-hex id costs no list read');
  rmSync(cwd, { recursive: true, force: true });
});

test('--new creates the board and prints its id', async () => {
  const srv = await mockBoard();
  const cwd = boardCwd();
  const { stdout, code } = await runBoard(cwd, ['board', '--new', 'Payments', '--endpoint', srv.url]);
  await srv.close();
  assert.equal(code, 0);
  assert.equal(stdout.trim(), `created "Payments" - board ${NEW_ID}.`);
  assert.equal(srv.seen[0].method, 'POST');
  assert.deepEqual(srv.seen[0].body, { name: 'Payments' });
  rmSync(cwd, { recursive: true, force: true });
});

test('--new with --apply sends the first layout in the same create', async () => {
  const srv = await mockBoard();
  const cwd = boardCwd();
  const file = join(cwd, 'board.json');
  writeFileSync(file, JSON.stringify(LAYOUT));
  const { stdout, code } = await runBoard(cwd, ['board', '--new', 'Payments', '--apply', file, '--endpoint', srv.url]);
  await srv.close();
  assert.equal(code, 0);
  assert.equal(stdout.trim(), `created "Payments" - board ${NEW_ID}, 2 widgets.`);
  assert.deepEqual(srv.seen[0].body, { name: 'Payments', layout: LAYOUT }, 'one request, not a create then a write');
  assert.equal(srv.seen.length, 1);
  rmSync(cwd, { recursive: true, force: true });
});

test('a board past the plan prints the server sentence and exits non-zero', async () => {
  const srv = await mockBoard();
  srv.setCreate(402);
  const cwd = boardCwd();
  const { stdout, stderr, code } = await runBoard(cwd, ['board', '--new', 'Payments', '--endpoint', srv.url]);
  await srv.close();
  assert.notEqual(code, 0);
  assert.equal(stdout, '');
  assert.equal(stderr.trim(), 'Your plan carries 1 dashboard per project. Growth carries 3.');
  rmSync(cwd, { recursive: true, force: true });
});

test('a proposal on a named board points at that board in the app', async () => {
  const srv = await mockBoard();
  srv.setPut(202);
  const cwd = boardCwd();
  const file = join(cwd, 'board.json');
  writeFileSync(file, JSON.stringify(LAYOUT));
  const { stdout, code } = await runBoard(cwd, ['board', '--board', PAY_ID, '--apply', file, '--endpoint', srv.url]);
  await srv.close();
  assert.equal(code, 0);
  assert.match(stdout, new RegExp(`${srv.url}/app/dashboard\\?board=${PAY_ID} and press Review`));
  rmSync(cwd, { recursive: true, force: true });
});

test('a value flag with nothing after it names the flag instead of acting as if it were absent', async () => {
  const srv = await mockBoard();
  const cwd = boardCwd();
  const { stderr, code } = await runBoard(cwd, ['board', '--new', '--endpoint', srv.url]);
  await srv.close();
  assert.equal(code, 1);
  assert.equal(stderr.trim(), 'board: --new needs a value');
  assert.equal(srv.seen.length, 0, 'a usage error must not reach the server');
  rmSync(cwd, { recursive: true, force: true });
});

test('--list with any other board flag is a usage error', async () => {
  const srv = await mockBoard();
  const cwd = boardCwd();
  const { stderr, code } = await runBoard(cwd, ['board', '--list', '--board', PAY_ID, '--endpoint', srv.url]);
  await srv.close();
  assert.equal(code, 1);
  assert.match(stderr, /--list takes no other board flag/);
  assert.equal(srv.seen.length, 0, 'a usage error must not reach the server');
  rmSync(cwd, { recursive: true, force: true });
});
