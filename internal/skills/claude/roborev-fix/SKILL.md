---
name: roborev-fix
description: Use when the user asks to fix open failing reviews, invokes /roborev-fix, or provides job IDs; do not use when the user only pastes review findings with no request to discover or close reviews
---

# roborev-fix

Fix all open failing review findings in one pass.

## Usage

```
/roborev-fix [job_id...]
```

## When NOT to invoke this skill

Do NOT invoke this skill just because the user pasted existing review
findings or review text into the conversation.

If the prompt already contains the findings to fix, treat that as direct fix
input and work on the code normally. The presence of verdicts, severities,
file paths, suggested fixes, or copied review summaries is not by itself a
request to run `/roborev-fix`.

Use this skill when the user explicitly invokes `/roborev-fix`, asks to fix
open failing/unaddressed reviews (in any phrasing), provides job IDs that need
fetching, or gives a mix of job IDs and pasted findings.

## IMPORTANT

You must **execute bash commands** to complete this task. Skip steps already satisfied by conversation context. Defer to CLAUDE.md when it conflicts.

## Instructions

When the user invokes `/roborev-fix [job_id...]`:

### 1. Gather findings

**Check the conversation first.** If the user has already pasted review
findings (verdicts, severities, file paths, suggested fixes), use those
directly. Do not re-fetch reviews that are already present in the
conversation. When reusing pasted findings, collect any job IDs mentioned
alongside them — step 5 needs these to comment on and close the reviews.
If job IDs are missing from the pasted output, discover them via
`roborev fix --list` and match each pasted finding to the correct
job by commit SHA or reviewed file paths. If a finding cannot be
confidently matched to a specific job, ask the user for the job ID
rather than closing the wrong review.

If job IDs are provided and findings are NOT already in the conversation,
fetch them:

```bash
roborev show --job <job_id> --json
```

If no job IDs are provided and no findings are in the conversation, discover
open failing reviews:

```bash
roborev fix --list
```

This lists each actionable open failing job with its ID, commit SHA/ref, agent, and summary (a panel review shows as its synthesis parent).
Collect the job IDs from the output.

If the command fails, report the error to the user. Common causes: the daemon
is not running, or the repo is not initialized (suggest `roborev init`).

If no open failing reviews are found, inform the user there is nothing to fix.

### 2. Fetch reviews (if needed)

Skip this step if findings are already available from step 1.

For each job ID, fetch the full review as JSON:

```bash
roborev show --job <job_id> --json
```

If the command fails for a job ID, report the error and continue with the remaining jobs.

The JSON output has this structure:
- `job_id`: the job ID
- `output`: the review text containing findings
- `job.verdict`: `"P"` for pass, `"F"` for fail (may be empty if the review errored)
- `job.git_ref`: the reviewed git ref (SHA, range, or synthetic ref)
- `closed`: whether this review has already been closed
- `comments`: array of comments left on this review (may be empty or absent)
  - Each comment has `responder` (who left it) and `response` (the text)
  - Comments from `roborev-fix` or `roborev-refine` are automated tool records
  - All other comments are from the developer (user feedback)

A discovered actionable open failing job may be a **synthesis (panel) parent**. Its `output` and
`job.verdict` are the synthesized result across the panel's reviewers, so fix
from the parent exactly as you would a single review. When the job is a panel,
`show --json` also includes an additive top-level `panel` block:

- `run_uuid`, `name`, `synthesis_job_id`
- `members`: array of reviewers, each with `job_id`, `name`, `agent`,
  `review_type`, `status`, and `verdict` (empty or absent until the member finishes)

Discovery lists parents only (synthesis jobs and non-panel reviews), never
individual members. Comment on and close the parent. Drill into a member's own
review (`show --json --job <member_job_id>`) only if the user explicitly asks.

Skip any reviews where `job.verdict` is `"P"` (passing reviews have no findings to fix).
Skip any reviews where `job.verdict` is empty or missing (the review may have errored and is not actionable).
Skip any reviews where `closed` is `true`, unless the user explicitly provided that job ID (in which case, warn them and ask to confirm).

If all discovered reviews are passed, closed, or otherwise skipped, inform the user there is nothing to fix.

If the review has `comments`, respect any developer feedback (false positives, preferred approaches).

The actionable closure set is exactly the non-skipped failing job IDs collected
in steps 1-2. Keep this original job list separate from any jobs created later
by commit hooks or follow-up reviews.

