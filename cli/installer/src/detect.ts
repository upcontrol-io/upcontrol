// Which coding agent (if any) is driving this process, from environment
// markers — the same technique checkly@8.x proved at scale. An explicit
// AI_AGENT/AGENT env always wins; CI is its own mode; a non-TTY stdout with no
// known agent still gets machine-readable output, because a pipe cannot answer
// prompts either.

export type CliMode = 'agent' | 'ci' | 'interactive';

export interface Detection {
  mode: CliMode;
  agent: string | null;
}

const ALIASES: Record<string, string> = {
  claude: 'claude-code',
  'claude-code': 'claude-code',
  cursor: 'cursor',
  codex: 'codex-cli',
  'codex-cli': 'codex-cli',
  gemini: 'gemini-cli',
  'gemini-cli': 'gemini-cli',
  copilot: 'github-copilot',
  'github-copilot': 'github-copilot',
  windsurf: 'windsurf',
};

// The number of distinct agents here is a published claim: both READMEs carry
// a "works with N coding agents" badge that links back to this list. Adding an
// agent means updating them, or the badge is asserting rather than measuring.
const MARKERS: Array<[string, string]> = [
  ['CLAUDECODE', 'claude-code'],
  ['CLAUDE_CODE', 'claude-code'],
  ['CURSOR_TRACE_ID', 'cursor'],
  ['CURSOR_AGENT', 'cursor'],
  ['CODEX_SANDBOX', 'codex-cli'],
  ['CODEX_THREAD_ID', 'codex-cli'],
  ['GEMINI_CLI', 'gemini-cli'],
  ['COPILOT_AGENT', 'github-copilot'],
  ['COPILOT_CLI', 'github-copilot'],
  ['GITHUB_COPILOT', 'github-copilot'],
  ['WINDSURF', 'windsurf'],
  ['WINDSURF_AGENT', 'windsurf'],
  ['CODEIUM_ENV', 'windsurf'],
  ['AMP_CURRENT_THREAD_ID', 'amp'],
  ['AIDER_MODEL', 'aider'],
  ['CLINE_ACTIVE', 'cline'],
  ['OPENCODE', 'opencode'],
];

export function detect(env: NodeJS.ProcessEnv = process.env, ttyOut?: boolean): Detection {
  const explicit = (env.AI_AGENT || env.AGENT || '').trim().toLowerCase();
  if (explicit) {
    return { mode: 'agent', agent: ALIASES[explicit] ?? explicit };
  }
  for (const [marker, agent] of MARKERS) {
    if (env[marker]) return { mode: 'agent', agent };
  }
  if (env.CI || env.GITHUB_ACTIONS || env.GITLAB_CI || env.BUILDKITE || env.CIRCLECI) {
    return { mode: 'ci', agent: null };
  }
  const tty = ttyOut ?? process.stdout.isTTY === true;
  if (!tty) return { mode: 'agent', agent: null };
  return { mode: 'interactive', agent: null };
}
