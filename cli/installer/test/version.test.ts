import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { CLI_VERSION } from '../dist/net.js';

// CLI_VERSION is its own constant because the built dist cannot import
// package.json (it is deliberately outside the package's exports). Nothing
// enforced the two matching, so 0.1.3 shipped reporting itself as 0.1.2 in
// `status` and in the cli_version field every install sends.
test('CLI_VERSION matches the published package version', () => {
  const pkg = JSON.parse(
    readFileSync(fileURLToPath(new URL('../package.json', import.meta.url)), 'utf8'),
  );
  assert.equal(CLI_VERSION, pkg.version);
});
