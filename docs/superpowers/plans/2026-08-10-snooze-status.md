# Snooze Status Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:executing-plans to implement this plan directly in the current
> agent, task-by-task. Never use subagent-driven development. Steps use checkbox
> (`- [ ]`) syntax for tracking.

**Goal:** Surface every active Agent Hook snooze in `roborev status` and show a
contextual TUI badge for the exact filtered checkout and branch.

**Approved spec/design:**
`docs/superpowers/specs/2026-08-10-snooze-status-design.md`

**Architecture:** Add one storage query for the active local snooze inventory
and include its result in the existing daemon status response. The CLI renders
that shared data as a table, while the TUI compares it with startup worktree
context and its exact active filters before adding a title badge.

**Tech Stack:** Go, SQLite, Huma/OpenAPI, Cobra, Bubble Tea/Lip Gloss, Testify.

## Global Constraints

- Snooze identity remains repository + linked worktree + branch.
- The TUI badge must not appear for broad, aggregate, sibling-worktree, or
  different-branch views.
- Expired snoozes are never reported as active.
- Do not add dependencies or a new daemon endpoint.
- Do not add TUI snooze controls, job-row annotations, or expired-row cleanup.
- Use Testify assertions and test observable owned behavior only.
- Do not create or edit a database migration; the existing table is sufficient.
- Regenerate the checked-in OpenAPI document and Go client through
  `make api-generate`; do not hand-edit generated files.

---

### Task 1: Query the active snooze inventory

**Files:**

- Modify: `internal/storage/agent_hook_snooze.go`
- Test: `internal/storage/agent_hook_snooze_test.go`

**Interfaces:**

- Consumes: existing `agent_hook_snoozes` and `repos` tables.
- Produces: `func (db *DB) ListActiveAgentHookSnoozes(now time.Time) ([]AgentHookSnooze, error)`.
- Produces: `AgentHookSnooze.RepoName string` serialized as `repo_name`.

- [ ] **Step 1: Write the failing storage test**

Add a focused test that proves the query excludes an expired row, includes
repository metadata, and orders active scopes deterministically:

```go
func TestListActiveAgentHookSnoozes(t *testing.T) {
	assert := assert.New(t)
	db := openTestDB(t)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	base := t.TempDir()
	now := time.Date(2026, 8, 10, 15, 0, 0, 0, time.UTC)

	zetaPath := filepath.Join(base, "zeta")
	alphaPath := filepath.Join(base, "alpha")
	for _, repoPath := range []string{zetaPath, alphaPath} {
		_, err := db.GetOrCreateRepo(repoPath)
		require.NoError(t, err)
	}
	_, err := db.SetAgentHookSnooze(
		zetaPath, filepath.Join(base, "zeta-worktree"), "topic/z", now.Add(2*time.Hour),
	)
	require.NoError(t, err)
	_, err = db.SetAgentHookSnooze(
		alphaPath, alphaPath, "main", now.Add(time.Hour),
	)
	require.NoError(t, err)
	_, err = db.SetAgentHookSnooze(
		alphaPath, alphaPath, "release", now.Add(75*time.Minute),
	)
	require.NoError(t, err)
	_, err = db.SetAgentHookSnooze(
		alphaPath, filepath.Join(base, "alpha-worktree"), "topic/a", now.Add(90*time.Minute),
	)
	require.NoError(t, err)
	_, err = db.SetAgentHookSnooze(
		alphaPath, filepath.Join(base, "expired"), "old", now,
	)
	require.NoError(t, err)

	got, err := db.ListActiveAgentHookSnoozes(now)
	require.NoError(t, err)
	require.Len(t, got, 4)
	assert.Equal("alpha", got[0].RepoName)
	assert.Equal(filepath.ToSlash(alphaPath), got[0].RepoPath)
	assert.Equal(filepath.ToSlash(alphaPath), got[0].WorktreePath)
	assert.Equal("main", got[0].Branch)
	assert.Equal(filepath.ToSlash(alphaPath), got[1].WorktreePath)
	assert.Equal("release", got[1].Branch)
	assert.Equal("alpha", got[2].RepoName)
	assert.Equal(filepath.ToSlash(filepath.Join(base, "alpha-worktree")), got[2].WorktreePath)
	assert.Equal("topic/a", got[2].Branch)
	assert.Equal("zeta", got[3].RepoName)
	assert.Equal("topic/z", got[3].Branch)
}
```

