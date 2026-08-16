# Consolidate the Agent Hook Daemon Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan directly in the current agent, task-by-task. Never use subagent-driven development. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the regular roborev daemon load, serve, and persist Agent Hook
session state while removing the separate Agent Hook daemon.

**Approved spec/design:**
`docs/superpowers/specs/2026-08-15-consolidate-agent-hook-daemon-design.md`

**Architecture:** The Agent Hook state machine receives repository and review
data through a small source interface instead of importing the daemon package.
The regular daemon implements that interface from its SQLite database, loads
the existing Agent Hook JSON snapshot during construction, and owns three API
operations for events, session status, and reset. Agent Hook CLI callbacks post
directly to the regular daemon and fail open when it is unavailable.

**Tech Stack:** Go 1.26.6, Cobra, Huma HTTP API, SQLite storage, testify.

## Global Constraints

- Preserve `${ROBOREV_DATA_DIR}/agent-hook/state.json` as the only Agent Hook
  state format and path.
- Do not add a SQLite migration, dual read/write path, fallback daemon, command
  alias, or compatibility shim.
- Do not add legacy-daemon discovery, takeover, shutdown, signaling, or a
  version-bounded sunset mechanism. Upgrading operators stop the old daemon
  before starting the new binary.
- Preserve fail-open behavior for coding-agent hook callbacks.
- Remove the separate daemon's runtime records, socket, lifecycle commands,
  logs, and address override.
- Use testify assertions and the repository's isolated test environment.
- Never run branch code against the live roborev daemon or live data directory.
- Never invoke `roborev review`.

---

### Task 1: Decouple the Agent Hook State Machine from Daemon Discovery

**Files:**

- Modify: `internal/agenthook/state.go`
- Modify: `internal/agenthook/types.go`
- Modify: `internal/agenthook/state_test.go`
- Modify: `internal/agenthook/config.go`
- Modify: `internal/agenthook/config_test.go`
- Modify: `cmd/roborev/agent_hook_client.go`
- Modify: `cmd/roborev/agent_hook_handler.go`
- Modify: `cmd/roborev/agent_hook_cmd.go`
- Modify: `cmd/roborev/agent_hook_test.go`
- Delete: `cmd/roborev/agent_hook_daemon.go`
- Delete: `internal/agenthook/client.go`
- Delete: `internal/agenthook/client_test.go`
- Delete: `internal/agenthook/daemon.go`
- Delete: `internal/agenthook/daemon_test.go`
- Delete: `internal/agenthook/lifecycle.go`
- Delete: `internal/agenthook/lifecycle_test.go`

**Interfaces:**

- Produces:
  `type ReviewSource interface { ResolveTrackedRepo(context.Context, string,
  string) (TrackedRepoResolution, bool); ListOpenReviewJobs(context.Context,
  string, string) ([]storage.ReviewJob, bool) }`.
- Produces: `func NewHTTPReviewSource(addr string) ReviewSource` for isolated
  state-machine tests and no production daemon ownership.
- Produces: `func LoadState(source ReviewSource) (*StateStore, error)`.
- Produces: `func (s *StateStore) Sessions() map[string]SessionState` and
  `func (s *StateStore) Reset(sessionID string, all bool) error`.
- Removes serialized `Request.RoborevServerAddr`; main-daemon selection remains
  an `Options` concern in the CLI.
- Produces:
  `func postAgentHook(ctx context.Context, addr string, req agenthook.Request)
  (agenthook.Response, error)`. `roborevAgentHookHandler` passes
  `h.opts.RoborevServerAddr` as `addr`; callers with no override pass an empty
  string and use regular-daemon discovery.

- [ ] **Step 1: Write failing state-store ownership tests**

Add focused tests that construct a store with a source and prove public session
inspection returns an independent snapshot and reset persists the selected
change:

