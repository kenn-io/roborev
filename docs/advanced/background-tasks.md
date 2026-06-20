---
title: Background Tasks
description: Run fix jobs in isolated worktrees and manage patches from the TUI
---

!!! warning "Advanced opt-in feature"
    Background tasks are disabled by default. Enable them from the command
    line:

    ```bash
    roborev config set advanced.tasks_enabled true --global
    ```

    This feature provides an advanced workflow for running fix jobs and
    applying patches directly from the TUI. Most users should address
    review findings by copying the review (`y`) and pasting it into their
    coding agent session, or by using
    [`/roborev-fix`](/guides/agent-skills/) or
    [`roborev fix`](/commands/#fixing-reviews).

## Fix Jobs

Press `F` on a completed review to open the inline fix panel at the
bottom of the review view. You can optionally type custom instructions
for the fix agent, or leave it blank for defaults. Press `Enter` to
submit, `Esc` to cancel.

While the fix panel is open, `Tab` toggles focus between the review
content (for scrolling) and the fix input. The panel border is magenta
when focused and gray when inactive.

From the queue view, pressing `F` fetches the review and opens the fix
panel in one step.

The fix runs in an isolated git worktree checked out at the reviewed
commit (not HEAD), so the patch is always generated against the correct
revision. This prevents conflicts when the working tree has moved on
since the review.

Results appear in the Tasks view (`T`). While the fix runs, you can
continue browsing reviews normally.

## Tasks View

Press `T` from the queue view to switch to the Tasks view, which shows
all fix jobs and their status.

| Key | Action |
|-----|--------|
| `Up`/`k`, `Down`/`j` | Navigate tasks |
| `Enter` / `l` | View output (live for running, result for done, error for failed) |
| `p` | View patch diff |
| `A` | Apply patch to working tree |
| `R` | Re-run fix against current HEAD (rebase) |
| `x` | Cancel queued/running job |
| `s` | Save patch to disk |
| `r` | Refresh task list |
| `T` / `Esc` | Return to queue |
| `?` | Toggle help |

The Tasks view also supports mouse interactions: click a task to select
it, or double-click to view its output.

## Applying and Rebasing Patches

Press `A` on a completed fix job to apply the generated patch and
commit it to your working tree. The parent review is automatically
closed when the patch is applied.

If the patch conflicts with the current state of your working tree
(e.g. because new commits landed after the review), roborev offers to
re-run the fix against current HEAD. Press `R` to trigger a rebase:
the agent receives the original patch as context and produces a new
patch targeting the current code.

## See Also

- [Agent Skills](/guides/agent-skills/): Fix findings from your agent session
- [Auto-Fix with Refine](/guides/auto-fixing/): Automated fix loop
- [Terminal UI](/integrations/tui/): TUI overview and keyboard shortcuts
