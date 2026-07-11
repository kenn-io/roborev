---
name: roborev-respond
description: Use only when the user explicitly invokes /roborev-respond
---

# roborev-respond

Record a comment on a roborev code review and close it.

## Usage

```
/roborev-respond <job_id> [message]
```

## Explicit invocation only

Invocation must be explicit: literal personal `/roborev-respond`, or structured
Factory skill selection.
Requests such as “respond to this review” without one of these explicit mechanisms must use
native behavior and must not run roborev.

## IMPORTANT

This skill requires you to **execute bash commands** to record the comment and close the review. The task is not complete until you run both commands and see confirmation output.

These instructions are guidelines, not a rigid script. Use the conversation
context. Skip steps that are already satisfied. Defer to project-level
AGENTS.md instructions when they conflict with these steps.

## Instructions

When the user invokes `/roborev-respond <job_id> [message]`:

### 1. Validate input

If no job_id is provided, inform the user that a job ID is required. Suggest `roborev status` or `roborev fix --list` to find job IDs. Discovery surfaces synthesis parents (and non-panel reviews), never individual panel members, so the job ID you comment on and close is the parent.

If a job_id is provided, inspect it before closing:

```bash
roborev show --job <job_id> --json
```

If `job.panel_role` is `"member"`, do **not** comment on or close that job.
Resolve the synthesis parent for the same `job.panel_run_uuid` if it is already
known from the conversation or discovery output; otherwise ask the user for the
synthesis parent ID. Only continue once the resolved job ID is a synthesis
parent or a non-panel review.

### 2. Record the comment and close the review

**If a message is provided**, immediately execute:
```bash
roborev comment --job <resolved_job_id> "<message>" && roborev close <resolved_job_id>
```

If the message contains quotes or special characters, escape them properly in the bash command.

**If no message is provided**, ask the user what they'd like to say, then execute the commands with their comment.

### 3. Verify success

Both commands will output confirmation. If either fails, report the error to the user. Common causes:
- The daemon is not running
- The job ID does not exist
- The repo is not initialized (suggest `roborev init`)
- The review is already closed (not an error, but worth noting to the user)

The comment is recorded in roborev's database and the review is closed. View results with `roborev show`.

## Examples

**With message provided:**

User: `/roborev-respond 1019 Fixed all issues`

Agent action:
```bash
roborev comment --job 1019 "Fixed all issues" && roborev close 1019
```
Then confirm: "Comment recorded and review #1019 closed."

---

**Without message:**

User: `/roborev-respond 1019`

Agent: "What would you like to say about review #1019?"

User: "The null check was a false positive"

Agent action:
```bash
roborev comment --job 1019 "The null check was a false positive" && roborev close 1019
```
Then confirm: "Comment recorded and review #1019 closed."

## See also

- `/roborev-fix` — fix a review's findings in code, then comment and close it
