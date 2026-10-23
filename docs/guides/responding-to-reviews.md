---
title: Responding to Reviews
description: Use reviews in your agent sessions and record responses and comments
---

The review queue is a ledger: every review stays open until you explicitly close
it. This is intentional. Open reviews represent findings that have not been
addressed, giving you a clear picture of what still needs attention across all
your branches.

Reviews are designed to be fed back into your AI coding agent sessions to
address findings. The quickest way is `roborev fix`, which runs an agent to
apply changes automatically. You can also copy reviews into an agent session
manually, or add comments for context. Once a finding is resolved, close the
review in the TUI (`a`) or via `roborev close` to remove it from your backlog.

## Auto-Fix with `roborev fix`

Let an agent address findings automatically. `roborev fix` runs **synchronously
in the foreground**: it blocks until the agent finishes and applies changes
directly to your working tree.

```bash
roborev fix                        # Fix all open reviews on this branch
roborev fix 123                    # Fix a specific review by job ID
roborev fix --batch                # Batch all open into one agent prompt
```

The agent reads the review, applies changes, commits, and closes the review.
This is a one-shot fix. For an iterative loop with re-review, see
[Auto-Fix with Refine](/docs/guides/auto-fixing/).

When a finding asks to restore prior behavior, the fix prompt directs the agent
to inspect relevant repository history before editing. The reviewed git ref is
included when available so established identifiers, schema object names, public
APIs, paths, and migration semantics can be preserved when still compatible.

## Copying Reviews

Press `y` in the TUI to copy the full review content to your clipboard,
including any comments attached to the review. Paste it into an AI coding agent
session to address the findings. The comments give the agent additional context
about known issues, false positives, and prior discussion. You can also share
reviews with teammates, include them in GitHub issues, or reference them in
commit messages.

`roborev show` also displays comments after the review output.

<figure class="screenshot" data-lightbox>
  <img src="/docs/assets/generated/tui-copy.svg" alt="roborev TUI copy review to clipboard" loading="lazy">
</figure>

## Adding a Comment

### From the TUI

Press `c` from either the queue view or review detail view to open the comment
modal:

<figure class="screenshot" data-lightbox>
  <img src="/docs/assets/generated/tui-respond.svg" alt="roborev TUI comment modal" loading="lazy">
</figure>

1. Navigate to the review you want to comment on
1. Press `c` to open the comment modal
1. Type your comment (multi-line supported)
1. Press `Enter` to submit, or `Esc` to cancel

### From the CLI

```bash
roborev comment <review-id> "Your comment text"
```

Or interactively:

```bash
roborev comment <review-id>
# Opens your $EDITOR for multi-line input
```

## How Comments Are Used

When the AI reviews the same commit again (or related commits), your comments
are included in the prompt:

- **Context about known issues**: "This null check is intentional, handled in
    caller"
- **Clarifications**: "This is test code, different standards apply"
- **Acknowledgments**: "Good catch, will fix in next commit"

!!! tip

    Comments help train the AI's understanding of your codebase conventions and
    reduce false positives in future reviews.

## Best Practices

1. **Be specific**: Reference line numbers and specific findings
1. **Explain the "why"**: Help the AI understand your reasoning
1. **Acknowledge valid findings**: Confirm when the AI catches real issues
1. **Note false positives**: Mark findings that don't apply to your context

## Example Comments

```
The singleton pattern here is intentional for the connection pool.
Thread safety is handled by the underlying driver.
```

```
This is generated code from protobuf - we don't modify it directly.
Please ignore style issues in *.pb.go files.
```

```
Good catch on the race condition! I'll add a mutex in the next commit.
```

## Re-evaluating a Finding

Send an explanation back to the agent that wrote a finding:

```bash
roborev comment 42 --rebuttal --finding "Missing null check in handler." \
  -m "The caller rejects null input before invoking the handler."
```

`--finding` must be an excerpt of the original review. To name the finding
instead, pass the selector that `roborev show 42` lists for it:

```bash
roborev comment 42 --rebuttal \
  --finding-id f:boprylsnljhg7ct3dqwt4t22nmbpi3sz2rej44fz \
  -m "The caller rejects null input before invoking the handler."
```

Copy the whole selector from `roborev show`. Part of one is rejected, even if it
would match only one finding. The selector must come from the review as it
stands now: after you rerun the job, a selector from the earlier review is
rejected, because the rebuttal would be recorded on a different review. Use
`--finding` or `--finding-id`, not both.

Roborev queues a focused assessment and waits for the original agent session,
using the review's model, provider and normal review permissions. It does not
request a full review or apply fixes. `roborev respond` accepts the same
options.

The assessment confirms, refutes or supersedes the selected finding and explains
its evidence, including any severity change. Both the explanation and assessment
are saved as comments; the original review and verdict remain available.

Add `--close` to close the **whole review** after the assessment is recorded.
This is your explicit close instruction, even when the agent confirms the
finding. Omit it when other findings still need attention.

Rebuttals require an open, completed local code review, a stored session ID and
the original checkout. Only these agents can run a rebuttal: Codex, Claude Code,
Pi and Grok. Other agents, CI reviews and isolated review sessions are
unsupported.

Assessments enforce read-only permissions even when `allow_unsafe_agents` is
enabled. An adapter that cannot enforce this policy is rejected before launch.

Missing sessions, stale worktrees, agent errors and malformed assessments leave
the review open. Rebuttals wait while another job holds their session, including
canceled jobs whose workers have not finished. Failed rebuttals are not
automatically retried or rerun with a fresh session; submit a new rebuttal on
the original review to try again.

The command prints its assessment job ID before waiting. If interrupted, the job
remains in the queue: inspect it with `roborev show <assessment-job-id>`. You
can record its completed assessment manually with `roborev comment` and close
the original review once satisfied. A daemon restart can invalidate an
interrupted session; submit a new rebuttal in that case.
