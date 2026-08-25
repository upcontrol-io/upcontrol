// The file-writing half of init: skill install, the pinned SDK dependency, key placement.
// The key goes only into .env, only after .gitignore covers it, and is never echoed.

import {
  cpSync,
  existsSync,
  mkdirSync,
  readdirSync,
  readFileSync,
  statSync,
  writeFileSync,
  appendFileSync,
} from 'node:fs';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

export const SDK_PIN = '0.1.0';

export function bundledSkillDir(): string {
  return join(dirname(fileURLToPath(import.meta.url)), '..', 'skill');
}

// Two copies by default, not a symlink: symlinks need privileges on Windows,
// and a broken link is worse than a duplicate byte-checked on every run.
function skillTargets(cwd: string, copilot: boolean): string[] {
  const t = [join(cwd, '.claude', 'skills', 'upcontrol'), join(cwd, '.agents', 'skills', 'upcontrol')];
  if (copilot) t.push(join(cwd, '.github', 'skills', 'upcontrol'));
  return t;
}

function dirEqual(a: string, b: string): boolean {
  if (!existsSync(b)) return false;
  const walk = (root: string, rel = ''): string[] => {
    const out: string[] = [];
    for (const e of readdirSync(join(root, rel))) {
      const r = join(rel, e);
      if (statSync(join(root, r)).isDirectory()) out.push(...walk(root, r));
      else out.push(r);
    }
    return out.sort();
  };
  const fa = walk(a);
  const fb = walk(b);
  if (fa.join('|') !== fb.join('|')) return false;
  return fa.every((f) => readFileSync(join(a, f)).equals(readFileSync(join(b, f))));
}

export interface SkillResult {
  installed: string[];
  updated: boolean;
}

export function installSkill(cwd: string, copilot: boolean): SkillResult {
  const src = bundledSkillDir();
  const installed: string[] = [];
  let updated = false;
  for (const target of skillTargets(cwd, copilot)) {
    if (!dirEqual(src, target)) {
      mkdirSync(dirname(target), { recursive: true });
      cpSync(src, target, { recursive: true, force: true });
      updated = true;
    }
    installed.push(target);
  }
  return { installed, updated };
}

export function skillFresh(cwd: string): boolean {
  const src = bundledSkillDir();
  const claude = join(cwd, '.claude', 'skills', 'upcontrol');
  const agents = join(cwd, '.agents', 'skills', 'upcontrol');
  return dirEqual(src, claude) && dirEqual(src, agents);
}

export interface DepResult {
  added: boolean;
  present: boolean;
}

// Pins the SDK exactly; an existing entry is left alone: loosening or
// tightening somebody's dependency is not init's call.
export function pinSdkDependency(cwd: string): DepResult {
  const pkgPath = join(cwd, 'package.json');
  if (!existsSync(pkgPath)) return { added: false, present: false };
  const raw = readFileSync(pkgPath, 'utf8');
  let pkg: Record<string, any>;
  try {
    pkg = JSON.parse(raw);
  } catch {
    return { added: false, present: false };
  }
  if (pkg.dependencies?.['@upcontrol/sdk'] || pkg.devDependencies?.['@upcontrol/sdk']) {
    return { added: false, present: true };
  }
  pkg.dependencies = { ...(pkg.dependencies ?? {}), '@upcontrol/sdk': SDK_PIN };
  const indent = raw.match(/^(\s+)"/m)?.[1] ?? '  ';
  const eol = raw.includes('\r\n') ? '\r\n' : '\n';
  writeFileSync(pkgPath, JSON.stringify(pkg, null, indent).replace(/\n/g, eol) + eol);
  return { added: true, present: true };
}

export type KeySource = 'env' | 'dotenv' | 'flag' | 'token' | 'minted' | 'none';

export function findKey(cwd: string, env: NodeJS.ProcessEnv = process.env): KeySource {
  if (env.UPCONTROL_API_KEY?.trim()) return 'env';
  if (readDotenvKey(cwd)) return 'dotenv';
  return 'none';
}

export function readDotenvKey(cwd: string): string | null {
  const p = join(cwd, '.env');
  if (!existsSync(p)) return null;
  for (const line of readFileSync(p, 'utf8').split(/\r?\n/)) {
    const m = line.match(/^\s*(?:export\s+)?UPCONTROL_API_KEY\s*=\s*(.+?)\s*$/);
    if (m) {
      const v = m[1].replace(/^["']|["']$/g, '');
      if (v) return v;
    }
  }
  return null;
}

export interface GitignoreResult {
  covered: boolean;
  fixed: boolean;
}

// Guarantees `.env` cannot be committed before the key is written. The entry
// is written even without .git: repos get initialized after installers run.
export function ensureEnvIgnored(cwd: string): GitignoreResult {
  const p = join(cwd, '.gitignore');
  const covers = (line: string): boolean => {
    const t = line.trim();
    return t === '.env' || t === '.env*' || t === '/.env' || t === '*.env' || t === '.env.*';
  };
  if (existsSync(p)) {
    const lines = readFileSync(p, 'utf8').split(/\r?\n/);
    if (lines.some(covers)) return { covered: true, fixed: false };
    appendFileSync(p, '\n# upcontrol: the API key lives in .env and must never be committed\n.env\n');
    return { covered: true, fixed: true };
  }
  writeFileSync(p, '# upcontrol: the API key lives in .env and must never be committed\n.env\n');
  return { covered: true, fixed: true };
}

// writeDotenvKey appends (or creates) .env with the key. Call ONLY after
// ensureEnvIgnored. The value is never printed by any caller.
export function writeDotenvKey(cwd: string, key: string): void {
  const p = join(cwd, '.env');
  if (existsSync(p)) {
    const raw = readFileSync(p, 'utf8');
    if (readDotenvKey(cwd)) return; // present already - never overwrite silently
    const sep = raw.length === 0 || raw.endsWith('\n') ? '' : '\n';
    appendFileSync(p, `${sep}UPCONTROL_API_KEY=${key}\n`);
    return;
  }
  writeFileSync(p, `UPCONTROL_API_KEY=${key}\n`);
}