- [ ] **Step 2: Run the test and verify RED**

Run:

```bash
go test ./internal/storage -run '^TestListActiveAgentHookSnoozes$'
```

Expected: compilation fails because `ListActiveAgentHookSnoozes` and
`AgentHookSnooze.RepoName` do not exist.

- [ ] **Step 3: Implement the minimal storage query**

Extend the record and add the query in `agent_hook_snooze.go`:

```go
type AgentHookSnooze struct {
	RepoName      string    `json:"repo_name"`
	RepoPath      string    `json:"repo_path"`
	WorktreePath  string    `json:"worktree_path"`
	Branch        string    `json:"branch"`
	SnoozedUntil  time.Time `json:"snoozed_until"`
}

func (db *DB) ListActiveAgentHookSnoozes(now time.Time) ([]AgentHookSnooze, error) {
	rows, err := db.Query(`
		SELECT r.name, r.root_path, s.worktree_path, s.branch, s.snoozed_until
		FROM agent_hook_snoozes s
		JOIN repos r ON r.id = s.repo_id
		WHERE julianday(s.snoozed_until) > julianday(?)
		ORDER BY r.name, s.worktree_path, s.branch`,
		now.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return nil, fmt.Errorf("list active agent hook snoozes: %w", err)
	}
	defer rows.Close()

	snoozes := make([]AgentHookSnooze, 0)
	for rows.Next() {
		var snooze AgentHookSnooze
		var untilRaw string
		if err := rows.Scan(
			&snooze.RepoName, &snooze.RepoPath, &snooze.WorktreePath,
			&snooze.Branch, &untilRaw,
		); err != nil {
			return nil, fmt.Errorf("scan active agent hook snooze: %w", err)
		}
		until, err := time.Parse(time.RFC3339Nano, untilRaw)
		if err != nil {
			return nil, fmt.Errorf("parse agent hook snooze deadline: %w", err)
		}
		snooze.SnoozedUntil = until
		snoozes = append(snoozes, snooze)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list active agent hook snoozes: %w", err)
	}
	return snoozes, nil
}
```

Also populate `RepoName` in `SetAgentHookSnooze` and `ActiveAgentHookSnooze`
from the already-loaded `repo.Name`.

- [ ] **Step 4: Run storage tests and verify GREEN**

Run:

```bash
go test ./internal/storage -run 'AgentHookSnooze'
```

Expected: all snooze storage tests pass.

- [ ] **Step 5: Commit the storage slice**

Invoke the mandatory commit skill, stage only the two storage files, and commit
with subject:

```text
List active Agent Hook snoozes
```

---

### Task 2: Add active snoozes to daemon status

**Files:**

- Modify: `internal/storage/models.go`
- Modify: `internal/daemon/server.go`
- Modify: `cmd/roborev/status.go`
- Test: `internal/daemon/routes_test.go`
- Test: `cmd/roborev/status_test.go`
- Regenerate: `pkg/client/openapi.yaml`
- Regenerate: files under `pkg/client/generated/`

**Interfaces:**

- Consumes: `(*storage.DB).ListActiveAgentHookSnoozes(time.Time)` from Task 1.
- Produces: `storage.DaemonStatus.ActiveSnoozes []storage.AgentHookSnooze` as
  JSON field `active_snoozes`.
- Preserves: existing `GET /api/status` endpoint and all current fields.

- [ ] **Step 1: Write failing populated, empty, failure, and CLI JSON tests**

Add `TestHumaGetStatusIncludesActiveSnoozes` with an active snooze and decode
through a local response shape so this RED test does not depend on the
production `DaemonStatus` field existing:

