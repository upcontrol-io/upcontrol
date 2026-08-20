# upcontrol

Monitoring that arrives with the "why", not just "down" - wired into your app
by the coding agent you already use.

```
npx upcontrol
```

One command, for every agent (Claude Code, Cursor, Codex, Gemini CLI, Copilot,
Windsurf). It does five deterministic things and runs no AI of its own:

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
4. **Sends a five-field project spec** - `name`, `description`, `framework`,
   `runtime`, `language`, read from `package.json` and the presence of
   `tsconfig.json`. Never dependency lists, versions, file paths, git remotes,
   env values or code. It prints the exact spec before sending it, so what
   leaves is on your screen; `--no-meta` skips the step entirely. It exists so
   that when you ask upcontrol to explain a log line, the answer knows what
   your stack is.
5. **Verifies.** `npx upcontrol verify` waits until data provably arrives and
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
- `npx upcontrol init --no-meta` - install without sending the project spec
- Your code never leaves your machine: the CLI talks only to the upcontrol
  API, the intelligence is your own agent, and the SDK sends only what the
  reviewed log points emit - scrubbed client-side first. What does leave is
  the five-field spec above (printed before it is sent, skippable) and the log
  lines your own code emits. Those lines reach a language model only when you
  select some in the dashboard and press Explain - never on ingest, and never
  the whole window.

https://upcontrol.io
