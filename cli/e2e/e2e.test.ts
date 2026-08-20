// The whole vertical against the real docker stack: mint an anonymous project,
// write the key to .env, push events through the SDK, watch verify flip.
// Gated like front's live suite: runs only under UC_CLI_E2E=1 with the stack
// answering /health, and is skipped (not failed) otherwise.
//
//   docker compose up -d   (repo root: ./up.ps1 or ./up.sh)
//   UC_CLI_E2E=1 node --test cli/e2e/
//
// Build both packages first: `npm run build` in cli/sdk and cli/installer.

import test from 'node:test';
import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { mkdtempSync, readFileSync, writeFileSync, existsSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';
import { pathToFileURL } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));
const cli = join(here, '..', 'installer', 'dist', 'main.js');
const sdkEsm = join(here, '..', 'sdk', 'dist', 'esm', 'index.js');
const ENDPOINT = process.env.UPCONTROL_ENDPOINT || 'http://localhost';

const gated = process.env.UC_CLI_E2E === '1';
let stackUp = false;
if (gated) {
  try {
    const res = await fetch(ENDPOINT + '/health', { signal: AbortSignal.timeout(3000) });
    stackUp = res.ok;
  } catch {
    stackUp = false;
  }
}
const run = gated && stackUp;
if (gated && !stackUp) {
  console.error(`UC_CLI_E2E=1 but ${ENDPOINT}/health does not answer - start the stack first (./up.ps1)`);
}

test('init -> SDK push -> verify, end to end', { skip: !run, timeout: 120_000 }, async () => {
  assert.ok(existsSync(cli), 'build cli/installer first (npm run build)');
  assert.ok(existsSync(sdkEsm), 'build cli/sdk first (npm run build)');

  const cwd = mkdtempSync(join(tmpdir(), 'uc-e2e-'));
  writeFileSync(join(cwd, 'package.json'), JSON.stringify({ name: 'fixture-app', version: '1.0.0' }, null, 2));
  writeFileSync(join(cwd, '.gitignore'), 'node_modules/\n');

  // 1. init in agent mode: mints a real key against the real backend.
  const initOut = execFileSync(process.execPath, [cli, 'init', '--endpoint', ENDPOINT], {
    cwd,
    encoding: 'utf8',
    env: { ...process.env, AI_AGENT: 'claude', UPCONTROL_API_KEY: '' },
  });
  const init = JSON.parse(initOut.trim().split('\n').pop()!);
  assert.equal(init.success, true);
  assert.equal(init.key.source, 'minted', JSON.stringify(init.key));
  assert.ok(init.key.claimUrl, 'claim url must ride the result');
  assert.ok(!initOut.includes('uc_live_'), 'the key must never appear in output');
  assert.ok(existsSync(join(cwd, '.claude', 'skills', 'upcontrol', 'SKILL.md')));
  assert.ok(existsSync(join(cwd, '.agents', 'skills', 'upcontrol', 'SKILL.md')));
  const key = readFileSync(join(cwd, '.env'), 'utf8').match(/UPCONTROL_API_KEY=(\S+)/)?.[1];
  assert.ok(key?.startsWith('uc_live_'), '.env must carry the minted key');

  // 2. The "instrumented app": pushes canonical events through the real SDK.
  const app = `
    import { track, flush } from ${JSON.stringify(pathToFileURL(sdkEsm).href)};
    track('payment_succeeded', { provider: 'stripe', currency: 'usd', livemode: false });
    track('signup', {});
    track('deploy', { version: '1.0.0', env: 'test' });
    await flush();
  `;
  writeFileSync(join(cwd, 'app.mjs'), app);
  execFileSync(process.execPath, ['app.mjs'], {
    cwd,
    encoding: 'utf8',
    env: { ...process.env, UPCONTROL_API_KEY: key, UPCONTROL_ENDPOINT: ENDPOINT },
  });

  // 3. verify: the SDK's first batch carried install_verified; the pipeline
  // (WAL -> batcher -> ClickHouse) lands it within seconds.
  const verifyOut = execFileSync(
    process.execPath,
    [cli, 'verify', '--endpoint', ENDPOINT, '--json', '--timeout', '60'],
    { cwd, encoding: 'utf8', env: { ...process.env, UPCONTROL_API_KEY: '', AI_AGENT: 'claude' } },
  );
  const verify = JSON.parse(verifyOut.trim().split('\n').pop()!);
  assert.equal(verify.verified, true, verifyOut);
  assert.ok(verify.lines >= 3, `expected the pushed lines in the window, got ${verify.lines}`);
  const names = ((verify.recent ?? []) as { name: string }[]).map((r) => r.name);
  assert.ok(names.includes('payment_succeeded'), `recent events: ${names.join(', ')}`);

  // 4. Re-run init: idempotent, nothing rewritten, existing key untouched.
  const secondOut = execFileSync(process.execPath, [cli, 'init', '--endpoint', ENDPOINT], {
    cwd,
    encoding: 'utf8',
    env: { ...process.env, AI_AGENT: 'claude', UPCONTROL_API_KEY: '' },
  });
  const second = JSON.parse(secondOut.trim().split('\n').pop()!);
  assert.equal(second.skill.updated, false, 're-init must not rewrite a fresh skill');
  assert.equal(second.key.source, 'dotenv', 're-init must keep the existing key');

  rmSync(cwd, { recursive: true, force: true });
});

test('anonymous mint is per-IP throttled', { skip: !run, timeout: 30_000 }, async () => {
  // The e2e above just minted from this IP; a second immediate mint must 429.
  const res = await fetch(ENDPOINT + '/v1/projects/anonymous', {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: '{}',
  });
  assert.equal(res.status, 429, 'second mint inside the cooldown must be refused');
});