```go
until := time.Now().Add(time.Hour).UTC()
_, err := db.SetAgentHookSnooze(
	repo.RootPath, repo.RootPath, "main", until,
)
require.NoError(t, err)

rr := serveHuma(t, srv, http.MethodGet, "/api/status", nil)
require.Equal(t, http.StatusOK, rr.Code)
var body struct {
	ActiveSnoozes []storage.AgentHookSnooze `json:"active_snoozes"`
}
require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
require.Len(t, body.ActiveSnoozes, 1)
assert.Equal(t, repo.Name, body.ActiveSnoozes[0].RepoName)
assert.Equal(t, repo.RootPath, body.ActiveSnoozes[0].RepoPath)
assert.Equal(t, repo.RootPath, body.ActiveSnoozes[0].WorktreePath)
assert.Equal(t, "main", body.ActiveSnoozes[0].Branch)
assert.Equal(t, until, body.ActiveSnoozes[0].SnoozedUntil)
```

Add `TestStatusCmdJSONIncludesActiveSnoozes`. Have its `OnStatus` hook encode a
`map[string]any` with the required ordinary status fields plus
`"active_snoozes": []storage.AgentHookSnooze{snooze}`. Decode CLI output into
a local nested shape and assert:

```go
var parsed struct {
	Daemon struct {
		ActiveSnoozes []storage.AgentHookSnooze `json:"active_snoozes"`
	} `json:"daemon"`
}
require.NoError(t, json.Unmarshal([]byte(output), &parsed))
require.Len(t, parsed.Daemon.ActiveSnoozes, 1)
assert.Equal(t, snooze, parsed.Daemon.ActiveSnoozes[0])
```

Also add the empty and failure route tests:

```go
func TestHumaGetStatusUsesEmptyActiveSnoozeArray(t *testing.T) {
	srv, _, _ := newTestServer(t)
	rr := serveHuma(t, srv, http.MethodGet, "/api/status", nil)
	require.Equal(t, http.StatusOK, rr.Code)

	var body struct {
		ActiveSnoozes json.RawMessage `json:"active_snoozes"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	assert.JSONEq(t, `[]`, string(body.ActiveSnoozes))
}

