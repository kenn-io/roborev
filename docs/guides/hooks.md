---
title: Review Hooks
description: Run shell commands automatically when reviews complete or fail
---

roborev runs reviews in the background. Without hooks, you find out about results by checking the TUI or running `roborev status`. Hooks close that gap -- they run shell commands automatically when reviews finish, so you can get notified, create issues, update dashboards, or trigger downstream workflows without polling.

Common use cases:

- **Desktop notifications** so you know when a review is ready
- **Issue creation** when reviews find problems (GitHub/GitLab/Jira via shell)
- **[Beads](https://github.com/steveyegge/beads) integration** to auto-create tracked issues for review failures and findings (built-in, zero config)
- **Chat notifications** to Slack, Discord, or Teams channels
- **Logging** review outcomes to a file or external service
- **CI-like workflows** that run scripts when reviews pass or fail

## Quick Start

The simplest useful hook is a desktop notification. Add this to `~/.roborev/config.toml` to get notified whenever any review finishes:

```toml
# macOS
[[hooks]]
event = "review.completed"
command = "osascript -e 'display notification \"Review done for {repo_name} ({sha}): {verdict}\" with title \"roborev\"'"

# Linux (requires libnotify)
[[hooks]]
event = "review.completed"
command = "notify-send roborev 'Review done for {repo_name}: {verdict}'"
```

No daemon restart needed -- hooks are picked up via hot-reload.

## Configuration

Hooks are configured as `[[hooks]]` entries in your global `~/.roborev/config.toml` or per-repo `.roborev.toml`. Each hook has an `event` to match and either a `command` to run or a built-in `type`.

```toml
[[hooks]]
event = "review.failed"
command = "/path/to/my-script.sh {job_id}"

[[hooks]]
event = "review.completed"
command = "echo {repo_name} {sha} {verdict} >> ~/review-log.txt"

[[hooks]]
event = "review.*"
branches = ["main", "release/*"]
type = "kata"
```

| Field | Type | Description |
|-------|------|-------------|
| `event` | string | Event pattern to match (see [Events](#events)) |
| `branches` | array | Optional branch allowlist using `path.Match` globs. Empty or omitted means all branches |
| `command` | string | Shell command to run, with `{var}` template interpolation |
| `type` | string | Built-in hook type: `"beads"`, `"kata"`, or `"webhook"`. Empty or `"command"` runs `command` |
| `url` | string | Webhook destination URL (required when `type = "webhook"`) |
| `project` | string | Kata hook project override. Defaults to the repo's `.kata.toml` binding |
| `labels` | array | Extra labels for Kata-created issues. `roborev` is always added |
| `priority` | integer | Kata issue priority (`0`-`4`). Omit to use roborev's defaults |

### Global vs Per-Repo Hooks

**Global hooks** (`~/.roborev/config.toml`) fire for every repo. Use these for notifications, logging, or anything that applies everywhere.

**Per-repo hooks** (`.roborev.toml`) fire only for that repo. Use these for repo-specific workflows like creating issues in the right tracker.

Both fire for matching events -- per-repo hooks run in addition to global hooks, not instead of them. This means you can have a global notification hook and a per-repo issue-creation hook, and both will fire.

```toml
# ~/.roborev/config.toml -- fires for all repos
[[hooks]]
event = "review.completed"
command = "echo {repo_name} {sha} {verdict} >> ~/roborev-events.log"

[[hooks]]
event = "review.failed"
command = "echo {repo_name} {sha} {error} >> ~/roborev-events.log"
```

```toml
# ~/code/myproject/.roborev.toml -- fires only for this repo
[[hooks]]
event = "review.failed"
type = "beads"
```

## Events

| Event | When it fires |
|-------|--------------|
| `review.started` | A review job starts processing |
| `review.completed` | A review finishes successfully (verdict is `P` for pass or `F` for fail) |
| `review.failed` | A review job fails after exhausting retries (agent error, timeout, etc.) |
| `review.canceled` | A running review is canceled. Canceling a still-queued job emits no event |
| `review.closed` | A review is marked closed |
| `review.reopened` | A closed review is reopened |
| `review.*` | Wildcard: matches any `review.` event |

Note the distinction between `review.completed` with verdict `F` (the review ran successfully and found issues) and `review.failed` (the review job itself errored out). For terminal notifications only, configure separate `review.completed` and `review.failed` hooks. `review.*` also matches started, canceled, closed, and reopened events, which is useful for webhooks and built-ins that ignore irrelevant event types internally.

## Branch Filtering

Add `branches` to any hook to run it only for selected branches:

```toml
[[hooks]]
event = "review.*"
branches = ["main", "release/*"]
command = "notify-send roborev 'Review event on {repo_name}: {sha}'"
```

Patterns use Go's `path.Match` rules: `main` matches exactly, `release/*` matches `release/1.2`, and an invalid pattern is ignored. If `branches` is set and roborev cannot determine a branch for the event, the hook does not run.

For local reviews, the matched branch is the job's local branch. For daemon CI pull-request reviews, the matched branch is the PR base/target branch, not the contributor's head branch. This means `branches = ["main"]` fires for PRs targeting `main`, which is usually what protected-branch integrations want.

## Template Variables

Use `{var}` syntax in your `command` string to inject event data. Variables are automatically shell-escaped (single-quoted) to prevent injection, so **do not wrap placeholders in your own quotes**:

```toml
# Good -- placeholders are auto-escaped
command = "echo {error}"

# Bad -- wrapping in quotes produces nested quoting
command = 'echo "{error}"'
```

| Variable | Description | Available |
|----------|-------------|-----------|
| `{job_id}` | Review job ID (numeric, not quoted) | Always |
| `{repo}` | Absolute repo path | Always |
| `{repo_name}` | Display name for the repo | Always |
| `{sha}` | Commit SHA or git ref | Always |
| `{agent}` | Agent that ran the review (e.g., `codex`, `claude-code`) | Always |
| `{verdict}` | `P` (pass) or `F` (fail) | `review.completed` only |
| `{findings}` | Full review output from the agent | `review.completed` only |
| `{error}` | Error message describing why the job failed | `review.failed` only |

Variables that aren't available for a given event type interpolate to an empty string (`''`).

## Built-in: Beads Integration

If you use [beads](https://github.com/steveyegge/beads) for issue tracking, roborev has a built-in hook type that creates issues automatically when reviews surface problems. This is the recommended way to track review findings without manual triage.

```toml
# .roborev.toml
[[hooks]]
event = "review.failed"
type = "beads"

[[hooks]]
event = "review.completed"
type = "beads"
```

What the `type = "beads"` hook does for each event:

| Event | Verdict | Action |
|-------|---------|--------|
| `review.failed` | n/a | Creates a **priority-1** issue: "Review failed for \{repo\} (\{sha\}): run roborev show \{job_id\}" |
| `review.completed` | `F` (fail) | Creates a **priority-2** issue: "Review findings for \{repo\} (\{sha\}): roborev show \{job_id\} / one-shot fix with roborev fix \{job_id\}" |
| `review.completed` | `P` (pass) | No action |

The hook runs in the repo's working directory so `bd` finds the correct `.beads/` database. Issue titles include the `roborev show` command so you can jump straight to the full review.

## Built-in: Kata Integration

If you use [Kata](https://github.com/kenn-io/kata) for task tracking, roborev can file review failures and findings as Kata issues. For an overview of both directions of the integration, see [Kata](/integrations/kata/). To enable the hook:

```toml
[[hooks]]
event = "review.*"
type = "kata"
branches = ["main"]
labels = ["from-review"]
priority = 2
```

What the `type = "kata"` hook does for each event:

| Event | Verdict | Action |
|-------|---------|--------|
| `review.failed` | n/a | Creates a priority-1 Kata issue for the failed review job |
| `review.completed` | `F` (fail) | Creates a priority-2 Kata issue for the review findings, including `roborev show` and `roborev fix` commands |
| `review.completed` | `P` (pass) | No action |

The Kata hook runs in the repo's working directory and uses the repo's committed `.kata.toml` binding by default. Set `project = "myproj"` to override the target project. Every issue receives the `roborev` label plus a marker label (`review-failed` or `review-finding`) and any extra `labels` you configure. roborev uses an idempotency key per job/event so reruns do not create duplicate Kata issues.

The `kata` CLI must be on `PATH`. If it is missing, or the repo is not bound to a Kata project, the hook is skipped. If you explicitly configured the hook but Kata fails because the binding is broken or the CLI command fails, the daemon logs the error so the integration does not fail silently.

## Built-in: Webhook Integration

Use `type = "webhook"` to POST the review event as JSON to an external endpoint. This is useful for connecting roborev to dashboards, chatbots, or any service that accepts webhook payloads.

```toml
[[hooks]]
event = "review.completed"
type = "webhook"
url = "https://example.com/roborev-webhook"

[[hooks]]
event = "review.failed"
type = "webhook"
url = "https://example.com/roborev-webhook"
```

The webhook sends a JSON POST with a 5-second timeout. The payload is the full review event as JSON (`type`, `ts`, `job_id`, `job_uuid`, `repo`, `repo_name`, `sha`, `branch`, `agent`, `verdict`, `findings`, `error`, `worktree_path`), with unset fields omitted. See [Event Fields](/advanced/streaming/#event-fields) for field semantics. The `url` field is treated as sensitive and is masked in `roborev config list` output. Webhook URLs are redacted in daemon logs (only the scheme and host are shown).

Webhook delivery is asynchronous and never blocks the review pipeline. Failed deliveries are logged but do not retry.

### Custom Beads Commands

If you want different behavior (different priority, labels, or formatting), write your own beads command using template variables:

```toml
[[hooks]]
event = "review.failed"
command = "bd create 'Agent error on {sha}: run roborev show {job_id}' -p 1"

[[hooks]]
event = "review.completed"
command = "test {verdict} = F && bd create 'Review findings on {sha}: roborev show {job_id}' -p 3"
```

## Examples

### Desktop Notifications

Get notified when reviews finish so you don't have to keep checking:

```toml
# macOS -- uses built-in osascript
[[hooks]]
event = "review.completed"
command = "osascript -e 'display notification \"Review {verdict} for {repo_name}\" with title \"roborev\"'"

# Linux -- requires libnotify (notify-send)
[[hooks]]
event = "review.completed"
command = "notify-send -u normal roborev 'Review {verdict} for {repo_name} ({sha})'"
```

### Slack Notifications

Post to a Slack channel when reviews fail. Set `SLACK_WEBHOOK_URL` in your environment:

```toml
[[hooks]]
event = "review.failed"
command = """curl -sf -X POST \
  -H 'Content-Type: application/json' \
  -d '{"text":"roborev: review failed for {repo_name} ({sha}). Run `roborev show {job_id}` for details."}' \
  $SLACK_WEBHOOK_URL"""
```

For both failures and findings, use two hooks:

```toml
[[hooks]]
event = "review.completed"
command = """test {verdict} = F && curl -sf -X POST \
  -H 'Content-Type: application/json' \
  -d '{"text":"roborev: review findings in {repo_name} ({sha}). Run `roborev show {job_id}`."}' \
  $SLACK_WEBHOOK_URL"""

[[hooks]]
event = "review.failed"
command = """curl -sf -X POST \
  -H 'Content-Type: application/json' \
  -d '{"text":"roborev: review failed for {repo_name} ({sha}). Run `roborev show {job_id}`."}' \
  $SLACK_WEBHOOK_URL"""
```

### GitHub Issues

Create a GitHub issue when a review finds problems. Requires the `gh` CLI:

```toml
[[hooks]]
event = "review.failed"
command = "gh issue create --title 'roborev: review failed for {sha}' --body 'Review job {job_id} failed. Run `roborev show {job_id}` for details.' --label bug"
```

### Event Logging

Log all review outcomes to a file for auditing or metrics:

```toml
# Global -- logs events from all repos
[[hooks]]
event = "review.completed"
command = "printf '%s %s %s %s %s\\n' $(date -Iseconds) {repo_name} {sha} {agent} {verdict} >> ~/.roborev/events.log"

[[hooks]]
event = "review.failed"
command = "printf '%s %s %s %s %s\\n' $(date -Iseconds) {repo_name} {sha} {agent} {error} >> ~/.roborev/events.log"
```

### Running a Script

For complex logic, point the hook at a script. The working directory is the repo, so you have access to the git tree:

```toml
[[hooks]]
event = "review.completed"
command = "~/.roborev/hooks/on-review.sh {job_id} {verdict} {sha}"
```

```bash
#!/usr/bin/env bash
# ~/.roborev/hooks/on-review.sh
job_id=$1 verdict=$2 sha=$3

if [ "$verdict" = "F" ]; then
    roborev show --job "$job_id" >> ~/reviews/findings.log
fi
```

### Conditional Hooks

Shell features work in commands since they run via `sh -c`. Use `&&`, `||`, or `test` to add conditions:

```toml
# Only notify on failures, not passes
[[hooks]]
event = "review.completed"
command = "test {verdict} = F && notify-send roborev 'Findings in {repo_name}'"

# Run different scripts based on verdict
[[hooks]]
event = "review.completed"
command = "test {verdict} = P && ~/hooks/on-pass.sh {job_id} || ~/hooks/on-fail.sh {job_id}"
```

## Behavior

- Hooks run **asynchronously** in goroutines and never block the review pipeline
- Hook errors are **logged** to the daemon log but never cause a review to fail
- Each hook's working directory is set to the **repo path**, so repo-relative commands work
- Commands run via `sh -c`, so shell features (pipes, redirects, `&&`, `||`) work
- Hooks pick up config changes via **hot-reload** -- no daemon restart needed
- Multiple hooks can match the same event; they all fire independently
- Branch filters fail closed: if a hook has a non-empty `branches` list and the event has no branch, the hook does not run

## Git Hooks

In addition to review event hooks, roborev installs git hooks to integrate with your development workflow. These are managed automatically by `roborev init`.

### Post-Rewrite Hook

The `post-rewrite` hook preserves review history when you rebase or amend commits. Without it, rebasing would orphan your reviews because the commit SHAs change.

When git runs `rebase` or `commit --amend`, the hook receives the old and new SHA pairs and calls `roborev remap`. For each pair, roborev:

1. Computes the patch ID for both the old and new commits
2. If the patch content is identical (only the SHA changed), updates the review record to point to the new SHA
3. If the patch content differs (the rebase modified the commit), leaves the review untouched

This means reviews survive clean rebases automatically. If you rebase and resolve conflicts that change a commit's diff, the review for that commit is not remapped (since the code it reviewed no longer matches).

The hook runs silently. If `roborev` is not on `PATH` or the daemon is not running, the hook exits without error and does not block git operations.

## Debugging

Hook output and errors appear in the daemon log. To watch hooks fire in real time:

```bash
# Follow the daemon log (the daemon writes to stderr)
roborev daemon run 2>&1 | grep -i hook
```

If a hook isn't firing, check:

1. The `event` field matches the event type (use `review.*` to catch everything)
2. The config file is valid TOML (`cat ~/.roborev/config.toml | toml-lint` or similar)
3. The command works when run manually from the repo directory
4. The daemon has reloaded the config (check `roborev status` for the config reload counter)
