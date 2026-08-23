import test from 'node:test';
import assert from 'node:assert/strict';
import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import { createServer } from 'node:http';
import { mkdtempSync, writeFileSync, existsSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';
import type { AddressInfo } from 'node:net';

interface MockBackend {
  url: string;
  metas: Record<string, unknown>[];
  close: () => Promise<void>;
}
import { collectSpec, formatSpec } from '../src/meta.ts';

// The project-spec half of init (ai-provider-and-scenarios plan, Decisions
// 15b/16): only the five whitelisted fields, the exact transparency copy on
// stdout, --no-meta sends nothing, and no meta failure can fail the install.

const execFileP = promisify(execFile);

const here = dirname(fileURLToPath(import.meta.url));
const cli = join(here, '..', 'dist', 'main.js');
const KEY = 'uc_live_deadbeefdeadbeefdeadbeefdeadbeef';
const SPEC_HEADER = 'project spec (sent so AI log analysis knows your stack; nothing else is read):';

const tmp = () => mkdtempSync(join(tmpdir(), 'uc-meta-'));

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

function mockBackend(metaStatus = 204): Promise<MockBackend> {
  const metas: Record<string, unknown>[] = [];
  const server = createServer((req, res) => {
    if (req.url === '/v1/projects/anonymous') {
      res.writeHead(200, { 'content-type': 'application/json' });
      res.end(JSON.stringify({ projectId: 'prj_test', key: KEY, claimToken: 'tok', claimUrl: 'http://x/claim/tok' }));
      return;
    }
    if (req.method === 'PUT' && req.url === '/v1/project/meta') {
      let body = '';
      req.on('data', (c) => (body += c));
      req.on('end', () => {
        metas.push({ auth: req.headers['x-upcontrol-key'], body: JSON.parse(body) });
        // A refusal carries the server's real error envelope. A bare status
        // with no body is a shape the API never sends, and answering that way
        // hid a crash for a whole release: an unread response body kept an
        // undici handle open and Node aborted the process during teardown
        // (0xC0000409). The mock has to be able to fail the way the server
        // fails, or the test proves nothing about the path it names.
        if (metaStatus >= 400) {
          res.writeHead(metaStatus, { 'content-type': 'application/json' });
          res.end(JSON.stringify({ error: { code: 'meta_too_large', message: 'description exceeds the 200-character cap' } }));
          return;
        }
        res.writeHead(metaStatus);
        res.end();
      });
      return;
    }
    res.writeHead(404);
    res.end();
  });
  return new Promise<MockBackend>((resolve) => {
    server.listen(0, '127.0.0.1', () =>
      resolve({
        url: `http://127.0.0.1:${(server.address() as AddressInfo).port}`,
        metas,
        close: () => new Promise<void>((r) => server.close(() => r())),
      }),
    );
  });
}

test('collectSpec: five fields from a fixture package.json, framework order respected', () => {
  const cwd = tmp();
  writeFileSync(
    join(cwd, 'package.json'),
    JSON.stringify({
      name: 'fixture-app',
      description: 'a test app',
      dependencies: { react: '^19.0.0', '@upcontrol/sdk': '0.1.0' },
      devDependencies: { next: '^15.0.0' },
    }),
  );
  writeFileSync(join(cwd, 'tsconfig.json'), '{}\n');
  const spec = collectSpec(cwd, 'node v24.0.0');
  assert.deepEqual(spec, {
    name: 'fixture-app',
    description: 'a test app',
    framework: 'next',
    runtime: 'node v24.0.0',
    language: 'typescript',
  });
  rmSync(cwd, { recursive: true, force: true });
});

test('collectSpec: framework map order beats dependency order, @nestjs/core maps to nest', () => {
  const cwd = tmp();
  // react is listed first in deps, but the ordered map puts express ahead of react
  writeFileSync(
    join(cwd, 'package.json'),
    JSON.stringify({ dependencies: { react: '^19', express: '^4' } }),
  );
  assert.equal(collectSpec(cwd, 'node v24.0.0')!.framework, 'express');

  writeFileSync(join(cwd, 'package.json'), JSON.stringify({ devDependencies: { '@nestjs/core': '^10' } }));
  assert.equal(collectSpec(cwd, 'node v24.0.0')!.framework, 'nest');

  writeFileSync(join(cwd, 'package.json'), JSON.stringify({ name: 'x' }));
  assert.equal(collectSpec(cwd, 'node v24.0.0')!.framework, undefined, 'no match - omitted, never guessed');
  assert.equal(collectSpec(cwd, 'node v24.0.0')!.language, 'javascript', 'no tsconfig.json');
  rmSync(cwd, { recursive: true, force: true });
});

test('collectSpec: no package.json or an unparseable one collects nothing', () => {
  const cwd = tmp();
  assert.equal(collectSpec(cwd, 'node v24.0.0'), null);
  writeFileSync(join(cwd, 'package.json'), '{ not json');
  assert.equal(collectSpec(cwd, 'node v24.0.0'), null);
  rmSync(cwd, { recursive: true, force: true });
});

test('formatSpec pins the Decision 16 copy: header, aligned fields, skip hint', () => {
  const text = formatSpec({
    name: 'fixture-app',
    description: 'a test app',
    framework: 'next',
    runtime: 'node v24.0.0',
    language: 'typescript',
  });
  assert.equal(
    text,
    SPEC_HEADER + '\n' +
      '  name        fixture-app\n' +
      '  description a test app\n' +
      '  framework   next\n' +
      '  runtime     node v24.0.0\n' +
      '  language    typescript\n' +
      '  (skip with --no-meta)',
  );
  // absent fields are omitted, never sent as blanks
  const partial = formatSpec({ runtime: 'node v24.0.0', language: 'javascript' });
  assert.ok(!partial.includes('name '), 'unset fields must not render placeholder lines');
  assert.ok(partial.includes('  (skip with --no-meta)'));
});

test('init prints the spec, uploads it keyed, and the key never reaches stdout', async () => {
  const srv = await mockBackend();
  const cwd = tmp();
  writeFileSync(
    join(cwd, 'package.json'),
    JSON.stringify({ name: 'fixture-app', description: 'a test app', dependencies: { express: '^4' } }),
  );
  writeFileSync(join(cwd, '.gitignore'), 'node_modules/\n');

  const stdout = await runCli(cwd, ['init', '--endpoint', srv.url]);
  await srv.close();

  assert.ok(!stdout.includes(KEY), 'THE KEY MUST NEVER APPEAR IN OUTPUT');
  assert.ok(stdout.includes(SPEC_HEADER));
  assert.ok(stdout.includes('  name        fixture-app'));
  assert.ok(stdout.includes('  (skip with --no-meta)'));
  const result = JSON.parse(stdout.trim().split('\n').pop()!);
  assert.equal(result.success, true, 'the JSON result must still be the last line');

  assert.equal(srv.metas.length, 1);
  assert.equal(srv.metas[0].auth, KEY, 'upload carries the project key in X-UpControl-Key');
  // the child runs the same process.execPath, so its process.version is this one
  assert.deepEqual(srv.metas[0].body, {
    name: 'fixture-app',
    description: 'a test app',
    framework: 'express',
    runtime: 'node ' + process.version,
    language: 'javascript',
  });
  rmSync(cwd, { recursive: true, force: true });
});

test('a refused upload leaves the process able to exit (0xC0000409 regression)', async () => {
  // The crash: putProjectMeta returned on !res.ok without reading the body,
  // undici kept the stream handle open, and Node aborted during teardown -
  // `Assertion failed: !(handle->flags & UV_HANDLE_CLOSING)`, exit
  // 3221226505 on Windows. It fired on the ordinary refusal path, so init
  // crashed for every project whose spec the server declined. execFileP
  // rejects on a non-zero exit, which is the assertion: reaching the next
  // line means the process exited cleanly.
  const srv = await mockBackend(400);
  const cwd = tmp();
  writeFileSync(
    join(cwd, 'package.json'),
    JSON.stringify({ name: 'fixture-app', description: 'a test app' }),
  );
  writeFileSync(join(cwd, '.gitignore'), 'node_modules/\n');

  const stdout = await runCli(cwd, ['init', '--endpoint', srv.url]);
  await srv.close();

  assert.equal(JSON.parse(stdout.trim().split('\n').pop()!).success, true);
  rmSync(cwd, { recursive: true, force: true });
});

test('the spec goes to the key this run established, not the ambient one', async () => {
  // --key names the project the user chose. Re-reading UPCONTROL_API_KEY
  // afterwards uploaded their spec to whatever project the environment
  // happened to name instead.
  const srv = await mockBackend();
  const cwd = tmp();
  const chosen = 'uc_live_' + 'c'.repeat(32);
  writeFileSync(join(cwd, 'package.json'), JSON.stringify({ name: 'fixture-app' }));
  writeFileSync(join(cwd, '.gitignore'), 'node_modules/\n');

  await runCli(cwd, ['init', '--key', chosen, '--endpoint', srv.url], {
    UPCONTROL_API_KEY: 'uc_live_' + 'a'.repeat(32),
  });
  await srv.close();

  assert.equal(srv.metas.length, 1);
  assert.equal(srv.metas[0].auth, chosen, 'the spec follows --key, not the environment');
  rmSync(cwd, { recursive: true, force: true });
});

test('a long or multi-line description is flattened and capped before it is printed or sent', async () => {
  // The server caps at 200 runes as sent and rejects the whole spec when one
  // value is over, so an ordinary long description would destroy the upload.
  // A newline would also forge a second line inside the block whose entire
  // job is to prove what leaves.
  const srv = await mockBackend();
  const cwd = tmp();
  writeFileSync(
    join(cwd, 'package.json'),
    JSON.stringify({ name: 'fixture-app', description: 'first\nsecond ' + 'x'.repeat(400) }),
  );
  writeFileSync(join(cwd, '.gitignore'), 'node_modules/\n');

  const stdout = await runCli(cwd, ['init', '--endpoint', srv.url]);
  await srv.close();

  const sent = (srv.metas[0].body as { description: string }).description;
  assert.equal([...sent].length, 200, 'capped to the 200 runes the server accepts');
  assert.ok(!sent.includes('\n'), 'no newline survives into the payload');
  assert.ok(stdout.includes('  description ' + sent), 'the block prints exactly what was sent');
  rmSync(cwd, { recursive: true, force: true });
});

test('a package.json describing nothing about the product uploads nothing', async () => {
  // PUT replaces the whole spec, so sending {runtime, language} - two facts
  // about the machine that ran init - would overwrite a good spec with
  // nothing about the product.
  const srv = await mockBackend();
  const cwd = tmp();
  writeFileSync(join(cwd, 'package.json'), JSON.stringify({ version: '1.0.0' }));
  writeFileSync(join(cwd, '.gitignore'), 'node_modules/\n');

  const stdout = await runCli(cwd, ['init', '--endpoint', srv.url]);
  await srv.close();

  assert.equal(srv.metas.length, 0, 'nothing descriptive - nothing sent');
  assert.ok(!stdout.includes('project spec'), 'and nothing claimed on stdout either');
  rmSync(cwd, { recursive: true, force: true });
});

test('init --no-meta prints nothing about the spec and sends nothing', async () => {
  const srv = await mockBackend();
  const cwd = tmp();
  writeFileSync(join(cwd, 'package.json'), JSON.stringify({ name: 'fixture-app' }));
  writeFileSync(join(cwd, '.gitignore'), 'node_modules/\n');

  const stdout = await runCli(cwd, ['init', '--no-meta', '--endpoint', srv.url]);
  await srv.close();

  assert.ok(!stdout.includes('project spec'), '--no-meta must skip the transparency block too');
  assert.equal(srv.metas.length, 0, '--no-meta must not PUT');
  assert.equal(JSON.parse(stdout.trim().split('\n').pop()!).success, true);
  rmSync(cwd, { recursive: true, force: true });
});

test('a refused meta upload never fails init, and no package.json sends nothing', async () => {
  const srv = await mockBackend(400);
  const cwd = tmp();
  writeFileSync(join(cwd, 'package.json'), JSON.stringify({ name: 'fixture-app' }));
  writeFileSync(join(cwd, '.gitignore'), 'node_modules/\n');

  const stdout = await runCli(cwd, ['init', '--endpoint', srv.url]);
  assert.equal(srv.metas.length, 1, 'the upload was attempted');
  assert.equal(JSON.parse(stdout.trim().split('\n').pop()!).success, true, 'a 400 must not fail the install');

  const bare = tmp();
  const bareOut = await runCli(bare, ['init', '--endpoint', srv.url]);
  await srv.close();
  assert.ok(!existsSync(join(bare, 'package.json')));
  assert.ok(!bareOut.includes('project spec'), 'nothing collectible - nothing printed');
  assert.equal(JSON.parse(bareOut.trim().split('\n').pop()!).success, true);
  rmSync(cwd, { recursive: true, force: true });
  rmSync(bare, { recursive: true, force: true });
});
