// The installer-collected project spec: ONLY these five fields - never
// dependency lists, file paths, git remotes, env values or code.

import { existsSync, readFileSync } from 'node:fs';
import { join } from 'node:path';

export interface ProjectSpec {
  name?: string;
  description?: string;
  framework?: string;
  runtime?: string;
  language?: string;
}

// Ordered: the first entry present in dependencies or devDependencies wins,
// so a Next app that also depends on react reports "next", not "react".
const FRAMEWORKS: ReadonlyArray<readonly [string, string]> = [
  ['next', 'next'],
  ['nuxt', 'nuxt'],
  ['@nestjs/core', 'nest'],
  ['express', 'express'],
  ['fastify', 'fastify'],
  ['koa', 'koa'],
  ['svelte', 'svelte'],
  ['vue', 'vue'],
  ['react', 'react'],
];

// The server rejects the whole spec when one value is over 200 runes, so a
// long description must be trimmed here or it destroys the upload.
const MAX_VALUE_RUNES = 200;

// One line, always: a newline would forge a second line inside the block
// whose job is to prove what is sent, and the server flattens it anyway.
function oneLine(value: string): string {
  return value.replace(/[\r\n\u2028\u2029]+/g, ' ').replace(/\s+/g, ' ').trim();
}

function clean(value: string): string {
  const flat = oneLine(value);
  return [...flat].length > MAX_VALUE_RUNES ? [...flat].slice(0, MAX_VALUE_RUNES).join('') : flat;
}

// A spec must say something about the product: PUT replaces, so a spec with
// only machine-derived runtime/language would overwrite a good one.
const DESCRIPTIVE = ['name', 'description', 'framework'] as const;

export function isDescriptive(spec: ProjectSpec): boolean {
  return DESCRIPTIVE.some((f) => (spec[f] ?? '').length > 0);
}

// null means nothing was collectible (no readable package.json); the caller
// then skips the upload - meta must never fail or even delay the install.
export function collectSpec(cwd: string, runtime: string): ProjectSpec | null {
  const pkgPath = join(cwd, 'package.json');
  if (!existsSync(pkgPath)) return null;
  let pkg: Record<string, unknown>;
  try {
    pkg = JSON.parse(readFileSync(pkgPath, 'utf8')) as Record<string, unknown>;
  } catch {
    return null;
  }
  if (typeof pkg !== 'object' || pkg === null) return null;

  const spec: ProjectSpec = {
    runtime,
    language: existsSync(join(cwd, 'tsconfig.json')) ? 'typescript' : 'javascript',
  };
  for (const field of ['name', 'description'] as const) {
    const v = pkg[field];
    if (typeof v !== 'string') continue;
    // Flattened and capped here, so what the block prints, what the server
    // stores and what the model reads are one string.
    const value = clean(v);
    if (value) spec[field] = value;
  }
  const depGroups = [pkg.dependencies, pkg.devDependencies].filter(
    (g): g is Record<string, unknown> => typeof g === 'object' && g !== null,
  );
  for (const [dep, framework] of FRAMEWORKS) {
    if (depGroups.some((g) => g[dep] !== undefined)) {
      spec.framework = framework;
      break;
    }
  }
  return spec;
}

const FIELDS = ['name', 'description', 'framework', 'runtime', 'language'] as const;

export function formatSpec(spec: ProjectSpec): string {
  const lines = ['project spec (sent so AI log analysis knows your stack; nothing else is read):'];
  for (const f of FIELDS) {
    const v = spec[f];
    if (v !== undefined) lines.push(`  ${f.padEnd(11)} ${v}`);
  }
  lines.push('  (skip with --no-meta)');
  return lines.join('\n');
}