### 3. Fix all findings

If a finding's context is unclear from the review output alone and `job.git_ref` is not `"dirty"`, run `git show <git_ref>` to see the original diff. Only do this when needed — the review output usually contains enough detail (file paths, line numbers, descriptions) to fix findings directly.

Parse findings from the `output` field of all failing reviews. Collect every finding with its severity, file path, and line number. Then:

1. **Sort by severity**: fix HIGH findings first, then MEDIUM, then LOW
2. **Group by file**: within each severity level, batch edits to the same file to minimize context switches
3. If the same file has findings from multiple reviews, fix them all together in one edit
4. If some findings cannot be fixed (false positives, intentional design), note them for the comment rather than silently skipping them

### 4. Run tests

Run the project's test suite to verify all fixes work:

```bash
go test ./...
```

Or whatever test command the project uses. If tests fail, fix the regressions before proceeding.

### 5. Record comments and close reviews

Closure ordering is mandatory. After fixes are verified, comment on and close
exactly the original actionable job IDs from steps 1-2 before waiting on,
fetching, or responding to any new review created by commit hooks. Do not treat
a post-fix auto-review as a prerequisite for closing the original addressed
reviews; handle that new review in a separate `/roborev-fix` cycle.

If repository policy requires committing before close comments can reference a
SHA, perform step 6 first, then immediately return here and close the original
job set. Otherwise, close before committing.

For each original job that was fixed, record a summary comment and then close
it. Run these as **separate commands**, but only run `roborev close` after
confirming the comment succeeded:

```bash
roborev comment --commenter roborev-fix --job <job_id> "<summary of changes>"
# Only if the comment above succeeded:
roborev close <job_id>
```

The comment should reference each finding by severity and file, state what was fixed, and note any findings intentionally skipped. Keep it concise (1-3 sentences). Escape quotes and special characters in the bash command.

### 6. Commit

Follow the project's commit conventions (see CLAUDE.md). If the project
instructs you to always commit, do so without asking.

### 7. Audit original closures

Before the final response, explicitly audit the original actionable job IDs and
verify each reports `closed=true`:

```bash
roborev show --job <job_id> --json
```

Do not rely on `roborev list --open` for this audit; unrelated open reviews can
obscure whether the original closure set was handled.

## Examples

**Pasted findings in the prompt:**

User: "Roborev found HIGH in foo.go:42 and MEDIUM in bar.go:10 ..."

Agent:
1. Treats the pasted findings as direct fix input
2. Fixes the code directly without invoking `/roborev-fix`
3. Only uses roborev commands if the user later asks to comment on or close a specific review

**Auto-discovery:**

User: `/roborev-fix`

Agent:
1. Runs `roborev fix --list` and finds 2 open failing reviews: job 1019 and job 1021
2. Fetches both reviews with `roborev show --job 1019 --json` and `roborev show --job 1021 --json`
3. Runs `git show <git_ref>` for one review where the finding lacked enough context
4. Fixes all 3 findings across both reviews, sorted by severity, grouped by file
5. Runs `go test ./...` to verify
6. Records comments and closes reviews:
   - `roborev comment --commenter roborev-fix --job 1019 "Fixed null check and added error handling"`
   - `roborev close 1019`
   - `roborev comment --commenter roborev-fix --job 1021 "Fixed missing validation"`
   - `roborev close 1021`
7. Commits the changes per project conventions, or commits before step 6 if repository policy requires a SHA in close comments
8. Audits jobs 1019 and 1021 with `roborev show --job <job_id> --json` and verifies `closed=true`

**Explicit job IDs:**

User: `/roborev-fix 1019 1021`

Agent:
1. Skips discovery, fetches job 1019 and 1021 directly
2. Job 1019 is verdict Fail with 2 findings; job 1021 is verdict Pass — skips 1021, informs user
3. Fixes the 2 findings from job 1019
4. Runs `go test ./...` to verify
5. Records comment and closes review:
   - `roborev comment --commenter roborev-fix --job 1019 "Fixed null check in foo.go and error handling in bar.go"`
   - `roborev close 1019`
6. Commits the changes per project conventions, or commits before step 5 if repository policy requires a SHA in close comments
7. Audits job 1019 with `roborev show --job 1019 --json` and verifies `closed=true`

## See also

- `/roborev-respond` — comment on a review and close it without fixing code
