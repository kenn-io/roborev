---
title: Claude Chic
description: Alternate terminal UI for Claude Code based on Textual and the Claude Agent SDK, with live roborev review integration
---

[Claude Chic](https://github.com/mrocklin/claudechic) is an alternative terminal UI for Claude Code based on the Textual Terminal UI framework using the Claude Agent SDK. Its roborev integration adds a live review sidebar, verdict icons, and one-command fix workflows, so you never have to leave your coding session to check review status.

![Claude Chic review sidebar showing pass/fail verdicts alongside a review detail view](/assets/static/claudechic-review-sidebar.png)

## Installation

```bash
uv tool install claudechic   # recommended
pip install claudechic        # alternative
```

**Prerequisites:** Both `roborev` and `claude` (Claude Code) must be on your PATH. To use the `/roborev-fix` skill, install roborev's agent skills first:

```bash
roborev skills install
```

## Review Sidebar

When a roborev daemon is running, Claude Chic displays a live sidebar panel alongside your coding session:

- Shows open reviews with verdict icons (green P for pass, red F for fail)
- Animated spinners for in-progress reviews
- Auto-polls every 5 seconds for new results
- Click a review to view its detail inline

The sidebar appears automatically when roborev reviews are detected and stays in sync as you work.

Claude Chic supports multiple [git worktrees](/guides/repository-management/#git-worktrees). Switching worktrees or branches filters the sidebar to show only reviews for that worktree or branch.

## Commands

| Command | Description |
|---------|-------------|
| `/reviews` | List all recent reviews with verdicts and closed/open status |
| `/reviews <job_id>` | Show full detail for a specific review |
| `/reviewer [context]` | Start a new roborev review with optional context |

These commands are available in the Claude Chic command palette alongside standard Claude Code commands.

## Workflow Example

A typical review-fix cycle inside Claude Chic:

1. Make changes to your code as usual in the Claude Chic session
2. roborev reviews your commits in the background
3. The sidebar updates: a red F appears for job `#1019`
4. Run `/reviews 1019` to read the full review inline
5. Run `/roborev-fix` to batch-fix all open findings
6. The sidebar auto-updates: the verdict flips to a green P

The entire loop happens without switching windows or terminals.

## See Also

- [Agent Skills](/guides/agent-skills/): Skills available to coding agents including roborev skills
- [Responding to Reviews](/guides/responding-to-reviews/): How to address and comment on review findings
- [Auto-Fix with Refine](/guides/auto-fixing/): Automated fix loops using `roborev fix`