```go
func TestStateStoreSessionsReturnsSnapshot(t *testing.T) {
    store := &StateStore{
        path: filepath.Join(t.TempDir(), "state.json"),
        sessions: map[string]SessionState{"session-1": {Count: 2}},
    }

    store.sessions["session-1"] = SessionState{
        Count: 2,
        StopCountsSincePrompt: map[string]int{"repo-a": 3},
    }
    got := store.Sessions()
    gotSession := got["session-1"]
    gotSession.StopCountsSincePrompt["repo-a"] = 9
    got["session-1"] = gotSession

    assert.Equal(t, 2, store.Sessions()["session-1"].Count)
    assert.Equal(t, 3, store.Sessions()["session-1"].StopCountsSincePrompt["repo-a"])
}

func TestStateStoreResetRollsBackWhenSaveFails(t *testing.T) {
    store := &StateStore{
        path: t.TempDir(), // atomic rename cannot replace this directory
        sessions: map[string]SessionState{"session-1": {Count: 2}},
    }

    err := store.Reset("session-1", false)

    require.Error(t, err)
    assert.Equal(t, 2, store.Sessions()["session-1"].Count)
}

func TestStateStoreResetPersistsSelectedSession(t *testing.T) {
    t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
    path := StatePath()
    store := &StateStore{
        path: path,
        sessions: map[string]SessionState{
            "session-1": {Count: 1},
            "session-2": {Count: 2},
        },
    }

    require.NoError(t, store.Reset("session-1", false))

    body, err := os.ReadFile(path)
    require.NoError(t, err)
    var snapshot Snapshot
    require.NoError(t, json.Unmarshal(body, &snapshot))
    assert.NotContains(t, snapshot.Sessions, "session-1")
    assert.Contains(t, snapshot.Sessions, "session-2")
}
```

- [ ] **Step 2: Run the focused tests and verify the expected compile failure**

Run:

```bash
ROBOREV_DATA_DIR="$(mktemp -d)" go test ./internal/agenthook -run 'TestStateStore(SessionsReturnsSnapshot|ResetPersistsSelectedSession)' -count=1
```

Expected: FAIL because `Sessions`, `Reset`, and the source-aware load helper do
not exist.

- [ ] **Step 3: Introduce the review-source boundary and state operations**

Move HTTP repository resolution and job listing behind `ReviewSource`. Keep
lineage matching, review-type filtering, and verdict counting in the Agent Hook
package. Make every `StateStore` use its configured source:

```go
type StateStore struct {
    mu       sync.Mutex
    path     string
    sessions map[string]SessionState
    reviews  ReviewSource
}

type ReviewSource interface {
    ResolveTrackedRepo(ctx context.Context, path, branch string) (TrackedRepoResolution, bool)
    ListOpenReviewJobs(ctx context.Context, repoRoot, branch string) ([]storage.ReviewJob, bool)
}
```

`NewHTTPReviewSource` must parse only a supplied loopback/unix address and must
not discover or start any process. `countOpenFailedReviews` must consume jobs
from the interface and retain the existing `failedReviewCountsForHead` checks.
Remove the `internal/daemon` import from `internal/agenthook`.

Implement `Sessions` with deep clones under the mutex. Implement `Reset` with
the same atomic save and rollback semantics as event recording.

- [ ] **Step 4: Convert existing state tests to explicit HTTP sources**

For tests that already use `httptest.Server`, construct stores with
`reviews: NewHTTPReviewSource(server.URL)` or call source methods directly.
Remove `RoborevServerAddr` from test requests. Do not add tests asserting that
deleted symbols remain absent.

- [ ] **Step 5: Write and run a failing regular-daemon routing test**

Add `TestPostAgentHookUsesRegularDaemonEndpoint` around an `httptest.Server`.
Call `postAgentHook(ctx, server.URL, request)`, assert the received path is
`/api/agent-hook/event`, and return a response that the client must decode.
Run:

```bash
ROBOREV_DATA_DIR="$(mktemp -d)" go test ./cmd/roborev -run TestPostAgentHookUsesRegularDaemonEndpoint -count=1
```

Expected: FAIL because `postAgentHook` still delegates to the auxiliary daemon
client and does not accept an address.

- [ ] **Step 6: Implement direct regular-daemon CLI clients**

Implement the explicit `postAgentHook(ctx, addr, req)` signature above. When
`addr` is non-empty, parse it as a regular `daemon.DaemonEndpoint`; otherwise
use `getDaemonEndpoint`. POST event JSON to `/api/agent-hook/event`, GET
`/api/agent-hook/sessions`, and POST reset JSON to
`/api/agent-hook/reset`. Use a five-second client timeout and include non-200
response bodies in returned errors.

Add an override-routing test that supplies one HTTP server through
`Options.RoborevServerAddr` while regular discovery points elsewhere, then
asserts the event reaches the override server. Preserve fail-open handler
behavior and do not call `ensureDaemon` from callbacks.

- [ ] **Step 7: Remove the auxiliary daemon lifecycle implementation**

