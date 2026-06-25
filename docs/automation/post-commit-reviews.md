# Automation: hands-off reviews

roborev is built to run hands-off. There are two automation layers - turn on
both for the full loop.

![How roborev works](/assets/static/how-it-works.svg){ loading=lazy }

## Layer 1 - Post-commit reviews

A git post-commit hook reviews every commit in the background. This works with
any editor or agent.

```bash
roborev init      # installs the hook, starts the daemon, registers the repo
```

Now every commit you make is reviewed automatically. Each review gets a verdict
(pass or fail) and, when it fails, a list of findings with severities and
file locations. Check that it is running:

```bash
roborev status        # daemon + queue
roborev show HEAD     # print the latest commit's review in the terminal
```

Then act on the reviews in whichever way fits how you work:

- **Copy-paste from the TUI.** `roborev tui` shows the review queue; open a
  review to read its findings and copy the full text straight into your coding
  agent (or fix by hand). This works with any agent or editor.
- **Fix failing reviews with the `roborev-fix` skill.** From inside Claude Code
  or Codex, `/roborev-fix` (Codex: `$roborev-fix`) pulls every open failing
  review for your current branch or git worktree, applies the fixes, and closes
  the reviews in one pass.
- **Clean the whole branch before a PR with the refine loop.**
  `/roborev-refine` (Codex: `$roborev-refine`) reviews the branch, fixes
  findings, and re-reviews until every review passes.

The `roborev-fix` and `roborev-refine` skills come from `roborev skills install`
(see [Agent Skills](../guides/agent-skills.md)).

## Layer 2 - Agent hook

The agent hook watches your coding-agent session and, once review work piles up,
tells the agent to run the roborev-fix skill before the session ends - closing
the write -> review -> fix loop automatically. (Claude Code invokes it as
`/roborev-fix`, Codex as `$roborev-fix`.)

```bash
roborev skills install        # install the roborev-fix skill
roborev agent-hook install    # wire the hook into Claude Code / Codex
```

See [Agent Hook](../agent-hook.md) for thresholds and configuration.

### Why CLI, not Desktop?

The agent hook relies on harness hooks (`PreToolUse` / `PostToolUse` / `Stop`)
that the Claude Code CLI and Codex expose. Claude Desktop does not expose these
hooks, so Layer 2 does not run there. Layer 1 (post-commit reviews) works
regardless of which agent or app you use.

## Let an agent finish setup

Point your coding agent at the built-in guide and it will inspect this repo and
help you finish configuration:

```bash
roborev quickstart            # human-readable
roborev quickstart --json     # machine-readable state for agents
```

## React to review events

To notify a channel or file issues automatically when reviews complete, add
[Review Event Hooks](../guides/hooks.md) (desktop notifications, Slack, Kata,
webhooks, or any shell command).
