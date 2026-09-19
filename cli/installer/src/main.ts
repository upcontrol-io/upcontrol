#!/usr/bin/env node
// npx upcontrol - the one command. No LLM runs here: the CLI installs the
// skill, pins the SDK, places the key and proves the chain (verify).

import { existsSync, readdirSync, readFileSync } from 'node:fs';
import { join, basename } from 'node:path';
import { detect, type Detection } from './detect.js';
import {
  SDK_PIN,
  bundledSkillDir,
  ensureEnvIgnored,
  findKey,
  installSkill,
  pinSdkDependency,
  readDotenvKey,
  readDotenvPublicKey,
  siteOrigins,
  skillFresh,
  writeDotenvKey,
  writeDotenvPublicKey,
} from './files.js';
import {
  CLI_VERSION,
  appendBoard,
  applyBoard,
  endpointFrom,
  fetchInstallStatus,
  mintAnonymousProject,
  mintPublicKey,
  readBoard,
  redeemInstallToken,
} from './net.js';

interface Flags {
  key?: string;
  token?: string;
  endpoint?: string;
  apply?: string;
  add?: string;
  copilot: boolean;
  noKey: boolean;
  json: boolean;
  web: boolean;
  timeout: number;
  help: boolean;
  version: boolean;
}

function parseArgs(argv: string[]): { cmd: string; args: string[]; flags: Flags } {
  const flags: Flags = { copilot: false, noKey: false, json: false, web: false, timeout: 120, help: false, version: false };
  const rest: string[] = [];
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    switch (a) {
      case '--key':
        flags.key = argv[++i];
        break;
      case '--token':
        flags.token = argv[++i];
        break;
      case '--endpoint':
        flags.endpoint = argv[++i];
        break;
      case '--apply':
        flags.apply = argv[++i];
        break;
      case '--add':
        flags.add = argv[++i];
        break;
      case '--copilot':
        flags.copilot = true;
        break;
      case '--no-key':
        flags.noKey = true;
        break;
      case '--json':
        flags.json = true;
        break;
      case '--web':
        flags.web = true;
        break;
      case '--timeout':
        flags.timeout = Number(argv[++i]) || 120;
        break;
      case '--help':
      case '-h':
        flags.help = true;
        break;
      case '--version':
      case '-v':
        flags.version = true;
        break;
      default:
        rest.push(a);
    }
  }
  const cmd = rest[0] ?? 'init';
  return { cmd, args: rest.slice(1), flags };
}

function out(line: string): void {
  process.stdout.write(line + '\n');
}

// Board failures go to stderr: the plain read's stdout is piped JSON, and an
// error line in it would ride into whatever consumed the pipe.
function err(line: string): void {
  process.stderr.write(line + '\n');
}

const HELP = `upcontrol ${CLI_VERSION} - monitoring wired in by the agent you already use

Usage:
  npx upcontrol [init]     install the agent skill, pin @upcontrol/sdk, provision a key
  npx upcontrol skills     list agent reference topics (skills <topic> prints one)
  npx upcontrol verify     wait until data provably arrives (exit 4 on failure)
  npx upcontrol status     one JSON line: endpoint, key source, skill freshness
  npx upcontrol board      print the project's dashboard layout as JSON
  npx upcontrol web <site> print the script tag for your site (web analytics + heatmaps)

Init flags:
  --token <uct_...>    one-time token from your dashboard's Connect page - lands
                       the key of YOUR project (never echoed, single use)
  --key <uc_live_...>  use this key instead of provisioning one (written to .env, never echoed)
  --no-key             skip key provisioning entirely
  --copilot            also install the skill for GitHub Copilot (.github/skills/)
  --endpoint <url>     override the API endpoint (default: $UPCONTROL_ENDPOINT or https://upcontrol.io)

Verify flags:
  --timeout <sec>      how long to wait (default 120)
  --web                page views pass too: a site-only install, the script tag and no SDK
  --json               machine-readable output

Board flags:
  --apply <file|->     replace the board with the layout in that file
  --add <file|->       append the widgets in that file to the board

  A single dash reads stdin for either flag. On a human-curated board --apply is kept
  as a proposal: open /app/dashboard in the app and press Review to apply it.

The skill teaches your agent event names and placement
rules; say what you want in plain language ("send all my logs to upcontrol")
and review the diff it stages.`;

