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

The daemon coalesces duplicate hook requests for the same repository, resolved
Git reference, and review target. Concurrent editor, worktree, or agent hooks
therefore launch one automatic review. Explicit `roborev review` commands still
start a fresh review when you deliberately request one.

Now every commit you make is reviewed automatically. Each review gets a verdict
(pass or fail) and, when it fails, a list of findings with severities and file
locations. Check that it is running:

```bash
roborev status        # daemon + queue
roborev show HEAD     # print the latest commit's review in the terminal
```

### Batch small commits

If reviewing every commit creates too much noise, set a repository-local batch
size in `.roborev.toml`:

```toml
post_commit_batch_size = 5
```

The post-commit hook then waits until five commits have accumulated and queues
one review for the full range. Values below `2`, including the default of `1`,
keep the usual one-review-per-commit behavior.

Pending commits are tracked separately for each branch and shared across linked
worktrees. The pre-push hook attempts to queue a partial batch before those
commits are pushed. It does not block the push if queueing fails, and the batch
remains pending for a later hook. After a rebase or amend, the next review
covers everything since the last commit the rewritten branch still shares with
its old history. Rewritten commits may be reviewed again. Run `roborev init`
after upgrading roborev to install or update the pre-push hook.

Batching changes review frequency, not review scope. With
`post_commit_review = "commit"`, roborev reviews the accumulated commit range.
With `post_commit_review = "branch"`, roborev still reviews the full branch, but
only when the batch threshold is reached or pending work is flushed.

Then act on the reviews in whichever way fits how you work:

- **Copy-paste from the TUI.** `roborev tui` shows the review queue; open a
    review to read its findings and copy the full text straight into your coding
    agent (or fix by hand). This works with any agent or editor.
- **Fix failing reviews with the `roborev-fix` skill.** From inside Claude Code
    or Codex, `/roborev-fix` (Codex: `$roborev-fix`) pulls every open failing
    review for your current branch or git worktree, applies the fixes, and
    closes the reviews in one pass.
- **Clean the whole branch before a PR with the refine loop.** `/roborev-refine`
    (Codex: `$roborev-refine`) reviews the branch, fixes findings, and
    re-reviews until every review passes.

The `roborev-fix` and `roborev-refine` skills come from `roborev skills install`
(see [Agent Skills](../guides/agent-skills.md)). Agent Hook installation updates
the bundled skills automatically for supported profiles.

## Layer 2 - Agent hook

The agent hook watches supported coding-agent sessions and, once review work
piles up, supplies exact review IDs to the `roborev-fix` skill before the
session ends. It never runs the separate `roborev fix --open` agent workflow.

```bash
roborev agent-hook install    # wire harnesses and update supported bundled skills
```

See [Agent Hook](../agent-hook.md) for thresholds and configuration.

### Why CLI, not Desktop?

The agent hook relies on harness lifecycle hooks supplied by Claude Code, Codex,
Copilot CLI, Cursor, Factory Droid, Gemini CLI, Hermes, and Qwen. Claude Desktop
does not expose these hooks, so Layer 2 does not run there. Layer 1 (post-commit
reviews) works regardless of which agent or app you use.

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
