import test from 'node:test';
import assert from 'node:assert/strict';
import { execFile } from 'node:child_process';
import { createServer } from 'node:http';
import type { AddressInfo } from 'node:net';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { promisify } from 'node:util';
import { releaseFrom } from '../dist/esm/release.js';

// app_started's release: the first non-empty platform variable, trimmed and cut to 64 whole
// characters. releaseFrom takes the environment as an argument, so these never touch process.env.

// Listed here on purpose and not imported: the order is the contract, and a test that read it
// from the implementation could not notice it moving.
const NAMES = [
  'UPCONTROL_RELEASE',
  'VERCEL_GIT_COMMIT_SHA',
  'RENDER_GIT_COMMIT',
  'RAILWAY_GIT_COMMIT_SHA',
  'HEROKU_SLUG_COMMIT',
  'FLY_IMAGE_REF',
  'K_REVISION',
];

test('the first variable set wins, in the documented order', () => {
  NAMES.forEach((name, i) => {
    // Every variable from this one on is set, so only the order can pick the winner.
    const env = Object.fromEntries(NAMES.slice(i).map((n) => [n, 'from-' + n]));
    assert.equal(releaseFrom(env), 'from-' + name);
  });
});

test('nothing set is undefined, never an empty string', () => {
  assert.equal(releaseFrom({}), undefined);
  assert.equal(releaseFrom({ NODE_ENV: 'production', VERCEL_ENV: 'production' }), undefined);
});

test('an empty or whitespace-only value is skipped to the next variable', () => {
  assert.equal(
    releaseFrom({
      UPCONTROL_RELEASE: '',
      VERCEL_GIT_COMMIT_SHA: ' \t\r\n ',
      RENDER_GIT_COMMIT: undefined,
      RAILWAY_GIT_COMMIT_SHA: 'abc123',
      K_REVISION: 'later',
    }),
    'abc123',
  );
  assert.equal(releaseFrom({ UPCONTROL_RELEASE: '  ', K_REVISION: '' }), undefined);
});

test('surrounding whitespace is trimmed, inner whitespace stays', () => {
  assert.equal(releaseFrom({ UPCONTROL_RELEASE: '  v2.4.1\n' }), 'v2.4.1');
  assert.equal(releaseFrom({ VERCEL_GIT_COMMIT_SHA: '\t0123abc\r\n' }), '0123abc');
  assert.equal(releaseFrom({ UPCONTROL_RELEASE: ' spring  release ' }), 'spring  release');
});

test('the value is cut to its first 64 characters, after the trim', () => {
  const sha = 'a94a8fe5ccb19ba61c4c0873d391e987982fbbd3';
  assert.equal(releaseFrom({ VERCEL_GIT_COMMIT_SHA: sha }), sha);

  const at64 = 'x'.repeat(64);
  assert.equal(releaseFrom({ UPCONTROL_RELEASE: at64 }), at64, '64 is kept whole');
  assert.equal(releaseFrom({ UPCONTROL_RELEASE: at64 + 'y' }), at64, '65 loses the last');
  assert.equal(releaseFrom({ FLY_IMAGE_REF: 'registry.fly.io/app:' + '0'.repeat(200) })?.length, 64);
  // Trim first, then cut: the padding does not eat into the 64.
  assert.equal(releaseFrom({ UPCONTROL_RELEASE: '   ' + at64 + 'y  ' }), at64);
});

test('the cut never leaves half of a surrogate pair', () => {
  const smile = '\u{1F600}'; // two UTF-16 units, one character

  // The pair is the 64th character: kept whole, so the string is 65 units long.
  const kept = 'a'.repeat(63) + smile;
  assert.equal(releaseFrom({ UPCONTROL_RELEASE: kept + 'tail' }), kept);

  // The pair starts at the 65th character: it goes entirely, not as a lone high surrogate.
  assert.equal(releaseFrom({ UPCONTROL_RELEASE: 'a'.repeat(64) + smile }), 'a'.repeat(64));

  // 64 whole pairs out of 70.
  const out = releaseFrom({ UPCONTROL_RELEASE: smile.repeat(70) });
  assert.equal(out, smile.repeat(64));
  assert.equal(Array.from(out!).length, 64);
});

// The wiring, through the built auto entry in a fresh process: it registers process handlers and
// guards itself once per process, so a second case cannot import it again in this one.
const run = promisify(execFile);
const sdkDir = fileURLToPath(new URL('..', import.meta.url));

async function appStarted(vars: Record<string, string>): Promise<Record<string, unknown>> {
  const bodies: string[] = [];
  const server = createServer(async (req, res) => {
    let body = '';
    for await (const chunk of req) body += chunk;
    bodies.push(body);
    res.writeHead(200, { 'content-type': 'application/json' });
    res.end(JSON.stringify({ accepted: body.split('\n').length }));
  });
  await new Promise<void>((r) => server.listen(0, '127.0.0.1', r));

  const env: NodeJS.ProcessEnv = { ...process.env };
  for (const n of NAMES) delete env[n];
  Object.assign(env, vars, {
    UPCONTROL_API_KEY: 'uc_live_test',
    UPCONTROL_ENDPOINT: `http://127.0.0.1:${(server.address() as AddressInfo).port}`,
    NODE_ENV: 'test',
  });
  try {
    // cwd is the package, so auto.js reads this package.json for `version`.
    await run(process.execPath, [fileURLToPath(new URL('../dist/esm/auto.js', import.meta.url))], {
      cwd: sdkDir,
      env,
    });
  } finally {
    await new Promise<void>((r) => server.close(() => r()));
  }
  const lines = bodies.join('\n').split('\n').map((l) => JSON.parse(l));
  const started = lines.find((l) => l.msg === 'app_started');
  assert.ok(started, 'app_started reached the wire');
  return started;
}

test('app_started carries the release on the wire, and no field when nothing names one', async () => {
  const pkg = JSON.parse(readFileSync(new URL('../package.json', import.meta.url), 'utf8'));
  const sha = 'a94a8fe5ccb19ba61c4c0873d391e987982fbbd3';

  const named = await appStarted({ VERCEL_GIT_COMMIT_SHA: ` ${sha}\n` });
  assert.equal(named.release, sha);
  assert.equal(named.version, pkg.version);
  assert.equal(named.env, 'test');

  const bare = await appStarted({});
  assert.equal('release' in bare, false);
  assert.equal(bare.version, pkg.version);
  assert.equal(bare.env, 'test');
});
