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

Verify it is live:

```bash
roborev status        # daemon + queue
roborev show HEAD     # the latest commit's review
```

## Layer 2 - Agent hook

The agent hook watches your coding-agent session and, once review work piles up,
tells the agent to run the `/roborev-fix` skill before the session ends - closing
the write -> review -> fix loop automatically.

```bash
roborev skills install        # install the /roborev-fix skill
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

## Acting on results

To notify or file issues when reviews complete, add [Review Event
Hooks](../guides/hooks.md).
