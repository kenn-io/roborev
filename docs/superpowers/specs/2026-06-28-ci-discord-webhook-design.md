# CI Discord Webhook Notifications Design

## Context

The CI poller runs inside the daemon, creates CI panel jobs for GitHub PRs, and
listens to daemon review lifecycle events to post PR results. When an agent is
quota limited, CI jobs fail with a stored `quota: ...` error and broadcast
`review.failed`. The same event also covers prompt failures, checkout failures,
synthesis failures, and other terminal CI job failures.

Discord webhooks expect a Discord-specific JSON payload such as `content` or
`embeds`. The existing generic `[[hooks]] type = "webhook"` posts raw roborev
event JSON, so it is not suitable for direct Discord notifications.

## Goals

- Add a simple global CI poller Discord webhook setting:
  `ci.discord_webhook_url`.
- Notify Discord for every terminal CI job failure.
- Include context that explains why the job failed when available.
- Keep notification delivery best-effort so webhook failures never affect job
  state, retries, synthesis, or PR posting.
- Mark the webhook URL sensitive in config output.

## Non-Goals

- Do not replace the generic hook system.
- Do not add Discord notifications for local post-commit, manual review, fix,
  refine, or non-CI jobs.
- Do not add multi-channel routing, rich customization, or per-repo Discord
  overrides.
- Do not expose private filesystem paths or prompt/output bodies in the Discord
  message.

## Configuration

Add `DiscordWebhookURL string` to `config.CIConfig`:

```toml
[ci]
discord_webhook_url = "https://discord.com/api/webhooks/..."
```

The field is global-only because the CI poller is a global daemon component and
the request is for a simple notification target. Empty means disabled. The field
is tagged `sensitive:"true"` so `roborev config list` and related output mask it.

The existing CI section already requires a daemon restart to apply changes, so
this setting follows that same restart behavior.

## Runtime Architecture

The CI poller starts one Discord notifier when `ci.discord_webhook_url` is set.
The notifier subscribes to daemon events and listens for `review.failed`.

For each failed event:

1. Load the job with `db.GetJobByID(event.JobID)`.
2. Ignore missing jobs and non-CI jobs.
3. Build a concise Discord payload from the event and job metadata.
4. POST to the configured webhook URL with a short HTTP timeout.
5. Log delivery failures with the webhook URL redacted.

This keeps worker failure handling unchanged. The worker remains responsible for
classifying failures, storing the final error, broadcasting `review.failed`, and
releasing CI panel synthesis when needed.

## Message Content

The Discord message should be a single embed so it is readable in a channel. The
embed title is `roborev CI job failed`.

Fields:

- Repository: `event.RepoName`.
- Job: job ID, panel role, panel name, panel member name, and review type when
  present.
- Agent: the event agent or stored job agent.
- Ref: short head SHA for a range, or short ref for non-range values.
- Branch: CI base branch from `job.HookBranch()` when present.
- Failure: a compact class derived from the stored error.
- Retry count: stored retry count.
- Error: trimmed error text.

Failure classes:

- `quota/cooldown` for errors prefixed with `quota:`, including
  `agent <name> quota cooldown active`.
- `provider/session outage` for errors prefixed with the existing outage
  prefix.
- `timeout/canceled` for timeout cancellation errors.
- `error` for everything else.

The message should not include `job.RepoPath`, `WorktreePath`, prompt text,
agent output, command lines, tokens, or full local file paths.

## Error Handling

Notification is best-effort:

- Empty webhook URL disables the notifier.
- Invalid webhook URLs are logged and skipped.
- HTTP 2xx is success.
- HTTP non-2xx logs status and a limited response body.
- Request failures log a redacted URL and sanitized error.
- A failed notification does not retry inside the notifier; CI poller retries
  remain governed by the existing CI retry state.

## Testing

Add focused unit tests:

- Config loads `ci.discord_webhook_url` and treats `ci.discord_webhook_url` as
  sensitive.
- Discord payload builder includes CI failure context and classifies
  quota/cooldown errors.
- Discord notifier ignores non-CI failed jobs.
- Discord notifier posts to a test HTTP server for CI failed jobs.
- Discord notifier redacts webhook URL credentials/query details in logs on
  HTTP failure.

Existing worker and CI poller tests should not need behavior changes because the
worker event flow remains unchanged.

## Documentation

Update CI poller documentation to show:

```toml
[ci]
discord_webhook_url = "https://discord.com/api/webhooks/..."
```

Document that notifications are daemon-local, best-effort, global-only, and
sent for CI job failures.
