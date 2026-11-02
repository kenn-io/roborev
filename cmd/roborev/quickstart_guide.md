## Direct CLI review workflow

You can work from the CLI help without installing a skill. Run these in the git
checkout, replacing job `42` with the ID returned by `review` or `job list`:

```bash
roborev doctor
roborev init
roborev review HEAD
roborev job wait --job 42
roborev job show --job 42
roborev fix 42
roborev job comment --job 42 "Fixed and verified the findings"
roborev job close 42
```

`doctor` reads setup state without starting the daemon. `init` configures the
repository and git hooks. Reviews run in the background; `wait` follows an
existing job. `fix` runs an agent in the foreground, edits and commits code,
and closes completed jobs. Re-review the resulting commit for fresh verification.
Use `fix --list` to preview work, or `refine` for repeated review and fix cycles.

Job IDs are positive integers scoped to the selected daemon. `show`, `wait`,
and `comment` try local git refs first; `--job` forces a numeric job ID.
`show` and `wait` default to `HEAD`; `review` always takes git refs.
Commands normally discover and auto-start a local daemon under
`ROBOREV_DATA_DIR` (default `~/.roborev`). `--server` targets an existing
loopback address, Unix socket, or remote HTTPS URL. Remote access supports only
`list`, `show --job`, `wait --job`, and `stream`, with signing credentials and
repository grants.

Output flags are command-local. `list --json` includes job status and verdict;
`show --json` returns the review record and stored document. Only `search`
uses `--agent` for concise output; other commands use it to select a provider
or integration. Errors go to stderr with a recovery hint. Exit codes are
0 success, 1 runtime failure or a waiting command's FAIL verdict, 2 usage,
3 export cursor database reset, and 4 invalid values. Displaying a FAIL with
`show` succeeds. Use `roborev --help` for environment variables and command groups,
and each command's `--help` for its flags and examples.

## How roborev works

roborev gives your coding agent a second set of eyes. It reviews your code
automatically, in the background, on every commit, and feeds findings back so
the agent (or you) can fix them. There are two automation layers:

**Layer 1 - Post-commit reviews (works everywhere).**
A git post-commit hook enqueues a background review of every commit. This works
with any editor or agent. Findings land in `roborev tui`, `roborev job show HEAD`,
and the daemon API.

**Layer 2 - Agent hook (CLI harnesses, optional).**
The agent hook watches your coding-agent session (turns, commits, failed
reviews). When review work piles up, it returns a self-contained fix instruction
before the session goes cold. The write -> review -> fix loop closes without you
asking. `roborev agent hook install` auto-detects Claude Code, Codex,
Copilot CLI, Cursor, Factory Droid, Gemini CLI, Hermes, Qwen, and Grok Build.
Hook installation updates bundled skills for every supported profile.
The hook never fires in a repo without its own review guidance
(`review_guidelines` in `.roborev.toml`, or `REVIEW.md`). Without guidance,
reviews flag generic concerns and an agent that fixes all of them tends to
overengineer. Set and tune guidelines before relying on the hook.
Claude Desktop does not expose harness hooks, so only Layer 1 runs there.

Quickstart health checks cover Claude Code, Codex, and Grok Build. Use
`roborev agent hook install` and agent-native config inspection to manage the
other supported profiles.

## Skills: drive roborev from your coding agent

Skills are the main way you and your agent use roborev from inside a session,
with or without the agent hook. `roborev agent skills install` adds them to Claude
Code and Codex as commands (Claude Code invokes them with `/`, Codex with `$`):

- `/roborev-review` - review the current commit; `/roborev-review-branch` reviews every commit on the branch
- `/roborev-refine` - review the branch, fix findings, and re-review until it passes
- `/roborev-fix` - fix all open failing reviews in one pass
- `/roborev-respond` - comment on a review and close it
- `/roborev-design-review` - run a design-focused review (`/roborev-design-review-branch` for the branch)

Invoke these directly whenever you want a review or a fix. They do not require
the agent hook. Layer 2 invokes `roborev-fix` with the exact review job IDs. It
does not substitute a CLI workflow when the skill is unavailable.

### Finalize a branch with the refine loop

Before opening a PR, get the whole branch review-clean with the refine loop:
review every commit on the branch, fix the findings, and re-review - repeating
until every review passes.

```
/roborev-refine
```

This is the recommended day-to-day workflow; the skill drives the
review -> fix -> re-review loop for you (up to `--max-iterations`, default 10).
A non-interactive `roborev refine` CLI command also exists for CI and scripting,
and can gate on a severity floor (`--min-severity medium` to ignore low
findings), but that is advanced - for interactive work the skill is the better
fit.

## Configuration playbook

You are helping a human configure roborev for their repo. Use the "Current
state" section above to see what is already set up, then apply only what is
missing. Confirm changes with the user before editing their files.

### Make reviews flag what this team cares about

Add standing instructions to every review with `review_guidelines` in the repo's
`.roborev.toml`. They are injected into each review prompt.

```toml
# .roborev.toml
review_guidelines = """
Every change to UI components must include or update a Playwright e2e test.
Flag any PR that changes UI without a corresponding e2e test.
"""
```

To act on review outcomes (notify, file issues), add hooks. Because empty hooks
are omitted from the generated config, you can add a `[[hooks]]` block directly:

```toml
[[hooks]]
event = "review.*"
type = "kata"
project = "myproj"
```

### Tune the agent's own CLAUDE.md / AGENTS.md

roborev reviews each commit, so frequent, small commits produce tighter, more
useful feedback. If the user's agent batches large changes, suggest adding this
to their CLAUDE.md or AGENTS.md:

```markdown
## Committing
Commit early and often. After each self-contained change that builds and passes
tests, make a commit rather than batching many changes together. Small commits
get faster, more focused automated review.
```

### Choose the review agent and model

Set the review agent and model per repo in `.roborev.toml`:

```toml
agent = "codex"          # codex, claude-code, gemini, copilot, ...
model = "gpt-5-codex"    # optional, agent-specific
```

Or set the defaults globally in `~/.roborev/config.toml`. Note the keys differ
(a top-level `agent` there collides with the global `[agent]` table):

```toml
default_agent = "codex"
default_model = "gpt-5-codex"
```

For per-workflow routing and legacy or exact reasoning levels, see
https://roborev.io/docs/configuration/.
