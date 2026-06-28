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
This uses the existing `MaskValue` behavior for sensitive keys, which reveals
only the last four characters.

The setting is hot-reloaded with the rest of the CI poller config. The CI
poller's event handler must read `cfgGetter.Config().CI.DiscordWebhookURL` for
each failed event instead of capturing the URL at startup.

## Runtime Architecture

The CI poller already subscribes to daemon review lifecycle events for PR
posting. Extend the existing `handleReviewFailed` path to also attempt Discord
notification. Do not start a separate notifier goroutine or subscribe a second
event listener; the existing event loop is the right ownership boundary for
CI-only behavior and avoids a start/stop lifecycle tied to config reloads.

For each failed event:

1. Read the current webhook URL from `cfgGetter.Config()`. If it is empty, do
   nothing and do not load the job.
2. Load the job with `db.GetJobByID(event.JobID)`.
3. Ignore missing jobs and jobs where `ReviewJob.IsCIReview()` is false.
4. Apply quota/cooldown dedupe before posting.
5. Build a concise Discord payload from the event and job metadata.
6. POST to the configured webhook URL with a short HTTP timeout.
7. Log delivery failures with the webhook URL redacted.

`db.GetJobByID` is required even though the event has some metadata. The event
does not carry job source, review type, retry count, or panel fields. Loading
the job is therefore necessary both to apply `ReviewJob.IsCIReview()` and to
populate the CI-specific context in the Discord message. This is race-free
because `FailJob` commits the terminal row before `broadcastFailed` emits the
event.

This keeps worker failure handling unchanged. The worker remains responsible for
classifying failures, storing the final error, broadcasting `review.failed`, and
releasing CI panel synthesis when needed.

## Quota/Cooldown Fan-Out Control

Quota cooldown is the highest-noise failure mode. After an agent enters
cooldown, every CI job routed to that agent can fail immediately with
`quota: agent <name> quota cooldown active` until the cooldown expires, unless a
healthy backup agent handles the job. Without suppression, one underlying quota
event can produce one Discord message per PR, review type, and panel member.

For `quota/cooldown` failures, the CI poller keeps an in-memory dedupe map keyed
by canonical agent name. The first quota/cooldown failure for an agent sends a
Discord message. Further quota/cooldown failures for that same agent are
suppressed until the dedupe expiry. Use
`config.ResolveAgentQuotaCooldown(cfg)` as the dedupe duration because it is the
same daemon-wide cap that bounds the worker's agent cooldown; provider reset
hints may shorten the worker's actual cooldown, but that exact expiry is not
available on the failure event. Non-quota failures are not deduped.

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

- `quota/cooldown` for errors matching `review.QuotaErrorPrefix`, including
  `agent <name> quota cooldown active`.
- `provider/session outage` for errors matching `review.OutageErrorPrefix`.
- `timeout` for prefixless agent timeout errors containing
  `agent timeout after`.
- `error` for everything else.

Use the existing `review` package constants and helpers where they apply:
`review.QuotaErrorPrefix`, `review.OutageErrorPrefix`,
`review.TimeoutErrorPrefix`, `review.IsQuotaFailure`,
`review.IsTransientFailure`, `review.IsTimeoutCancellation`, and
`review.IsGenuineFailure`. The `agent timeout after` check is CI-specific
because worker member timeouts are stored without `review.TimeoutErrorPrefix`.
Canceled jobs broadcast `review.canceled`, not `review.failed`, and are not in
scope for this notification path.

The message should not include `job.RepoPath`, `WorktreePath`, prompt text,
agent output, command lines, tokens, or full local file paths.

## Error Handling

Notification is best-effort:

- Empty webhook URL skips the notification path.
- Invalid webhook URLs are logged and skipped.
- HTTP 2xx is success.
- HTTP non-2xx logs status and a limited response body.
- Request failures log a redacted URL and sanitized error.
- A failed notification does not retry inside the notification path. CI poller
  retries remain governed by the existing CI retry state.

Mirror the existing generic webhook delivery pattern: a five-second HTTP client
timeout, `redactWebhookURL`, and `redactURLError`. Those helpers already avoid
logging secret webhook path segments, credentials, query strings, and fragments.

## Testing

Add focused unit tests:

- Config loads `ci.discord_webhook_url` and treats `ci.discord_webhook_url` as
  sensitive.
- The webhook URL is read fresh at event time: setting or clearing it through a
  mutable `ConfigGetter` affects the next `review.failed` handling without
  restarting the poller.
- Discord payload builder includes CI failure context and classifies
  quota/cooldown errors.
- Discord payload builder classifies `agent timeout after ...` as `timeout`.
- Discord notification path ignores non-CI failed jobs.
- Discord notification path posts to a test HTTP server for CI failed jobs.
- Discord notification path dedupes quota/cooldown notifications per canonical
  agent during the configured agent quota cooldown window.
- Discord notification path redacts webhook URL credentials/query details in
  logs on HTTP failure.

Existing worker and CI poller tests should not need behavior changes because the
worker event flow remains unchanged.

## Documentation

Update CI poller documentation to show:

```toml
[ci]
discord_webhook_url = "https://discord.com/api/webhooks/..."
```

Document that notifications are daemon-local, hot-reloaded, best-effort,
global-only, deduped for quota/cooldown bursts, and sent for CI job failures.
