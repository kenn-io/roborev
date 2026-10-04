---
title: Event Streaming & Daemon API
description: Stream review events and integrate with the daemon REST API
---

## Daemon API

When global `auth_key` is configured, all API and streaming requests require
`Authorization: Bearer <key>`. CLI and TUI clients add it automatically. See
[Daemon authentication](../configuration.md#daemon-authentication) for setup and
key rotation.

The daemon exposes a REST API on the configured `server_addr`. With the default
value of `127.0.0.1:7373`, the API is reachable at `http://127.0.0.1:7373`. An
OpenAPI 3.1.0 spec is available at `/openapi.json` for client generation and
integration tooling. roborev also ships a generated public Go client for
integrations that want a stable typed wrapper.

```bash
# Fetch the OpenAPI spec (TCP, default address)
curl http://127.0.0.1:7373/openapi.json

# With Unix domain socket (path depends on your configuration; see Configuration > Unix Domain Socket)
curl --unix-socket "$XDG_RUNTIME_DIR/roborev/daemon.sock" http://localhost/openapi.json
```

### Endpoints

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/jobs` | GET | List review jobs (supports cursor-based pagination via `before` parameter) |
| `/api/review` | GET | Get review by job ID or SHA |
| `/api/comments` | GET | List comments for a job or commit |
| `/api/repos` | GET | List repos with job counts |
| `/api/branches` | GET | List branches with job counts |
| `/api/status` | GET | Get daemon status |
| `/api/health` | GET | Get daemon health checks |
| `/api/activity` | GET | List recent daemon activity |
| `/api/summary` | GET | Get review summary statistics |
| `/api/cost` | GET | Get approximate aggregate review cost |
| `/api/queue/pause` | POST | Pause queue processing |
| `/api/queue/unpause` | POST | Resume queue processing |
| `/api/job/cancel` | POST | Cancel a queued or running job |
| `/api/job/rerun` | POST | Re-enqueue a completed or failed job |
| `/api/review/close` | POST | Close or reopen a review |
| `/api/comment` | POST | Add a comment to a job or commit |

These endpoints have typed request/response schemas in the OpenAPI spec. The
daemon also exposes endpoints used by the CLI, TUI, and subsystems for
enqueueing jobs, streaming job output, reading logs and patches, sync
operations, and token backfill. Most JSON endpoints are represented in the
generated client; endpoints that stream or return raw bytes are exposed through
raw helper methods.

### Worker health

`/api/health` reports stalled jobs through the `workers` component. A job that
has run for more than 30 minutes is unhealthy if it has no active worker or its
assigned execution deadline has passed. Longer reviews remain healthy while
their worker still has time remaining. The deadline includes repository, global,
and panel-member timeout settings resolved when the attempt starts;
configuration reloads do not change it during that attempt.

### CI health

When CI polling is enabled, `/api/health` includes a `ci` component. The
component and overall `healthy` field become false when polling stops,
repository discovery fails, or a repository cannot list its pull requests or
queue a review, including a deferred retry. Failures to list, check, claim, or
remove retries, cancel superseded reviews, clean up closed pull requests, or
re-arm stuck attempts also make CI unhealthy. Failed cleanup remains eligible
for another poll, including when only part of a panel was canceled. An HTTP 200
response alone does not indicate healthy polling.

The `ready` field reports whether the daemon can process work. It requires
healthy database and workers, configured sync health, and a completed initial CI
poll without discovery or repository polling errors. Outstanding review failures
keep `healthy` false while their retries run, but do not prevent `ready` from
becoming true. Deployment checks may use `ready`; availability monitoring must
use `healthy`.

Polling continues for other pull requests and repositories after a failure. Each
repository stays unhealthy until its next successful poll or until a successful
discovery removes it from the configured set. Discovery failures remain
unhealthy even when polling can use cached or partial repository lists.
Successful discovery clears the discovery failure.

A failed review stays unhealthy during backoff and while its retry is queued or
running. Polling restores these failures after a restart. Recovery requires a
posted review with usable output; exhausting retries or abandoning delivery does
not restore health. Ordinary queued reviews with no previous failure are
healthy. An enqueue error alone can clear once polling confirms an active panel.
A first attempt that finishes without usable output also makes CI unhealthy,
including when every reviewer times out.

Each error belongs to that commit. A successful review of another commit does
not clear an older retry's error until the obsolete attempt is removed.
Closed-PR cleanup and removal of obsolete retries clear their errors only after
they succeed. An intentionally empty review matrix removes a claimed retry
instead of re-arming disabled work. Adding a configured skip label removes a
deferred retry and its health error on the next poll, without waiting for
backoff to expire.

Terminal failures do not expire. After an agent or provider repair, restarting
the daemon gives exhausted agent failures a fresh retry budget. Normal polls do
not reset that budget. Polling checks that the PR remains open, eligible, and at
the same commit before retrying. Empty reviews, timeouts, and permanent delivery
failures remain terminal. An explicit rerun of a CI panel retains its GitHub
delivery target, including after the PR closes and reopens, and checks the PR
commit before posting. A historical CI run whose delivery target is missing
cannot be rerun. Recovery leaves previous runs available for inspection, and
health stays unhealthy until a usable review is posted.

The latest requested run determines review status and health. Starting a rerun
leaves the previous GitHub status in place until it produces a result. A failed
rerun can set pending during automatic retries and error when retries stop.
Canceling a rerun does not discard a previously delivered review or cause the
normal poll to review that commit again.

An unresolved PR also clears from health when it closes, advances to a new
commit, or gets a skip label. To acknowledge a failure after retries stop, add a
configured skip label. This waives review while the label remains. Skipping
publishes the skipped check and sets the commit status to success with a "Review
skipped" description before clearing the failure; a publishing error leaves the
failure for the next poll. Later polls leave an identical skip status unchanged.

Cleanup also retires the failed panel, allowing a fresh review if the PR reopens
or the skip label is removed at the same commit. Active and successfully posted
reviews are left unchanged.

Cleanup also removes stored review state for numbers GitHub confirms are
ordinary issues. This verification requires access to GitHub's issue endpoint. A
pull-request lookup returning 404 is insufficient: failed verification retains
the records and keeps CI unhealthy.

CI failures also appear in `recent_errors` with a repository or discovery
summary. Detailed errors remain in the daemon log. Recovery preserves the error
history; use the component and overall `healthy` fields for current health.
These checks report observed failures and stopped polling, without imposing a
poll-duration limit.

CI fetches prune obsolete remote-tracking refs before updating them. This lets
polling recover when a remote branch changes between names such as `feature` and
`feature/update` without a conflicting stale ref blocking new reviews.

## Public Go Client

External Go integrations can import the public daemon client:

```go
import roborevclient "go.kenn.io/roborev/pkg/client"

func main() {
    c, err := roborevclient.New("http://127.0.0.1:7373")
    if err != nil {
        panic(err)
    }
    _ = c
}
```

The client embeds typed methods generated from `pkg/client/openapi.yaml`. For
streaming or raw-byte endpoints, use the hand-written helpers:

| Helper | Endpoint |
|--------|----------|
| `GetJobLogRaw` | `/api/job/log` |
| `GetJobOutputRaw` | `/api/job/output` |
| `GetJobPatchRaw` | `/api/job/patch` |
| `StreamEventsRaw` | `/api/stream/events` |
| `SyncNowRaw` | `/api/sync/now` |

The generated package is intended for integrations that run against the same
installed roborev version as the daemon. The CLI and TUI still evolve in
lockstep with the daemon, so pin roborev versions when building long-lived
external tools.

## Event Stream

Stream review events in real-time for integrations, notifications, or custom
tooling:

```bash
roborev stream              # Stream all events
roborev stream --repo .     # Stream events for current repo only
```

## Event Format

Events are emitted as newline-delimited JSON (JSONL):

```json
{"type":"review.started","ts":"2025-01-11T10:00:00Z","job_id":42,"repo":"/path/to/repo","repo_name":"myrepo","sha":"abc123","agent":"codex"}
{"type":"review.completed","ts":"2025-01-11T10:01:30Z","job_id":42,"repo":"/path/to/repo","repo_name":"myrepo","sha":"abc123","branch":"main","agent":"codex","verdict":"P"}
```

## Event Types

| Type | Description |
|------|-------------|
| `review.started` | Review job started processing |
| `review.completed` | Review finished successfully |
| `review.failed` | Review failed (includes `error` field) |
| `review.canceled` | Review was canceled |
| `review.closed` | Review was marked closed |
| `review.reopened` | Review was reopened |
| `review.commented` | A comment was added to a job or commit |

## Event Fields

Common fields:

- `type`: Event type
- `ts`: ISO 8601 timestamp
- `job_id`: Unique job identifier
- `job_uuid`: Stable job UUID when available
- `repo`: Repository path
- `repo_name`: Repository display name
- `sha`: Commit SHA (or `dirty` for uncommitted changes)
- `branch`: Branch used for event and hook matching, when known. For CI
    pull-request reviews this is the PR base branch
- `agent`: Agent that processed the review, when available
- `worktree_path`: Worktree path used by the job, when different from the main
    repo path

Additional fields:

- `verdict`: Pass/Fail verdict (on `review.completed`)
- `error`: Error message (on `review.failed`)

## Filtering with jq

Use `jq` to filter events:

```bash
# Only completed reviews
roborev stream | jq -c 'select(.type == "review.completed")'

# Only failures
roborev stream | jq -c 'select(.type == "review.failed")'

# Specific repo
roborev stream | jq -c 'select(.repo_name == "myproject")'

# Failed verdicts
roborev stream | jq -c 'select(.verdict == "F")'
```

## Integration Examples

### Desktop Notifications (macOS)

```bash
roborev stream | while read -r event; do
  type=$(echo "$event" | jq -r '.type')
  repo=$(echo "$event" | jq -r '.repo_name')
  if [ "$type" = "review.completed" ]; then
    verdict=$(echo "$event" | jq -r '.verdict')
    if [ "$verdict" = "F" ]; then
      osascript -e "display notification \"Review failed\" with title \"$repo\""
    fi
  fi
done
```

### Webhook

```bash
roborev stream | while read -r event; do
  curl -X POST -H "Content-Type: application/json" \
    -d "$event" https://your-webhook.example.com/reviews
done
```

## See Also

- [TUI](/docs/integrations/tui/) - Interactive terminal interface
- [Commands Reference](/docs/commands/) - Full command list
