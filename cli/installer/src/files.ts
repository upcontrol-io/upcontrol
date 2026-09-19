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

export const SDK_PIN = '1.0.0';

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

interface SkillResult {
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

interface DepResult {
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

type KeySource = 'env' | 'dotenv' | 'flag' | 'token' | 'minted' | 'none';

export function findKey(cwd: string, env: NodeJS.ProcessEnv = process.env): KeySource {
  if (env.UPCONTROL_API_KEY?.trim()) return 'env';
  if (readDotenvKey(cwd)) return 'dotenv';
  return 'none';
}

// readDotenv reads one variable from .env; null when it is absent or empty.
// name is one of the two UPCONTROL_ constants below, never input.
function readDotenv(cwd: string, name: string): string | null {
  const p = join(cwd, '.env');
  if (!existsSync(p)) return null;
  const re = new RegExp(`^\\s*(?:export\\s+)?${name}\\s*=\\s*(.+?)\\s*$`);
  for (const line of readFileSync(p, 'utf8').split(/\r?\n/)) {
    const m = line.match(re);
    if (m) {
      const v = m[1].replace(/^["']|["']$/g, '');
      if (v) return v;
    }
  }
  return null;
}

export function readDotenvKey(cwd: string): string | null {
  return readDotenv(cwd, 'UPCONTROL_API_KEY');
}

interface GitignoreResult {
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

// writeDotenv appends (or creates) .env with one variable. Call ONLY after
// ensureEnvIgnored. The value is never printed by any caller.
function writeDotenv(cwd: string, name: string, value: string): void {
  const p = join(cwd, '.env');
  if (existsSync(p)) {
    if (readDotenv(cwd, name)) return; // present already - never overwrite silently
    const raw = readFileSync(p, 'utf8');
    const sep = raw.length === 0 || raw.endsWith('\n') ? '' : '\n';
    appendFileSync(p, `${sep}${name}=${value}\n`);
    return;
  }
  writeFileSync(p, `${name}=${value}\n`);
}

export function writeDotenvKey(cwd: string, key: string): void {
  writeDotenv(cwd, 'UPCONTROL_API_KEY', key);
}

// The public web key rides the same .env (so a rerun reuses it) and the same
// never-overwrite rule: a value already there is the one the site's tag carries.
// .env stays ignored as a whole, public key included.
export function readDotenvPublicKey(cwd: string): string | null {
  return readDotenv(cwd, 'UPCONTROL_PUBLIC_KEY');
}

export function writeDotenvPublicKey(cwd: string, key: string): void {
  writeDotenv(cwd, 'UPCONTROL_PUBLIC_KEY', key);
}

/** One site argument as the origins a public key is bound to, [] when the
 *  argument is not a usable address. Ported from the app's website card so both
 *  doors scope a key identically: a bare host gets `http://` only for localhost
 *  and 127.*, `https://` otherwise; a real domain also gets its www/apex twin,
 *  because a site answers on both and a key minted for one is silently refused
 *  on the other. */
export function siteOrigins(site: string): string[] {
  const out: string[] = [];
  try {
    const typed = site.trim();
    const withScheme =
      typed.includes('://') ? typed : `${typed.startsWith('localhost') || typed.startsWith('127.') ? 'http' : 'https'}://${typed}`;
    const url = new URL(withScheme);
    // An opaque origin ("null") would be compared byte for byte by the server and
    // match every sandboxed page in the world: exactly the unscoped key the kinds
    // exist to prevent.
    if (url.protocol !== 'http:' && url.protocol !== 'https:') return [];
    // A browser never sends "*" in an Origin, so a wildcard host mints a key
    // that authenticates nowhere and is printed back as if it were a scope.
    if (!/^[a-z0-9.-]+$/.test(url.hostname) && !url.hostname.startsWith('[')) return [];
    out.push(url.origin);
    const host = url.hostname;
    if (host !== 'localhost' && !/^[\d.]+$/.test(host) && !host.startsWith('[')) {
      const twin = host.startsWith('www.') ? host.slice(4) : `www.${host}`;
      out.push(`${url.protocol}//${twin}${url.port ? `:${url.port}` : ''}`);
    }
  } catch {
    // An address the parser cannot read is refused whole: guessing at it is how a key
    // ends up scoped to an origin nobody meant.
    return [];
  }
  return out;
}
