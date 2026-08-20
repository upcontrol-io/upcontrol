import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, readFileSync, writeFileSync, existsSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import {
  installSkill,
  skillFresh,
  ensureEnvIgnored,
  writeDotenvKey,
  readDotenvKey,
  pinSdkDependency,
} from '../src/files.ts';
import { detect } from '../src/detect.ts';

const tmp = () => mkdtempSync(join(tmpdir(), 'uc-cli-'));

test('installSkill writes both agent dirs and is idempotent', () => {
  const cwd = tmp();
  const first = installSkill(cwd, false);
  assert.equal(first.updated, true);
  assert.ok(existsSync(join(cwd, '.claude', 'skills', 'upcontrol', 'SKILL.md')));
  assert.ok(existsSync(join(cwd, '.agents', 'skills', 'upcontrol', 'SKILL.md')));
  assert.ok(existsSync(join(cwd, '.claude', 'skills', 'upcontrol', 'references', 'dictionary.md')));
  assert.equal(skillFresh(cwd), true);
  const second = installSkill(cwd, false);
  assert.equal(second.updated, false, 're-run must be a no-op');
  rmSync(cwd, { recursive: true, force: true });
});

test('a locally modified skill is stale and gets refreshed', () => {
  const cwd = tmp();
  installSkill(cwd, false);
  writeFileSync(join(cwd, '.claude', 'skills', 'upcontrol', 'SKILL.md'), 'tampered');
  assert.equal(skillFresh(cwd), false);
  const again = installSkill(cwd, false);
  assert.equal(again.updated, true);
  assert.equal(skillFresh(cwd), true);
  rmSync(cwd, { recursive: true, force: true });
});

test('copilot flag adds the third copy', () => {
  const cwd = tmp();
  installSkill(cwd, true);
  assert.ok(existsSync(join(cwd, '.github', 'skills', 'upcontrol', 'SKILL.md')));
  rmSync(cwd, { recursive: true, force: true });
});

test('ensureEnvIgnored appends when .gitignore misses .env and reports the fix', () => {
  const cwd = tmp();
  writeFileSync(join(cwd, '.gitignore'), 'node_modules/\n');
  const r = ensureEnvIgnored(cwd);
  assert.deepEqual(r, { covered: true, fixed: true });
  assert.ok(readFileSync(join(cwd, '.gitignore'), 'utf8').split(/\r?\n/).includes('.env'));
  const r2 = ensureEnvIgnored(cwd);
  assert.deepEqual(r2, { covered: true, fixed: false }, 'second run must not append again');
  rmSync(cwd, { recursive: true, force: true });
});

test('ensureEnvIgnored creates .gitignore when absent', () => {
  const cwd = tmp();
  const r = ensureEnvIgnored(cwd);
  assert.equal(r.fixed, true);
  assert.ok(readFileSync(join(cwd, '.gitignore'), 'utf8').includes('.env'));
  rmSync(cwd, { recursive: true, force: true });
});

test('writeDotenvKey appends without clobbering and never overwrites an existing key', () => {
  const cwd = tmp();
  writeFileSync(join(cwd, '.env'), 'DATABASE_URL=postgres://x\n');
  writeDotenvKey(cwd, 'uc_live_abc');
  assert.equal(readDotenvKey(cwd), 'uc_live_abc');
  assert.ok(readFileSync(join(cwd, '.env'), 'utf8').startsWith('DATABASE_URL='), 'existing lines survive');
  writeDotenvKey(cwd, 'uc_live_OTHER');
  assert.equal(readDotenvKey(cwd), 'uc_live_abc', 'an existing key is never silently replaced');
  rmSync(cwd, { recursive: true, force: true });
});

test('pinSdkDependency pins exactly and leaves an existing entry alone', () => {
  const cwd = tmp();
  writeFileSync(join(cwd, 'package.json'), JSON.stringify({ name: 'x', dependencies: { express: '^4' } }, null, 2));
  const r = pinSdkDependency(cwd);
  assert.equal(r.added, true);
  const pkg = JSON.parse(readFileSync(join(cwd, 'package.json'), 'utf8'));
  assert.equal(pkg.dependencies['@upcontrol/sdk'], '0.1.0');
  assert.match(pkg.dependencies['@upcontrol/sdk'], /^\d/, 'exact pin, no range');
  const r2 = pinSdkDependency(cwd);
  assert.equal(r2.added, false);
  rmSync(cwd, { recursive: true, force: true });
});

test('detect: explicit env wins, markers map, CI and pipes are non-interactive', () => {
  assert.deepEqual(detect({ AI_AGENT: 'claude' }, true), { mode: 'agent', agent: 'claude-code' });
  assert.deepEqual(detect({ CLAUDECODE: '1' }, true), { mode: 'agent', agent: 'claude-code' });
  assert.deepEqual(detect({ CURSOR_TRACE_ID: 'x' }, true), { mode: 'agent', agent: 'cursor' });
  assert.deepEqual(detect({ GEMINI_CLI: '1' }, true), { mode: 'agent', agent: 'gemini-cli' });
  assert.deepEqual(detect({ CI: 'true' }, true), { mode: 'ci', agent: null });
  assert.equal(detect({}, false).mode, 'agent', 'a pipe cannot answer prompts');
  assert.equal(detect({}, true).mode, 'interactive');
});