func TestHumaGetStatusFailsWhenActiveSnoozesCannotBeRead(t *testing.T) {
	srv, db, _ := newTestServer(t)
	_, err := db.Exec(`DROP TABLE agent_hook_snoozes`)
	require.NoError(t, err)

	rr := serveHuma(t, srv, http.MethodGet, "/api/status", nil)
	assert.Equal(t, http.StatusInternalServerError, rr.Code)
}
```

Add human and JSON CLI tests whose status hook returns HTTP 500. Assert that
both modes report `500 Internal Server Error` as unavailable and do not emit
zero-valued queue data.

- [ ] **Step 2: Run all new status tests and verify RED**

Run:

```bash
go test ./internal/daemon -run 'TestHumaGetStatus(Includes|UsesEmpty|FailsWhen)'
go test ./cmd/roborev -run '^TestStatusCmdJSONIncludesActiveSnoozes$'
```

Expected: populated and empty JSON lack `active_snoozes`, the query-failure
route returns 200, and the CLI drops the unknown daemon field.

- [ ] **Step 3: Populate the status response**

Add the field in `internal/storage/models.go`:

```go
ActiveSnoozes []AgentHookSnooze `json:"active_snoozes"`
```

In `humaGetStatus`, query before constructing the response and return the
existing Huma 500 form on failure:

```go
activeSnoozes, err := s.db.ListActiveAgentHookSnoozes(time.Now())
if err != nil {
	return nil, huma.Error500InternalServerError(
		fmt.Sprintf("list active agent hook snoozes: %v", err),
	)
}
```

Assign `ActiveSnoozes: activeSnoozes` in the `storage.DaemonStatus` literal.
The storage query returns a non-nil empty slice so JSON clients receive `[]`
when nothing is snoozed.

In `cmd/roborev/status.go`, reject the response before decoding it:

```go
if resp.StatusCode != http.StatusOK {
	return writeStatusUnavailable(
		fmt.Errorf("daemon returned %s", resp.Status),
	)
}
```

- [ ] **Step 4: Regenerate the public API artifacts**

Run:

```bash
make api-generate
```

Inspect the diff and confirm only the OpenAPI schema and generated client model
changes expected for `repo_name` and `active_snoozes` are present.

- [ ] **Step 5: Run focused API tests and verify GREEN**

Run:

```bash
go test ./internal/daemon ./pkg/client -run 'TestHumaGetStatus|TestClient'
go test ./cmd/roborev -run 'TestStatusCmdJSONIncludes(ActiveSnoozes|DaemonEndpoint)'
```

Expected: the status route and public client tests pass.

- [ ] **Step 6: Commit the daemon API slice**

Invoke the mandatory commit skill, stage the model, handler, test, OpenAPI, and
generated-client changes, and commit with subject:

```text
Expose active snoozes in daemon status
```

---

### Task 3: Render active snoozes in `roborev status`

**Files:**

- Modify: `cmd/roborev/status.go`
- Test: `cmd/roborev/status_test.go`

**Interfaces:**

- Consumes: `storage.DaemonStatus.ActiveSnoozes` from Task 2.
- Produces: an `Active Snoozes` terminal table with Repo, Worktree, Branch, and
  Until columns.
- Preserves: `statusJSONResult` nesting, so JSON records appear at
  `daemon.active_snoozes`.

- [ ] **Step 1: Write the failing populated human status test**

Add a mock status response containing this fixed snooze:

```go
until := time.Date(2026, 8, 10, 20, 30, 0, 0, time.UTC)
snooze := storage.AgentHookSnooze{
	RepoName:      "roborev",
	RepoPath:      "/src/roborev",
	WorktreePath:  "/worktrees/snooze-status",
	Branch:        "feature/snooze-status",
	SnoozedUntil:  until,
}
```

Execute `statusCmd()` against the mock and assert:

```go
assert.Contains(t, output, "Active Snoozes:")
assert.Contains(t, output, "roborev")
assert.Contains(t, output, "/worktrees/snooze-status")
assert.Contains(t, output, "feature/snooze-status")
assert.Contains(t, output, until.Local().Format("Jan 02 15:04 MST"))
```

Use the existing mock daemon's health and jobs fallbacks; do not introduce a
new test helper for this test. The JSON path was covered in Task 2's RED/GREEN
cycle.

- [ ] **Step 2: Run the CLI tests and verify RED**

Run:

```bash
go test ./cmd/roborev -run '^TestStatusCmd.*Snooze'
```

Expected: the human-output test fails because no `Active Snoozes` section is
printed.

- [ ] **Step 3: Add the minimal populated table**

After health output and before recent jobs, initially render the section and
rows without the length guard. This is the smallest change that makes the
populated test pass and leaves the empty-state behavior for its own RED cycle:

```go
fmt.Println("Active Snoozes:")
w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
fmt.Fprintln(w, "  Repo\tWorktree\tBranch\tUntil")
for _, snooze := range status.ActiveSnoozes {
	fmt.Fprintf(w, "  %s\t%s\t%s\t%s\n",
		snooze.RepoName,
		snooze.WorktreePath,
		snooze.Branch,
		snooze.SnoozedUntil.Local().Format("Jan 02 15:04 MST"),
	)
}
if err := w.Flush(); err != nil {
	return fmt.Errorf("flush active snoozes: %w", err)
}
fmt.Println()
```

Keep the implementation inline beside the other status sections; a one-use
formatter helper is unnecessary. Re-run the populated test and verify it
passes.

- [ ] **Step 4: Write the failing empty-inventory test**

Add a status test whose mock returns `ActiveSnoozes: []storage.AgentHookSnooze{}`
and assert:

```go
assert.NotContains(t, output, "Active Snoozes:")
```

Run:

```bash
go test ./cmd/roborev -run '^TestStatusCmdOmitsActiveSnoozesWhenEmpty$'
```

Expected: FAIL because the minimal implementation always prints the heading.

- [ ] **Step 5: Guard the section and verify GREEN**

Wrap the populated table from Step 3 in:

```go
if len(status.ActiveSnoozes) > 0 {
	fmt.Println("Active Snoozes:")
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "  Repo\tWorktree\tBranch\tUntil")
	for _, snooze := range status.ActiveSnoozes {
		fmt.Fprintf(w, "  %s\t%s\t%s\t%s\n",
			snooze.RepoName,
			snooze.WorktreePath,
			snooze.Branch,
			snooze.SnoozedUntil.Local().Format("Jan 02 15:04 MST"),
		)
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("flush active snoozes: %w", err)
	}
	fmt.Println()
}
```

Run all status tests:

Run:

```bash
go test ./cmd/roborev -run '^TestStatusCmd'
```

Expected: all status command tests pass.

- [ ] **Step 6: Commit the CLI slice**

Invoke the mandatory commit skill, stage `status.go` and `status_test.go`, and
commit with subject:

```text
Show active snoozes in status output
```

---

### Task 4: Show an exact-context snooze badge in the TUI

**Files:**

- Modify: `cmd/roborev/tui/tui.go`
- Modify: `cmd/roborev/tui/render_queue.go`
- Create: `cmd/roborev/tui/snooze_status_test.go`

**Interfaces:**

- Consumes: `model.status.ActiveSnoozes`, active repo/branch filters, and TUI
  startup checkout context.
- Produces: `func (m model) activeSnooze(now time.Time) *storage.AgentHookSnooze`.
- Produces: `model.cwdWorktreePath string`, the exact checkout root detected at
  startup.
- Produces: `func detectCwdRepoContext(ctx context.Context, path string) (repoRoot, repoIdentity, worktreePath, branch string)`.
- Produces: title badge `[SNOOZED until Jan 02 15:04]` for one exact active
  scope.

- [ ] **Step 1: Write the failing exact-match helper test**

Add a table-driven test whose hand-built model starts with one matching snooze:

```go
func TestTUIActiveSnoozeRequiresExactFilteredCheckout(t *testing.T) {
	now := time.Date(2026, 8, 10, 15, 0, 0, 0, time.UTC)
	until := now.Add(time.Hour)
	base := model{
		activeRepoFilter:   []string{"/repos/roborev"},
		activeBranchFilter: "feature/status",
		cwdRepoRoot:        "/repos/roborev",
		cwdWorktreePath:    "/worktrees/status",
		cwdBranch:          "feature/status",
		status: storage.DaemonStatus{ActiveSnoozes: []storage.AgentHookSnooze{{
			RepoPath:      "/repos/roborev",
			WorktreePath:  "/worktrees/status",
			Branch:        "feature/status",
			SnoozedUntil:  until,
		}},
	}

	tests := []struct {
		name   string
		mutate func(*model)
		want   bool
	}{
		{name: "exact scope", want: true},
		{name: "broad repo", mutate: func(m *model) { m.activeRepoFilter = nil }},
		{name: "different repo", mutate: func(m *model) {
			m.activeRepoFilter = []string{"/repos/other"}
		}},
		{name: "aggregate repos", mutate: func(m *model) {
			m.activeRepoFilter = []string{"/repos/roborev", "/repos/mirror"}
		}},
		{name: "broad branch", mutate: func(m *model) { m.activeBranchFilter = "" }},
		{name: "different branch", mutate: func(m *model) {
			m.activeBranchFilter = "main"
		}},
		{name: "sibling worktree", mutate: func(m *model) {
			m.cwdWorktreePath = "/worktrees/sibling"
		}},
		{name: "expired", mutate: func(m *model) {
			m.status.ActiveSnoozes[0].SnoozedUntil = now
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := base
			m.activeRepoFilter = append([]string(nil), base.activeRepoFilter...)
			m.status.ActiveSnoozes = append(
				[]storage.AgentHookSnooze(nil), base.status.ActiveSnoozes...,
			)
			if tt.mutate != nil {
				tt.mutate(&m)
			}
			assert.Equal(t, tt.want, m.activeSnooze(now) != nil)
		})
	}
}
```

The mutations name the bugs this test catches: removing any scope comparison or
expiry check makes a negative case incorrectly match.

- [ ] **Step 2: Run the helper test and verify RED**

Run:

```bash
go test ./cmd/roborev/tui -run '^TestTUIActiveSnoozeRequiresExactFilteredCheckout$'
```

Expected: compilation fails because `cwdWorktreePath` and `activeSnooze` do not
exist.

- [ ] **Step 3: Write the failing linked-worktree discovery test**

Use `internal/testutil.NewTestRepoWithCommit`, create a linked worktree with
`git worktree add -b feature/status`, then exercise a small production discovery
function directly so the test remains isolated from global config and live
daemon discovery:

```go
func TestDetectCwdRepoContextPreservesLinkedWorktree(t *testing.T) {
	repo := testutil.NewTestRepoWithCommit(t)
	worktree := filepath.Join(t.TempDir(), "status-worktree")
	repo.Run("worktree", "add", "-b", "feature/status", worktree)

	repoRoot, _, worktreePath, branch := detectCwdRepoContext(
		context.Background(), worktree,
	)
	assert.Equal(t, filepath.ToSlash(repo.Path()), repoRoot)
	assert.Equal(t, filepath.ToSlash(worktree), worktreePath)
	assert.Equal(t, "feature/status", branch)
}
```

Run:

```bash
go test ./cmd/roborev/tui -run '^TestDetectCwdRepoContextPreservesLinkedWorktree$'
```

Expected: compilation fails because `detectCwdRepoContext` does not exist.

- [ ] **Step 4: Implement normalized startup context and matching**

Extract startup discovery and normalize native Git paths to the forward-slash
form used by storage and daemon filters:

```go
func detectCwdRepoContext(
	ctx context.Context, path string,
) (repoRoot, repoIdentity, worktreePath, branch string) {
	worktreeRoot, err := gitrepo.Root(ctx, path)
	if err != nil || worktreeRoot == "" {
		return "", "", "", ""
	}
	worktreePath = filepath.ToSlash(filepath.Clean(worktreeRoot))
	mainRoot, err := gitrepo.MainRoot(ctx, worktreeRoot)
	if err != nil || mainRoot == "" {
		return "", "", "", ""
	}
	repoRoot = filepath.ToSlash(filepath.Clean(mainRoot))
	return repoRoot, config.ResolveRepoIdentity(repoRoot, nil), worktreePath,
		gitrepo.CurrentBranch(ctx, worktreeRoot)
}
```

Call `detectCwdRepoContext(ctx, ".")` from `newModel`, store all four results,
add `cwdWorktreePath` beside `cwdRepoRoot` in `model`, and normalize a non-empty
`opt.repoFilter` with `filepath.ToSlash(filepath.Clean(opt.repoFilter))` before
storing it in `activeRepoFilter`. Add this pure matcher in `render_queue.go`:

```go
func (m model) activeSnooze(now time.Time) *storage.AgentHookSnooze {
	if len(m.activeRepoFilter) != 1 ||
		m.activeRepoFilter[0] != m.cwdRepoRoot ||
		m.activeBranchFilter == "" ||
		m.activeBranchFilter != m.cwdBranch ||
		m.cwdWorktreePath == "" {
		return nil
	}
	for i := range m.status.ActiveSnoozes {
		snooze := &m.status.ActiveSnoozes[i]
		if snooze.RepoPath == m.cwdRepoRoot &&
			snooze.WorktreePath == m.cwdWorktreePath &&
			snooze.Branch == m.cwdBranch &&
			snooze.SnoozedUntil.After(now) {
			return snooze
		}
	}
	return nil
}
```

- [ ] **Step 5: Run the discovery and helper tests and verify GREEN**

Run:

```bash
go test ./cmd/roborev/tui -run 'TestDetectCwdRepoContext|TestTUIActiveSnooze'
```

Expected: all exact and negative scope cases pass.

- [ ] **Step 6: Write the failing title and compact-mode test**

Build a model with the matching fields above, set width `200`, render the queue,
and assert the badge contains `SNOOZED` and the local deadline formatted as
`Jan 02 15:04`. Use `until := time.Now().Add(time.Hour)` for this rendering
fixture because production rendering intentionally reads the real clock. Repeat
with height `10` to prove compact mode preserves the title badge. Then change
the active branch filter and assert the badge is gone.

```go
assert.Contains(t, out, "[SNOOZED until "+until.Local().Format("Jan 02 15:04")+"]")
assert.Contains(t, compact, "[SNOOZED")
assert.NotContains(t, differentBranch, "[SNOOZED")
```

- [ ] **Step 7: Run the rendering test and verify RED**

Run:

```bash
go test ./cmd/roborev/tui -run '^TestTUIQueueTitleShowsExactSnooze$'
```

Expected: the badge assertions fail because `renderQueueTitle` does not render
active snooze context.

- [ ] **Step 8: Render the badge in the non-optional title span**

After the existing global `[PAUSED]` badge logic, add:

```go
if snooze := m.activeSnooze(time.Now()); snooze != nil {
	label := "[SNOOZED until " +
		snooze.SnoozedUntil.Local().Format("Jan 02 15:04") + "]"
	app += " " + warningFlashStyle.Render(label)
}
```

The render-time expiry check means the existing display tick repaints away an
expired badge without adding synchronization or another polling path.

- [ ] **Step 9: Run TUI tests and verify GREEN**

Run:

```bash
go test ./cmd/roborev/tui -run 'Snooze|QueueHeader|QueueTitle'
```

Expected: snooze, pause badge, and title layout tests pass.

- [ ] **Step 10: Commit the TUI slice**

Invoke the mandatory commit skill, stage the TUI model, renderer, and tests,
and commit with subject:

```text
Mark snoozed checkout context in the TUI
```

---

### Task 5: Document and verify snooze visibility

**Files:**

- Modify: `docs/commands.md`
- Modify: `docs/agent-hook.md`
- Modify: `docs/changelog.md`

**Interfaces:**

- Documents: `roborev status` terminal section,
  `daemon.active_snoozes` JSON field, and contextual TUI badge behavior.
- Produces no new code interface.

- [ ] **Step 1: Update user-facing documentation**

In the status section of `docs/commands.md`, state that active snoozes are
listed with exact repo/worktree/branch scope and expiry, and that JSON exposes
them at `daemon.active_snoozes`.

In `docs/agent-hook.md` immediately after the snooze scope explanation, add that
`roborev status` lists all active scopes and that a TUI launched with automatic
filters from the exact snoozed checkout displays the title badge until filters
change or the snooze expires.

In the unreleased section of `docs/changelog.md`, add one concise bullet linking
to the Agent Hook snooze guide.

- [ ] **Step 2: Format and validate documentation**

Run:

```bash
make markdown
make markdown-ci
```

Expected: both commands exit successfully and only the intended documentation
is changed.

- [ ] **Step 3: Run all quality gates**

Run:

```bash
go test ./...
go build ./...
make lint-ci
prek run --all-files
```

Expected: every command exits successfully. Do not use `make install` or run a
branch daemon against the user's live data.

- [ ] **Step 4: Inspect the complete diff**

Run:

```bash
git status --short
git diff --stat origin/main
git diff origin/main
```

Confirm the code matches the approved spec, generated files contain only the
additive schema fields, no unrelated user changes are staged, and the
superpowers artifacts remain separate from product behavior.

- [ ] **Step 5: Commit documentation**

Invoke the mandatory commit skill, stage the three user-facing documentation
files, and commit with subject:

```text
Document active snooze visibility
```

- [ ] **Step 6: Finish the branch workflow**

Invoke `superpowers:verification-before-completion`, then
`superpowers:finishing-a-development-branch`. Follow repository instructions
for final status, integration, and handoff without changing branches or merging
a pull request.
