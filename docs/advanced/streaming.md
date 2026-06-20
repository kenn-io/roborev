---
title: Event Streaming & Daemon API
description: Stream review events and integrate with the daemon REST API
---

## Daemon API

The daemon exposes a REST API on the configured `server_addr`. With the default value of `127.0.0.1:7373`, the API is reachable at `http://127.0.0.1:7373`. An OpenAPI 3.1.0 spec is available at `/openapi.json` for client generation and integration tooling. roborev also ships a generated public Go client for integrations that want a stable typed wrapper.

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

These endpoints have typed request/response schemas in the OpenAPI spec. The daemon also exposes endpoints used by the CLI, TUI, and subsystems for enqueueing jobs, streaming job output, reading logs and patches, sync operations, and token backfill. Most JSON endpoints are represented in the generated client; endpoints that stream or return raw bytes are exposed through raw helper methods.

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

The client embeds typed methods generated from `pkg/client/openapi.yaml`. For streaming or raw-byte endpoints, use the hand-written helpers:

| Helper | Endpoint |
|--------|----------|
| `GetJobLogRaw` | `/api/job/log` |
| `GetJobOutputRaw` | `/api/job/output` |
| `GetJobPatchRaw` | `/api/job/patch` |
| `StreamEventsRaw` | `/api/stream/events` |
| `SyncNowRaw` | `/api/sync/now` |

The generated package is intended for integrations that run against the same installed roborev version as the daemon. The CLI and TUI still evolve in lockstep with the daemon, so pin roborev versions when building long-lived external tools.

## Event Stream

Stream review events in real-time for integrations, notifications, or custom tooling:

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

## Event Fields

Common fields:
- `type`: Event type
- `ts`: ISO 8601 timestamp
- `job_id`: Unique job identifier
- `job_uuid`: Stable job UUID when available
- `repo`: Repository path
- `repo_name`: Repository display name
- `sha`: Commit SHA (or `dirty` for uncommitted changes)
- `branch`: Branch used for event and hook matching, when known. For CI pull-request reviews this is the PR base branch
- `agent`: Agent that processed the review, when available
- `worktree_path`: Worktree path used by the job, when different from the main repo path

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

- [TUI](/integrations/tui/) - Interactive terminal interface
- [Commands Reference](/commands/) - Full command list