Stop registering `agentHookDaemonCmd`. Delete the auxiliary daemon client,
server, lifecycle, runtime record, log, socket, and their tests. Remove
`ServiceName`, `ROBOREV_AGENT_HOOK_DAEMON_ADDR`, and parsing for that override.
Preserve the regular-daemon `--roborev-server` and environment override.

- [ ] **Step 8: Run Agent Hook and CLI package tests**

Run:

```bash
ROBOREV_DATA_DIR="$(mktemp -d)" go test ./internal/agenthook ./cmd/roborev -count=1
```

Expected: PASS.

- [ ] **Step 9: Commit the state and CLI cutover**

Stage only Task 1 files and commit with subject:

```text
refactor(agent-hook): target the regular daemon
```

---

### Task 2: Materialize Existing JSON State in the Regular Daemon

**Files:**

- Create: `internal/daemon/agent_hook.go`
- Create: `internal/daemon/server_agent_hook_test.go`
- Modify: `internal/daemon/server.go`
- Modify: `internal/daemon/routes.go`
- Modify: `internal/daemon/types.go`

**Interfaces:**

- Consumes: `agenthook.LoadState`, `agenthook.ReviewSource`,
  `agenthook.StateStore.RecordContext`, `Sessions`, and `Reset` from Task 1.
- Produces: `daemonAgentHookSource`, backed by `*storage.DB`.
- Produces HTTP contracts:
  `POST /api/agent-hook/event`, `GET /api/agent-hook/sessions`, and
  `POST /api/agent-hook/reset`.
- Adds `agentHookState *agenthook.StateStore` and `agentHookStateErr error` to
  `Server`; only Agent Hook endpoints consult the load error.

- [ ] **Step 1: Write a failing JSON materialization API test**

Create a test that sets an isolated data directory, writes a real
`agenthook.Snapshot` to `agenthook.StatePath()`, constructs the server, and
reads it through the sessions endpoint:

```go
func TestAgentHookSessionsLoadsExistingSnapshot(t *testing.T) {
    dataDir := t.TempDir()
    t.Setenv("ROBOREV_DATA_DIR", dataDir)
    require.NoError(t, os.MkdirAll(filepath.Dir(agenthook.StatePath()), 0o700))
    body, err := json.Marshal(agenthook.Snapshot{Sessions: map[string]agenthook.SessionState{
        "session-1": {Count: 7, ReminderPromptCount: 2},
    }})
    require.NoError(t, err)
    require.NoError(t, os.WriteFile(agenthook.StatePath(), body, 0o600))
    server, _, _ := newTestServer(t)

    got := serveHuma(t, server, http.MethodGet, "/api/agent-hook/sessions", nil)

    require.Equal(t, http.StatusOK, got.Code)
    var output AgentHookSessionsOutput
    require.NoError(t, json.Unmarshal(got.Body.Bytes(), &output.Body))
    assert.Equal(t, 7, output.Body.Sessions["session-1"].Count)
    assert.Equal(t, 2, output.Body.Sessions["session-1"].ReminderPromptCount)
}
```

- [ ] **Step 2: Run the materialization test and verify the expected failure**

Run:

```bash
ROBOREV_DATA_DIR="$(mktemp -d)" go test ./internal/daemon -run TestAgentHookSessionsLoadsExistingSnapshot -count=1
```

Expected: FAIL because the regular daemon does not own Agent Hook state or
register the endpoint.

- [ ] **Step 3: Implement the database-backed review source**

In `internal/daemon/agent_hook.go`, resolve registered repositories using the
same main-root, identity, and snooze rules as `humaResolveRepo`. List only done,
open job metadata with `storage.WithoutPrompt`, branch-or-empty filtering, and
panel-member exclusion. Return `false` on storage errors so Agent Hook reminder
evaluation fails open.

- [ ] **Step 4: Load the JSON snapshot during server construction**

After creating the `Server` value and before registering routes, call:

```go
s.agentHookState, s.agentHookStateErr = agenthook.LoadState(daemonAgentHookSource{db: db})
```

Do not fail `Server.Start` when `agentHookStateErr != nil`. Preserve the error
on `Server`, do not overwrite or reset the unreadable snapshot, and let only
Agent Hook endpoints return the wrapped state-loading error for that server's
lifetime. Recovery requires repairing or removing the file and restarting the
regular daemon. The normal test package remains isolated through its existing
`TestMain`.

- [ ] **Step 5: Register event, sessions, and reset operations**

