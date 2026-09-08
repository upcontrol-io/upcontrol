import test from 'node:test';
import assert from 'node:assert/strict';
import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import { readFileSync } from 'node:fs';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

// CLI_VERSION is its own constant because the built dist cannot import
// package.json (it is deliberately outside the package's exports). Nothing
// enforced the two matching, so 0.1.3 shipped reporting itself as 0.1.2 in
// `status` and in the cli_version field every install sends.
//
// The check asks the BUILT binary rather than importing the constant, which is
// what the version claim is actually about: what a user gets from
// `npx upcontrol --version`. It used to import `../dist/net.js`, and that never
// typechecked — `tsconfig.json` sets `declaration: false`, so no `.d.ts` is ever
// emitted beside it. Spawning needs no declarations, and every other test that
// touches the artifact spawns it too.

const execFileP = promisify(execFile);
const here = dirname(fileURLToPath(import.meta.url));
const cli = join(here, '..', 'dist', 'main.js');

test('the built CLI reports the published package version', async () => {
  const pkg = JSON.parse(
    readFileSync(join(here, '..', 'package.json'), 'utf8'),
  ) as { version: string };
  const { stdout } = await execFileP(process.execPath, [cli, '--version']);
  assert.equal(stdout.trim(), pkg.version);
});
