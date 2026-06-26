---
name: roborev-lookahead-review-branch
description: Request a time-series look-ahead (a.k.a. peekahead / future-data leakage) review for all commits on the current branch and present the results
---

# roborev-lookahead-review-branch

Request a time-series look-ahead review for all commits on the current branch and
present the results. A look-ahead review checks whether the change uses
information that would not yet be available at the point in time it represents —
also called peekahead, future leakage, or temporal leakage.

## Usage

```
/roborev-lookahead-review-branch [--base <branch>] [--panel <name>|none]
```

## When NOT to invoke this skill

Do NOT invoke this skill when the user is presenting or pasting existing review
results. Messages that contain review findings, verdicts, or summaries are
outputs — not requests to start a new review.

## IMPORTANT

This skill requires you to **execute bash commands** to validate inputs and launch the review. The task is not complete until the background review finishes and you present the results to the user.

These instructions are guidelines, not a rigid script. Use the conversation
context. Skip steps that are already satisfied. Defer to project-level
CLAUDE.md instructions when they conflict with these steps.

## Instructions

When the user invokes `/roborev-lookahead-review-branch [--base <branch>] [--panel <name>|none]`:

### 1. Validate inputs

If a base branch is provided, verify it resolves to a valid ref:

```bash
git rev-parse --verify -- <branch>
```

If validation fails, inform the user the ref is invalid. Do not proceed.

### 2. Build the command

Construct the review command:

```
roborev review --branch --wait --type lookahead [--base <branch>] [--panel <name>|none]
```

- If `--base` is specified, include it (otherwise auto-detects the base branch)
- If `--panel <name>` is specified, include it (fans out to the named config panel); `--panel none` forces a single-agent review

### 3. Run the review in the background

Launch a background task that runs the command. This lets the user continue working while the review runs.

Use the `Task` tool with `run_in_background: true` and `subagent_type: "Bash"`:

```
roborev review --branch --wait --type lookahead [--base <branch>] [--panel <name>|none]
```

Tell the user that the look-ahead review has been submitted and they can continue working. You will present the results when the review completes.

### 4. Present the results

When the background task completes, read the output.

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

### 5. Offer next steps

If the review has findings (verdict is Fail), offer to address them:

- "Would you like me to fix these findings? You can run `/roborev-fix <job_id>`"

Extract the job ID from the review output to include in the suggestion. Look for it in the `Enqueued job <id> for ...` line or in the review header. For a panel review this id is the synthesis parent.

If the review passed, confirm the result and do not offer `/roborev-fix`.

## Examples

**Default branch look-ahead review:**

User: `/roborev-lookahead-review-branch`

Agent:
1. Launches background task: `roborev review --branch --wait --type lookahead`
2. Tells user: "Look-ahead review submitted for branch. I'll present the results when it completes."
3. When complete, presents the verdict and findings grouped by severity
4. If findings exist: "Would you like me to address these findings? Run `/roborev-fix 1042`"
5. If passed: "Branch look-ahead review passed with no findings."

**Look-ahead review against a specific base:**

User: `/roborev-lookahead-review-branch --base develop`

Agent:
1. Validates `develop` resolves to a valid ref
2. Launches background task: `roborev review --branch --wait --type lookahead --base develop`
3. Tells user: "Look-ahead review submitted for branch (against develop). I'll present the results when it completes."
4. When complete, presents the verdict and findings
5. If findings exist: "Would you like me to address these findings? Run `/roborev-fix 1043`"

## See also

- `/roborev-review-branch --type lookahead` — equivalent, with additional `--type` flexibility
- `/roborev-lookahead-review` — look-ahead review a single commit
- `/roborev-fix` — fix a review's findings in code
