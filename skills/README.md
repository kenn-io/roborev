# roborev Skills

Let AI agents automatically fix issues found in code reviews.

## Installation

```bash
roborev skills install
```

Skills are updated automatically when you run `roborev update`.

## Skills

| Skill | Description |
|-------|-------------|
| `/roborev-fix [job_id...]` | Fix all open review findings (or specific jobs) in one pass |
| `/roborev-design-review <path-or-job-id>` | Review a design proposal for completeness and feasibility |
| `/roborev-respond <job_id> [message]` | Add a response to a review |
| `/roborev-snooze [on\|off] [duration]` | Silence or resume Agent Hook reminders in the current workspace |

## Example Workflow

When you receive a review notification:

```
Review #1019: Fail
- high: Missing null check in foo.go:42
- low: Consider adding error context in bar.go:15
```

Ask your agent to fix it:

```
/roborev-fix 1019
```

The agent will:
1. Fetch the review
2. Read the relevant files
3. Fix issues by priority (high severity first)
4. Run tests to verify
5. Offer to commit the changes

After fixing, document what was done:

```
/roborev-respond 1019 Fixed null check and improved error handling
```

## Supported Agents

| Agent | Invocation |
|-------|------------|
| Claude Code | `/roborev-fix`, `/roborev-design-review`, `/roborev-respond`, `/roborev-snooze` |
| Codex | `$roborev-fix`, `$roborev-design-review`, `$roborev-respond`, `$roborev-snooze` |
