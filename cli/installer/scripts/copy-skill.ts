// The skill's single source of truth is cli/plugin/; the installer bundles a
// verbatim copy at build time so the published tarball is self-contained and
// version-locked (freshness is a byte comparison against this copy).
import { cpSync, rmSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const src = join(here, '..', '..', 'plugin');
const dst = join(here, '..', 'skill');

rmSync(dst, { recursive: true, force: true });
cpSync(src, dst, { recursive: true });
console.log('skill: copied from cli/plugin');
