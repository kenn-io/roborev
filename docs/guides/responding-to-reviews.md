---
title: Responding to Reviews
description: Use reviews in your agent sessions and record responses and comments
---


The review queue is a ledger: every review stays open until you explicitly close it. This is intentional. Open reviews represent findings that have not been addressed, giving you a clear picture of what still needs attention across all your branches.

Reviews are designed to be fed back into your AI coding agent sessions to address findings. The quickest way is `roborev fix`, which runs an agent to apply changes automatically. You can also copy reviews into an agent session manually, or add comments for context. Once a finding is resolved, close the review in the TUI (`a`) or via `roborev close` to remove it from your backlog.

## Auto-Fix with `roborev fix`

Let an agent address findings automatically. `roborev fix` runs **synchronously in the foreground**: it blocks until the agent finishes and applies changes directly to your working tree.

```bash
roborev fix                        # Fix all open reviews on this branch
roborev fix 123                    # Fix a specific review by job ID
roborev fix --batch                # Batch all open into one agent prompt
```

The agent reads the review, applies changes, commits, and closes the review. This is a one-shot fix. For an iterative loop with re-review, see [Auto-Fix with Refine](/guides/auto-fixing/).

## Copying Reviews

Press `y` in the TUI to copy the full review content to your clipboard, including any comments attached to the review. Paste it into an AI coding agent session to address the findings. The comments give the agent additional context about known issues, false positives, and prior discussion. You can also share reviews with teammates, include them in GitHub issues, or reference them in commit messages.

`roborev show` also displays comments after the review output.

<figure class="screenshot" data-lightbox>
  <img src="/assets/generated/tui-copy.svg" alt="roborev TUI copy review to clipboard" loading="lazy">
</figure>

## Adding a Comment

### From the TUI

Press `c` from either the queue view or review detail view to open the comment modal:

<figure class="screenshot" data-lightbox>
  <img src="/assets/generated/tui-respond.svg" alt="roborev TUI comment modal" loading="lazy">
</figure>

1. Navigate to the review you want to comment on
2. Press `c` to open the comment modal
3. Type your comment (multi-line supported)
4. Press `Enter` to submit, or `Esc` to cancel

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

When the AI reviews the same commit again (or related commits), your comments are included in the prompt:

- **Context about known issues**: "This null check is intentional, handled in caller"
- **Clarifications**: "This is test code, different standards apply"
- **Acknowledgments**: "Good catch, will fix in next commit"

!!! tip
    Comments help train the AI's understanding of your codebase conventions and reduce false positives in future reviews.

## Best Practices

1. **Be specific**: Reference line numbers and specific findings
2. **Explain the "why"**: Help the AI understand your reasoning
3. **Acknowledge valid findings**: Confirm when the AI catches real issues
4. **Note false positives**: Mark findings that don't apply to your context

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
