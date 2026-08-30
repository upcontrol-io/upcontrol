import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { SDK_VERSION } from '../dist/esm/client.js';

// SDK_VERSION is its own constant because the built dist cannot import
// package.json (it is deliberately outside the package's exports). Nothing
// enforced the two matching, so 0.1.1 shipped reporting js/0.1.0 on the wire
// and in install_verified.
test('SDK_VERSION matches the published package version', () => {
  const pkg = JSON.parse(
    readFileSync(fileURLToPath(new URL('../package.json', import.meta.url)), 'utf8'),
  );
  assert.equal(SDK_VERSION, pkg.version);
});