async function cmdInit(det: Detection, flags: Flags): Promise<number> {
  const cwd = process.cwd();
  const endpoint = endpointFrom(process.env, flags.endpoint);
  const skill = installSkill(cwd, flags.copilot);
  const dep = pinSdkDependency(cwd);

  let keySource = findKey(cwd);
  let claimUrl: string | undefined;
  let keyNote = '';
  // True when this run tried and failed to establish a key: the result says
  // success:false and init exits 1, so no agent wires an app that sends nothing.
  let keyFailed = false;
  const present = keySource === 'env' || keySource === 'dotenv';
  if (present && (flags.token || flags.key)) {
    // A key already here is never redeemed or written over: writeDotenvKey
    // refuses to overwrite, so the request would burn a one-time token for a
    // key that lands nowhere while the app keeps sending to the old project.
    const where = keySource === 'env' ? 'UPCONTROL_API_KEY' : '.env';
    // Nothing is written, but the one anti-leak guarantee still holds on this path:
    // a key that is already in .env must not be committable. Only there: with the key
    // in the environment there is no file to guard, and ensureEnvIgnored would create
    // a .gitignore in a directory that may not even be a repo.
    const gi = keySource === 'dotenv' ? ensureEnvIgnored(cwd) : { fixed: false };
    if (flags.key !== (process.env.UPCONTROL_API_KEY?.trim() || readDotenvKey(cwd))) {
      const what = flags.token ? 'the token was not used' : 'the key from --key was not written';
      keyNote = `a key is already set in ${where} - left as is, and ${what}. Remove that line, and the UPCONTROL_PUBLIC_KEY line if there is one, and rerun to install this project's key.`;
    }
    if (gi.fixed) keyNote = (keyNote ? keyNote + ' ' : '') + '.gitignore did not cover .env - fixed.';
  } else if (flags.token) {
    // The dashboard's one-time token: redeem it for this account's project key.
    // On failure never fall back to the anonymous mint: wrong-project logs.
    const redeemed = await redeemInstallToken(endpoint, flags.token);
    if (redeemed.ok && redeemed.key) {
      const gi = ensureEnvIgnored(cwd);
      writeDotenvKey(cwd, redeemed.key);
      keySource = 'token';
      keyNote = gi.fixed ? '.gitignore did not cover .env - fixed, then wrote the key' : 'written to .env';
    } else if (redeemed.error === 'unreachable') {
      keyFailed = true;
      keyNote = `backend unreachable at ${endpoint} - check UPCONTROL_ENDPOINT and retry`;
    } else if (redeemed.status === 429) {
      keyFailed = true;
      keyNote = 'redeem throttled - the token is still valid, run the same command again in 30s';
    } else {
      keyFailed = true;
      keyNote = 'the install token was already used or expired - generate a fresh command in /app/sources';
    }
  } else if (flags.key) {
    const gi = ensureEnvIgnored(cwd);
    writeDotenvKey(cwd, flags.key);
    keySource = 'flag';
    keyNote = gi.fixed ? '.gitignore did not cover .env - fixed, then wrote the key' : 'written to .env';
  } else if (keySource === 'none' && !flags.noKey) {
    const mint = await mintAnonymousProject(endpoint, det.agent);
    if (mint.ok && mint.key) {
      const gi = ensureEnvIgnored(cwd);
      writeDotenvKey(cwd, mint.key);
      keySource = 'minted';
      claimUrl = mint.claimUrl;
      keyNote = gi.fixed ? '.gitignore did not cover .env - fixed, then wrote the key' : 'written to .env';
    } else if (mint.status === 429) {
      keyFailed = true;
      keyNote = 'provisioning throttled - run `npx upcontrol init` again in 30s';
    } else {
      keyFailed = true;
      keyNote = `backend unreachable at ${endpoint} - set UPCONTROL_API_KEY or paste a key from /app/sources#key`;
    }
  }

  const result = {
    success: !keyFailed,
    mode: det.mode,
    agent: det.agent,
    skill: { installed: skill.installed.map((p) => p.slice(cwd.length + 1)), updated: skill.updated },
    sdk: { package: '@upcontrol/sdk', pinned: SDK_PIN, added: dep.added, inPackageJson: dep.present },
    key: { source: keySource, ...(claimUrl ? { claimUrl } : {}), ...(keyNote ? { note: keyNote } : {}) },
    endpoint,
    hint: 'Run `npx upcontrol skills` for event names and placement rules; finish with `npx upcontrol verify`.',
  };

  if (det.mode !== 'interactive') {
    out(JSON.stringify(result));
    return keyFailed ? 1 : 0;
  }

  out(`upcontrol ${CLI_VERSION}`);
  out('');
  out(`  skill      ${skill.updated ? 'installed' : 'up to date'} (${result.skill.installed.join(', ')})`);
  out(`  sdk        @upcontrol/sdk ${dep.added ? `pinned at ${SDK_PIN} - run your package manager's install` : dep.present ? 'already in package.json' : 'no package.json here - skipped'}`);
  out(`  key        ${describeKey(keySource)}${keyNote ? ` (${keyNote})` : ''}`);
  if (claimUrl) {
    out('');
    out(`  Your project is unclaimed: data flows, but alerts have nowhere to go.`);
    out(`  Claim it (free, keeps the same key): ${claimUrl}`);
  }
  out('');
  out('  Now tell your agent what you want, for example:');
  out('');
  out('    Add observability to this project with upcontrol. My goal:');
  out('    send all my logs to upcontrol and alert me when payments stop.');
  out('');
  out('  The agent reads the installed upcontrol skill, stages a diff for your');
  out('  review, and finishes with `npx upcontrol verify`.');
  if (keyFailed) {
    out('');
    out('  init did NOT provision a key - fix the note on the key line above and rerun.');
    return 1;
  }
  return 0;
}

function describeKey(source: string): string {
  switch (source) {
    case 'env':
      return 'found in UPCONTROL_API_KEY';
    case 'dotenv':
      return 'found in .env';
    case 'flag':
      return 'taken from --key';
    case 'token':
      return 'redeemed from the dashboard token';
    case 'minted':
      return 'anonymous project provisioned';
    default:
      return 'none';
  }
}

function cmdSkills(args: string[]): number {
  const dir = join(bundledSkillDir(), 'references');
  if (args.length === 0) {
    out('upcontrol skill topics (print one with `npx upcontrol skills <topic>`):');
    out('');
    for (const f of readdirSync(dir).sort()) {
      const name = basename(f, '.md');
      const firstLine = readFileSync(join(dir, f), 'utf8').split('\n')[0].replace(/^#\s*/, '');
      out(`  ${name.padEnd(12)} ${firstLine}`);
    }
    out('');
    out('The skill itself (installed into your repo): .claude/skills/upcontrol/SKILL.md');
    return 0;
  }
  const topic = args[0].replace(/[^a-z-]/g, '');
  const p = join(dir, topic + '.md');
  if (!existsSync(p)) {
    out(`unknown topic "${args[0]}" - run \`npx upcontrol skills\` for the list`);
    return 1;
  }
  out(readFileSync(p, 'utf8'));
  return 0;
}

async function cmdStatus(flags: Flags): Promise<number> {
  const cwd = process.cwd();
  const endpoint = endpointFrom(process.env, flags.endpoint);
  const keySource = findKey(cwd);
  const key = process.env.UPCONTROL_API_KEY?.trim() || readDotenvKey(cwd);
  const result: Record<string, unknown> = {
    endpoint,
    keySource,
    skillFresh: skillFresh(cwd),
    sdkPin: SDK_PIN,
    cliVersion: CLI_VERSION,
  };
  if (key) {
    const st = await fetchInstallStatus(endpoint, key);
    result.reachable = st.ok || st.status !== undefined;
    if (st.ok) {
      result.verified = st.verified;
      result.lines = st.lines;
      if (st.web) result.webViews = st.web.views;
    } else if (st.status === 401) {
      result.keyRejected = true;
    }
  } else {
    result.reachable = null;
  }
  out(JSON.stringify(result));
  return 0;
}

async function cmdVerify(det: Detection, flags: Flags): Promise<number> {
  const cwd = process.cwd();
  const endpoint = endpointFrom(process.env, flags.endpoint);
  const key = process.env.UPCONTROL_API_KEY?.trim() || readDotenvKey(cwd);
  const emit = (o: Record<string, unknown>, human: string) => {
    if (flags.json || det.mode !== 'interactive') out(JSON.stringify(o));
    else out(human);
  };
  if (!key) {
    emit(
      { verified: false, error: 'no_key', hint: 'run `npx upcontrol init` first' },
      'verify: no key found (UPCONTROL_API_KEY or .env) - run `npx upcontrol init` first',
    );
    return 2;
  }

  const startedAt = Date.now();
  const deadline = startedAt + flags.timeout * 1000;
  let last: Awaited<ReturnType<typeof fetchInstallStatus>> | null = null;
  let printedWaiting = false;
  for (;;) {
    last = await fetchInstallStatus(endpoint, key);
    // Page views pass only when asked for (--web, a site-only install): any
    // visit to the project's site would otherwise hide an SDK that never connected.
    const webViews = last.ok ? (last.web?.views ?? 0) : 0;
    if (last.ok && (last.verified || (flags.web && webViews > 0))) {
      const names = (last.recent ?? []).map((r) => `${r.name} x${r.count}`).join(', ');
      emit(
        { verified: last.verified === true, verifiedAt: last.verifiedAt, lines: last.lines, web: last.web, recent: last.recent },
        (last.verified
          ? `✓ install_verified - key ok, transport ok, scrubber ok\n`
          : `✓ verified - page views arriving\n`) +
          `  lines in window: ${last.lines ?? 0}\n` +
          (webViews > 0 ? `  page views arriving: ${webViews}\n` : '') +
          (names ? `  arriving now: ${names}\n` : '') +
          `  Nothing else needed.`,
      );
      return 0;
    }
    if (last.status === 401) {
      emit(
        { verified: false, error: 'key_rejected' },
        'verify FAILED: the key was rejected (401). It may have been rotated - get the current one at /app/sources#key.',
      );
      return 4;
    }
    if (!last.ok && last.error === 'unreachable') {
      emit(
        { verified: false, error: 'unreachable', endpoint },
        `verify: cannot reach ${endpoint} - is the endpoint right (UPCONTROL_ENDPOINT)?`,
      );
      return 3;
    }
    if (Date.now() > deadline) break;
    if (!printedWaiting && det.mode === 'interactive' && !flags.json) {
      out('Waiting for the app to report... start it (or send it traffic) with the key in place.');
      printedWaiting = true;
    }
    await new Promise((r) => setTimeout(r, 2000));
  }

  // Timed out: distinguish "nothing at all" from "lines but no marker" (the
  // marker may also have been displaced by the ring on an old install).
  const lines = last?.ok ? (last.lines ?? 0) : 0;
  const views = last?.ok ? (last.web?.views ?? 0) : 0;
  const webHint = views > 0
    ? `page views are arriving (${views}); for a site-only install, with the script tag and no SDK, run \`npx upcontrol verify --web\``
    : '';
  const webFields = webHint ? { web: last?.web, hint: webHint } : {};
  if (lines > 0) {
    const names = (last?.recent ?? []).map((r) => r.name).join(', ');
    emit(
      { verified: false, error: 'no_marker_but_lines', lines, recent: last?.recent, ...webFields },
      `verify: ${lines} lines are arriving but no install_verified marker in the window.\n` +
        (names ? `  arriving: ${names}\n` : '') +
        `  If this is an old install, that is normal (the ring displaced it). For a fresh\n` +
        `  install it means the app is running an SDK that never connected - restart it.` +
        (webHint ? `\n  ${webHint}.` : ''),
    );
    return 4;
  }
  emit(
    { verified: false, error: 'timeout', waitedSec: flags.timeout, ...webFields },
    `verify FAILED: ${webHint ? 'no install_verified marker' : 'nothing arrived'} in ${flags.timeout}s.\n` +
      (webHint ? `  ${webHint}.\n` : '') +
      `  - is the app running with the key in its environment? (dotenv loaded?)\n` +
      `  - did the code path with the SDK import actually execute?\n` +
      `  Run \`npx upcontrol status\` for where the key was found, and\n` +
      `  \`npx upcontrol skills verify\` for the full failure taxonomy.`,
  );
  return 4;
}

// Reads the board input spec: `-` is stdin (fd 0), anything else a file path.
function readBoardInput(spec: string): string | null {
  try {
    return readFileSync(spec === '-' ? 0 : spec, 'utf8');
  } catch {
    return null;
  }
}

// One failure path for every board call: unreachable keeps verify's exit 3,
// a refused write prints the server's own sentence (what is wrong with the
// layout) instead of a bare status.
function boardFail(endpoint: string, r: { error?: string; status?: number; message?: string }): number {
  if (r.error === 'unreachable') {
    err(`board: cannot reach ${endpoint} - is the endpoint right (UPCONTROL_ENDPOINT)?`);
    return 3;
  }
  err(r.message ?? `board: refused (HTTP ${r.status ?? '?'})`);
  return 1;
}

async function cmdBoard(flags: Flags): Promise<number> {
  const endpoint = endpointFrom(process.env, flags.endpoint);
  if (flags.apply !== undefined && flags.add !== undefined) {
    err('board: --apply and --add are mutually exclusive - pick one');
    return 1;
  }
  const key = process.env.UPCONTROL_API_KEY?.trim() || readDotenvKey(process.cwd());
  if (!key) {
    err('board: no key found (UPCONTROL_API_KEY or .env) - run `npx upcontrol init` first');
    return 2;
  }

  if (flags.apply === undefined && flags.add === undefined) {
    const r = await readBoard(endpoint, key);
    if (!r.ok) return boardFail(endpoint, r);
    out((r.text ?? '').trim());
    return 0;
  }

  const spec = (flags.apply ?? flags.add)!;
  const raw = readBoardInput(spec);
  if (raw === null) {
    err(`board: cannot read ${spec}`);
    return 1;
  }
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    err(`board: ${spec} is not JSON`);
    return 1;
  }

  if (flags.apply !== undefined) {
    const r = await applyBoard(endpoint, key, parsed);
    if (!r.ok) return boardFail(endpoint, r);
    const widgets = (parsed as { widgets?: unknown[] }).widgets;
    const n = Array.isArray(widgets) ? widgets.length : 0;
    if (r.status === 202) {
      out('The board was edited in the app, so this layout is waiting there as a proposal.');
      out(`Open ${endpoint}/app/dashboard and press Review to apply it.`);
      return 0;
    }
    out(`stored - ${n} widgets.`);
    return 0;
  }

  // --add takes a bare widget array or a document carrying one (the shape
  // `board` itself prints), so an agent can model its file on the live board.
  const widgets = Array.isArray(parsed) ? parsed : (parsed as { widgets?: unknown[] }).widgets;
  if (!Array.isArray(widgets)) {
    err(`board: ${spec} holds no widgets array`);
    return 1;
  }
  const r = await appendBoard(endpoint, key, widgets);
  if (!r.ok) return boardFail(endpoint, r);
  out(`added ${widgets.length} widgets - the board now has ${r.total}.`);
  return 0;
}

// The web command's one job: turn the site addresses into the tag an agent
// places once. The public key is printed ONLY inside the tag; the secret key
// that minted it never reaches any output, in any mode or error.
async function cmdWeb(det: Detection, flags: Flags, args: string[]): Promise<number> {
  const cwd = process.cwd();
  const endpoint = endpointFrom(process.env, flags.endpoint);
  if (args.length === 0) {
    err('usage: npx upcontrol web <site> [site...]');
    return 2;
  }
  const key = process.env.UPCONTROL_API_KEY?.trim() || readDotenvKey(cwd);
  if (!key) {
    err('no key: run npx upcontrol init first');
    return 2;
  }
  const origins: string[] = [];
  for (const site of args) {
    const fromSite = siteOrigins(site);
    if (fromSite.length === 0) {
      err(`not a site address: ${site}`);
      return 2;
    }
    origins.push(...fromSite);
  }
  const unique = [...new Set(origins)];

  let publicKey = process.env.UPCONTROL_PUBLIC_KEY?.trim() || readDotenvPublicKey(cwd);
  let state: 'minted' | 'reused';
  if (publicKey) {
    state = 'reused';
  } else {
    const mint = await mintPublicKey(endpoint, key, new URL(unique[0]).hostname, unique);
    if (!mint.ok) {
      if (mint.error === 'unreachable') {
        err(`web: cannot reach ${endpoint} - is the endpoint right (UPCONTROL_ENDPOINT)?`);
      } else if (mint.code === 'key_limit') {
        err('the project already has 5 keys: revoke one in the app (Sources, Manage keys)');
      } else if (mint.code === 'bad_origin') {
        err(`not a site address: ${unique.join(', ')}`);
      } else if (mint.code === 'bad_key') {
        err('the project key was refused (revoked, or retiring after a rotation): remove UPCONTROL_API_KEY from .env or the environment, then rerun the install command from the app');
      } else {
        err(`web: refused (HTTP ${mint.status ?? '?'})`);
      }
      return 3;
    }
    publicKey = mint.value!;
    ensureEnvIgnored(cwd);
    writeDotenvPublicKey(cwd, publicKey);
    state = 'minted';
  }

  const tag = `<script defer src="${endpoint}/uc.js" data-key="${publicKey}"></script>`;
  // A reused key covers only the sites it was minted for, whatever this run
  // named, and no endpoint widens it: every mode says so, and only a mint
  // reports origins.
  let note: string | undefined;
  if (state === 'reused') {
    const where = process.env.UPCONTROL_PUBLIC_KEY?.trim() ? 'the environment' : '.env';
    note =
      `the key in ${where} covers only the sites it was minted for; to add one, revoke it in the app ` +
      `(Sources, Manage keys), remove UPCONTROL_PUBLIC_KEY from ${where} and rerun naming every address`;
    err(`key: reused - ${note}`);
  }
  if (flags.json || det.mode !== 'interactive') {
    out(
      JSON.stringify({
        ok: true,
        tag,
        ...(note ? { note } : { origins: unique }),
        key: state,
        next: 'put the tag inside <head> on every page: npx upcontrol skills web',
      }),
    );
    return 0;
  }
  out(tag);
  out('Put it inside <head> on every page. Where that is in your framework: npx upcontrol skills web');
  return 0;
}

async function main(): Promise<number> {
  const { cmd, args, flags } = parseArgs(process.argv.slice(2));
  const det = detect();
  if (flags.version) {
    out(CLI_VERSION);
    return 0;
  }
  if (flags.help || cmd === 'help') {
    out(HELP);
    return 0;
  }
  switch (cmd) {
    case 'init':
      return cmdInit(det, flags);
    case 'skills':
      return cmdSkills(args);
    case 'verify':
      return cmdVerify(det, flags);
    case 'status':
      return cmdStatus(flags);
    case 'board':
      return cmdBoard(flags);
    case 'web':
      return cmdWeb(det, flags, args);
    default:
      out(`unknown command "${cmd}"\n`);
      out(HELP);
      return 1;
  }
}

// process.exitCode, never process.exit(): exit() tears the process down on
// top of live libuv handles and truncates piped stdout; the loop must drain.
main().then(
  (code) => {
    process.exitCode = code;
  },
  (err) => {
    process.stderr.write('upcontrol: unexpected error: ' + (err?.message ?? String(err)) + '\n');
    process.exitCode = 1;
  },
);