Define Huma request/response types in `internal/daemon/types.go`. Each handler
first returns a clear service-unavailable error when Agent Hook state failed to
load. Otherwise, the event handler rejects an empty session ID, calls
`RecordContext`, and returns the `agenthook.Response`. Sessions returns
`StateStore.Sessions`. Reset requires either `all=true` or a non-empty session
ID and calls `StateStore.Reset`.

- [ ] **Step 6: Add distinct event, reset, and corrupt-state startup tests**

Add an event API test that registers a real test repository, seeds a done/open
failed review in the database, posts a Stop event, and asserts the response and
persisted snapshot reflect the event. This test must exercise
`daemonAgentHookSource.ResolveTrackedRepo` and `ListOpenReviewJobs`, not a mock.

Add a separate reset API test that starts from a JSON snapshot with two
sessions, resets one session through `/api/agent-hook/reset`, and proves both
the sessions endpoint and the JSON file retain only the other session.

Add one corrupt-state test that writes invalid JSON, constructs the server,
asserts a normal unrelated endpoint such as `/api/status` remains available,
asserts all three Agent Hook endpoints return the clear state-loading error,
and verifies the invalid file bytes remain unchanged.

- [ ] **Step 7: Run focused daemon tests**

Run:

```bash
ROBOREV_DATA_DIR="$(mktemp -d)" go test ./internal/daemon -run 'TestAgentHook' -count=1
```

Expected: PASS.

- [ ] **Step 8: Commit main-daemon state ownership**

Stage only Task 2 files and commit with subject:

```text
feat(daemon): own Agent Hook session state
```

---

### Task 3: Update User Documentation and Verify the Repository

**Files:**

- Modify: `README.md`
- Modify: `docs/agent-hook.md`
- Modify: `docs/commands.md`
- Modify: `docs/development.md`
- Modify: `docs/automation/post-commit-reviews.md` when it describes separate
  lifecycle behavior.
- Modify: `docs/changelog.md`

**Interfaces:**

- Documents the same commands retained in the approved spec.
- Documents the existing JSON snapshot as state materialized by the regular
  daemon.
- Removes the second-daemon lifecycle commands and address override from public
  docs.

- [ ] **Step 1: Rewrite Agent Hook runtime documentation**

State that installed hooks post normalized events to the regular roborev daemon,
which loads and persists session accounting at
`${ROBOREV_DATA_DIR}/agent-hook/state.json`. Preserve fail-open and at-most-once
delivery explanations. Remove manual Agent Hook daemon management examples.
Add an upgrade note telling operators of releases with the auxiliary daemon to
run that release's `roborev agent-hook daemon stop` before installing or
starting the new release.

- [ ] **Step 2: Update command and configuration references**

Remove `roborev agent-hook daemon start|status|stop|restart` and
`ROBOREV_AGENT_HOOK_DAEMON_ADDR`. Keep `--roborev-server` and
`ROBOREV_AGENT_HOOK_ROBOREV_ADDR` as regular-daemon overrides. Add a concise
changelog entry describing automatic reuse of existing JSON counters.

- [ ] **Step 3: Format documentation**

Run:

```bash
make markdown
```

Expected: documentation formatting completes successfully.

- [ ] **Step 4: Run focused and repository-wide quality gates in scratch state**

Create one owner-private scratch directory and use it for every command:

```bash
umask 077
scratch_data=$(mktemp -d)
ROBOREV_DATA_DIR="$scratch_data" go test ./internal/agenthook ./internal/daemon ./cmd/roborev -count=1
ROBOREV_DATA_DIR="$scratch_data" go test ./... -count=1
ROBOREV_DATA_DIR="$scratch_data" go build ./...
make lint-ci
make markdown-ci
```

Expected: every command exits zero. Do not run or install the branch binary.

- [ ] **Step 5: Review final scope**

Run `git status --short`, `git diff`, and `git diff --check`. Confirm the diff
contains no unrelated files, no private data, no second-daemon runtime code,
and no generated binary. Use search only as a review aid; do not add absence
tests.

- [ ] **Step 6: Commit the plan and implementation documentation**

Stage the implementation plan, all Task 3 documentation, and any necessary
formatting or integration correction. Commit with subject:

```text
docs: describe Agent Hook on the regular daemon
```

- [ ] **Step 7: Push and verify the branch**

Follow the repository completion workflow without changing branches:

```bash
git fetch origin
git rebase origin/main
git push -u origin HEAD
git status --short --branch
```

Expected: the branch reports that it is up to date with its upstream and the
working tree is clean.
