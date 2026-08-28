# upcontrol

Monitoring that arrives with the "why", not just "down" - wired into your app
by the coding agent you already use.

```
npx upcontrol
```

[![Works with 10 coding agents](https://img.shields.io/badge/works%20with-10%20coding%20agents-blue)](https://github.com/upcontrol-io/upcontrol/blob/master/cli/installer/src/detect.ts)

One command, for every agent (Claude Code, Cursor, Codex, Gemini CLI, Copilot,
Windsurf, Amp, Aider, Cline, opencode). It does four deterministic things and
runs no AI of its own:

1. **Installs the upcontrol skill** into `.claude/skills/` and
   `.agents/skills/` (add `--copilot` for `.github/skills/`) - the canonical
   event dictionary, placement rules and stack recipes your agent follows.
2. **Pins `@upcontrol/sdk`** in package.json at an exact version - the library
   whose `track()` never throws and never blocks.
3. **Provisions a key.** No account needed: an anonymous project is minted and
   the key is written into `.env` - only after `.gitignore` is confirmed to
   cover it, and without ever printing it. Claim the project later (free, same
   key) via the printed link. Already have a key? `--key uc_live_...` or
   `UPCONTROL_API_KEY`.
4. **Verifies.** `npx upcontrol verify` waits until data provably arrives and
   names the failure precisely when it does not.

Then talk to your agent in plain language:

> send all my logs to upcontrol
>
> track user behavior and tell me where customers churn
>
> alert me when my nightly cron dies silently

The agent reads the skill, stages a diff for **your review** (it never
commits), and finishes only when `verify` reports data flowing.

Run inside an agent, the command detects it and answers in JSON - so "set up
upcontrol for me" works as a single prompt too.

- `npx upcontrol skills` - the reference topics the agent reads
- `npx upcontrol status` - endpoint, key source, skill freshness, one JSON line
- Your code never leaves your machine: the CLI talks only to the upcontrol
  API, the intelligence is your own agent, and the SDK sends only what the
  reviewed log points emit - scrubbed client-side first. What leaves is the
  log lines your own code emits, and nothing on our side ever feeds them to a
  language model.

https://upcontrol.io
