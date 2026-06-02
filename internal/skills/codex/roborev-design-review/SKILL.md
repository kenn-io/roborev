---
name: roborev-design-review
description: Request a design review for a commit and present the results
---

# roborev-design-review

Request a design review for a commit and present the results.

## Usage

```
$roborev-design-review [commit] [--panel <name>|none]
```

## When NOT to invoke this skill

Do NOT invoke this skill when the user is presenting or pasting existing review
results. Messages that contain review findings, verdicts, or summaries are
outputs — not requests to start a new review.

## IMPORTANT

This skill requires you to **execute bash commands** to validate the commit and run the review. The task is not complete until the review finishes and you present the results to the user.

These instructions are guidelines, not a rigid script. Use the conversation
context. Skip steps that are already satisfied. Defer to project-level
CLAUDE.md instructions when they conflict with these steps.

## Instructions

When the user invokes `$roborev-design-review [commit] [--panel <name>|none]`:

### 1. Validate inputs

If a commit ref is provided, verify it resolves to a valid commit:

```bash
git rev-parse --verify -- <commit>^{commit}
```

If validation fails, inform the user the ref is invalid. Do not proceed.

### 2. Build and run the command

Construct and execute the review command:

```bash
roborev review [commit] --wait --type design [--panel <name>|none]
```

- If no commit is specified, omit it (defaults to HEAD)
- If `--panel <name>` is specified, include it (fans out to the named config panel); `--panel none` forces a single-agent review

The `--wait` flag blocks until the review completes.

### 3. Present the results

If the command output contains an error (e.g., daemon not running, repo not initialized, review errored), report it to the user. Suggest `roborev status` to check the daemon, `roborev init` if the repo is not initialized, or re-running the review.

Otherwise, present the review to the user:
- Show the verdict prominently (Pass or Fail)
- If there are findings, list them grouped by severity with file paths and line numbers so the user can navigate directly
- If the review passed, a brief confirmation is sufficient

#### Panels (multi-reviewer reviews)

If you pass `--panel <name>`, or a `default_panel` is configured for explicit
reviews, the review fans out to a panel of reviewers. In that case the
`Enqueued job <id>` is the **synthesis (parent)** job that aggregates them, and
its verdict and findings are the synthesized result across the whole panel.
Present that synthesized verdict/findings, and offer fix on that parent id —
never an individual reviewer. `roborev show` prints a one-line reviewers summary
(e.g. `3 reviewers: bug P, security F`) for a synthesis job. `--panel none`
forces a single-agent review, and automatic post-commit hook reviews stay
single-agent regardless of `default_panel`.

### 4. Offer next steps

If the review has findings (verdict is Fail), offer to address them:

- "Would you like me to fix these findings? You can run `$roborev-fix <job_id>`"

Extract the job ID from the review output to include in the suggestion. Look for it in the `Enqueued job <id> for ...` line or in the review header. For a panel review this id is the synthesis parent.

If the review passed, confirm the result and do not offer `$roborev-fix`.

## Examples

**Default design review of HEAD:**

User: `$roborev-design-review`

Agent:
1. Executes `roborev review --wait --type design`
2. Presents the verdict and findings grouped by severity
3. If findings exist: "Would you like me to address these findings? Run `$roborev-fix 1042`"
4. If passed: "Design review passed with no findings."

**Design review of a specific commit:**

User: `$roborev-design-review abc123`

Agent:
1. Validates: `git rev-parse --verify -- abc123^{commit}`
2. Executes `roborev review abc123 --wait --type design`
3. Presents the verdict and findings
4. If findings exist: "Would you like me to address these findings? Run `$roborev-fix 1043`"

## See also

- `$roborev-review --type design` — equivalent, with additional `--type` flexibility
- `$roborev-design-review-branch` — design review all commits on the current branch
- `$roborev-fix` — fix a review's findings in code
