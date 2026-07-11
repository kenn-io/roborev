---
name: roborev-design-review-branch
description: Use only when the user explicitly invokes /roborev-design-review-branch
---

# roborev-design-review-branch

Request a design review for all commits on the current branch and present the results.

## Usage

```
/roborev-design-review-branch [--base <branch>] [--panel <name>|none]
```

## Explicit invocation only

Invocation must be explicit: literal personal `/roborev-design-review-branch`, or structured
Factory skill selection.
Requests such as “review this branch's design” without one of these
explicit mechanisms must use native behavior and must not run roborev.

## When NOT to invoke this skill

Do NOT invoke this skill when the user is presenting or pasting existing review
results. Messages that contain review findings, verdicts, or summaries are
outputs — not requests to start a new review.

## IMPORTANT

This skill requires you to **execute bash commands** to validate inputs and run the review. The task is not complete until the review finishes and you present the results to the user.

These instructions are guidelines, not a rigid script. Use the conversation
context. Skip steps that are already satisfied. Defer to project-level
AGENTS.md instructions when they conflict with these steps.

## Instructions

When the user invokes `/roborev-design-review-branch [--base <branch>] [--panel <name>|none]`:

### 1. Validate inputs

If a base branch is provided, use the base-branch command snippet below; it stores and validates the ref before invoking `roborev review`.

If validation fails, inform the user the ref is invalid. Do not proceed.

### 2. Build and run the command

Construct and execute the review command:

If no base branch is specified, run:

```bash
roborev review --branch --wait --type design [--panel <name>|none]
```

If a base branch is specified, run:

```bash
read -r branch <<'ROBOREV_REF'
<branch>
ROBOREV_REF
git rev-parse --verify -- "$branch" || exit 1
roborev review --branch --wait --type design --base "$branch" [--panel <name>|none]
```

- If `--base` is specified, include it (otherwise auto-detects the base branch)
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

- "Would you like me to fix these findings? You can run `/roborev-fix <job_id>`"

Extract the job ID from the review output to include in the suggestion. Look for it in the `Enqueued job <id> for ...` line or in the review header. For a panel review this id is the synthesis parent.

If the review passed, confirm the result and do not offer `/roborev-fix`.

## Examples

**Default branch design review:**

User: `/roborev-design-review-branch`

Agent:
1. Executes `roborev review --branch --wait --type design`
2. Presents the verdict and findings grouped by severity
3. If findings exist: "Would you like me to address these findings? Run `/roborev-fix 1042`"
4. If passed: "Branch design review passed with no findings."

**Design review against a specific base:**

User: `/roborev-design-review-branch --base develop`

Agent:
1. Validates: `git rev-parse --verify -- "develop"`
2. Executes `roborev review --branch --wait --type design --base develop`
3. Presents the verdict and findings
4. If findings exist: "Would you like me to address these findings? Run `/roborev-fix 1043`"

## See also

- `/roborev-review-branch --type design` — equivalent, with additional `--type` flexibility
- `/roborev-design-review` — design review a single commit
- `/roborev-fix` — fix a review's findings in code
