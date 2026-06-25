## How roborev works

roborev gives your coding agent a second set of eyes. It reviews your code
automatically, in the background, on every commit, and feeds findings back so
the agent (or you) can fix them. There are two automation layers:

**Layer 1 - Post-commit reviews (works everywhere).**
A git post-commit hook enqueues a background review of every commit. This works
with any editor or agent. Findings land in `roborev tui`, `roborev show HEAD`,
and the daemon API.

**Layer 2 - Agent hook (CLI harnesses).**
The agent hook watches your coding-agent session (turns, commits, failed
reviews). When review work piles up, it returns one instruction telling the
agent to run the roborev-fix skill before the session goes cold - closing the
write -> review -> fix loop without you asking. (Claude Code invokes it as
`/roborev-fix`; Codex as `$roborev-fix`.) This requires a CLI harness
(Claude Code CLI or Codex) that exposes PreToolUse / PostToolUse / Stop hooks.
Claude Desktop does not expose these, so only Layer 1 runs there.

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

For per-workflow routing and reasoning levels (fast / standard / thorough), see
https://roborev.io/configuration/.
