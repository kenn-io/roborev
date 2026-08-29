# Web Review Failure and Load Performance Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the browser Review tab explain failed reviews and reduce the wait
for the initial review list.

**Architecture:** Project failed-job reasons through the authenticated browser
job allowlist, then render that reason before the lazy rich-review component.
Start authenticated list hydration concurrently with status polling, and add a
SQLite expression index that matches the existing first-page ordering.

**Tech Stack:** Go, SQLite through `modernc.org/sqlite`, Svelte 5, TypeScript,
Vitest, Testing Library, Bun, Testify.

## Global Constraints

- Expose `error` only for jobs whose status is `failed`.
- Keep browser session authentication and the existing job projection allowlist.
- Treat stored failure text as ordinary Svelte text, not HTML.
- Keep `wasEverAvailable` false until `/api/status` succeeds.
- Preserve list ordering, filters, counts, pagination, and cursor format.
- Do not redesign the jobs API or cache job statistics.
- Use no new dependencies.
- Do not run `roborev review`, install a development binary, or touch the live
  daemon database.

---

### Task 1: Deliver failed review reasons across the browser boundary

**Files:**

- Modify: `internal/daemon/browser_handler.go:173-278`
- Test: `internal/daemon/browser_handler_test.go:340-430`
- Modify: `web/src/lib/components/reviews/ReviewContent.svelte:1-105`
- Test: `web/src/lib/components/reviews/ReviewContent.test.ts:1-68`
- Create: `web/src/lib/components/reviews/ReviewContentLoaded.test.ts`
- Test: `web/tests/e2e/reviews.spec.ts:180-220`
- Modify: `docs/web-ui.md:38-62`

**Interfaces:**

- Consumes: `storage.ReviewJob.Error string` and `storage.JobStatusFailed`.
- Produces: `browserReviewJob.Error string` serialized as optional JSON
  `error`; a `Review failed` UI state containing the selected job's reason.

- [ ] **Step 1: Add the failing authenticated projection test**

Add `TestBrowserHandlerProjectsFailureReasonOnlyForFailedJobs`. Its core handler
must return one failed job with `error: "agent process exited with status 1"`
and one done job with `error: "stale error"`. Send the response through
`authenticatedBrowserRequest`, decode `Jobs []map[string]any`, and assert:

```go
require.Equal(t, http.StatusOK, recorder.Code)
require.Len(t, response.Jobs, 2)
assert.Equal(t, reason, response.Jobs[0]["error"])
_, found := response.Jobs[1]["error"]
assert.False(t, found)
```

- [ ] **Step 2: Run the projection test and verify the field is missing**

```bash
go test ./internal/daemon -run TestBrowserHandlerProjectsFailureReasonOnlyForFailedJobs -count=1
```

Expected: FAIL because the failed job map has no `error` key.

- [ ] **Step 3: Add the minimal failed-only browser projection**

Add this field to `browserReviewJob`:

```go
Error string `json:"error,omitempty"`
```

Compute the projected value before the struct literal:

```go
errorMessage := ""
if job.Status == storage.JobStatusFailed {
	errorMessage = job.Error
}
```

Set `Error: errorMessage` in `projectBrowserReviewJob`. Do not project the field
for other statuses.

- [ ] **Step 4: Verify the browser boundary and existing omission contract**

```bash
go test ./internal/daemon -run 'TestBrowserHandler(ProjectsFailureReasonOnlyForFailedJobs|OmitsInternalJobMetadata)' -count=1
```

Expected: PASS. The failed reason crosses an authenticated request and the
existing internal-metadata sentinel remains absent.

- [ ] **Step 5: Add failing component tests**

Extend the hoisted state in `ReviewContent.test.ts` with `error`, include it in
`getSelectedJob`, and reset it after each test. Replace the old terminal-job
test with these expectations:

```ts
state.output = "";
state.notFound = true;
state.status = "failed";
state.error = "agent process exited with status 1";
render(ReviewContent);
expect(screen.getByRole("alert")).toHaveTextContent("Review failed");
expect(screen.getByRole("alert")).toHaveTextContent(state.error);
expect(screen.queryByText("No review output available.")).toBeNull();
```

Add a second test with `state.error = ""` that expects
`No failure reason was recorded.`. Create `ReviewContentLoaded.test.ts` without
mocking `@kenn-io/roborev-ui/review-content`; mock only the store, render a
failed job, wait for the lazy import to settle, and assert the same alert and
reason remain visible.

- [ ] **Step 6: Run the component tests and verify the failed state is absent**

```bash
bun run --cwd web test -- ReviewContent.test.ts ReviewContentLoaded.test.ts
```

Expected: FAIL because the wrapper still renders its generic empty state.

- [ ] **Step 7: Render failure before the rich component**

Add these derived values:

```ts
const selectedJob = $derived(stores.roborevReview?.getSelectedJob());
const failed = $derived(selectedJob?.status === "failed");
const failureReason = $derived(
  selectedJob?.error?.trim() || "No failure reason was recorded.",
);
```

Order markup as loading, failed, rich renderer, renderer load error, pending,
rendering, and generic empty. Use this failed branch:

```svelte
{:else if failed}
  <div class="review-failure" role="alert">
    <h3>Review failed</h3>
    <pre>{failureReason}</pre>
  </div>
```

Style the `pre` with `white-space: pre-wrap`, `overflow-wrap: anywhere`, and the
existing monospace and inset-background tokens. Do not use `{@html}`.

- [ ] **Step 8: Document, verify, and commit the behavior**

Add after the drawer feature list in `docs/web-ui.md`:

```markdown
When a review job fails, the **Review** tab shows the recorded agent or process
failure so you can see why no review output was produced.
```

Add this end-to-end assertion against the seeded failed job:

```ts
test("shows why a review failed", async ({ page }) => {
  await openReview(page, 48);

  await expect(
    page.getByRole("heading", { name: "Review failed" }),
  ).toBeVisible();
  await expect(
    page.getByText("fixture agent exited before producing a review"),
  ).toBeVisible();
});
```

Run:

```bash
bun run --cwd web test -- ReviewContent.test.ts ReviewContentLoaded.test.ts
go test ./internal/daemon -run 'TestBrowserHandler(ProjectsFailureReasonOnlyForFailedJobs|OmitsInternalJobMetadata)' -count=1
make markdown-ci
```

Expected: all commands PASS. Commit the six code/test files and
`docs/web-ui.md` with subject `fix(web): explain failed reviews`.

### Task 2: Start the authenticated review list without waiting for status

**Files:**

- Modify: `web/src/lib/stores/roborev/daemon.svelte.ts:10-24`
- Modify: `web/src/lib/stores/composition.svelte.ts:19-40`
- Modify: `web/src/lib/components/AppShell.svelte:25-39`
- Test: `web/src/App.test.ts:35-150`

**Interfaces:**

- Produces: `DaemonStoreOptions.initiallyAvailable?: boolean` and
  `ReviewStoreOptions.daemonInitiallyAvailable?: boolean`.
- Consumes: successful browser session bootstrap before `AppShell` mounts.

- [ ] **Step 1: Add a failing startup-overlap test**

Add `loads review rows without waiting for initial daemon status` to
`App.test.ts`. Use a pending promise for `/api/status`, collect request paths,
and return the existing empty jobs and open event-stream fixtures for their
routes:

```ts
let resolveStatus: ((response: Response) => void) | undefined;
const statusResponse = new Promise<Response>((resolve) => {
  resolveStatus = resolve;
});
const paths: string[] = [];
vi.stubGlobal(
  "fetch",
  vi.fn((input: RequestInfo | URL) => {
    const url = new URL(
      input instanceof Request ? input.url : input,
      location.origin,
    );
    paths.push(url.pathname);
    if (url.pathname === "/api/ui/session/bootstrap") {
      return Promise.resolve(response(200, credentials));
    }
    if (url.pathname === "/api/status") return statusResponse;
    if (url.pathname === "/api/jobs") {
      return Promise.resolve(
        response(200, {
          jobs: [],
          has_more: false,
          stats: { done: 0, closed: 0, open: 0 },
        }),
      );
    }
    if (url.pathname === "/api/stream/events") {
      return Promise.resolve(
        new Response(new ReadableStream({ start() {} }), { status: 200 }),
      );
    }
    return Promise.resolve(response(404));
  }),
);
render(App);
await vi.waitFor(() => expect(paths).toContain("/api/jobs"));
expect(paths).toContain("/api/status");
resolveStatus?.(response(200, status));
```

- [ ] **Step 2: Run the overlap test and verify jobs remain gated**

```bash
bun run --cwd web test -- App.test.ts -t "loads review rows without waiting for initial daemon status"
```

Expected: FAIL because `/api/jobs` is not requested while status is pending.

- [ ] **Step 3: Add provisional availability plumbing**

Add the option and initialize only `available` from it:

```ts
export interface DaemonStoreOptions {
  client: RoborevClient;
  runtime: AppRuntime;
  initiallyAvailable?: boolean;
}

let available = $state(opts.initiallyAvailable ?? false);
let wasEverAvailable = $state(false);
```

Add `daemonInitiallyAvailable?: boolean` to `ReviewStoreOptions`, pass it as
`initiallyAvailable` to `createDaemonStore`, and set
`daemonInitiallyAvailable: true` in the `AppShell` call to
`createReviewStores`. Do not initialize `wasEverAvailable` from the option.

- [ ] **Step 4: Verify startup and daemon failure behavior**

```bash
bun run --cwd web test -- App.test.ts daemon.svelte.test.ts composition.svelte.test.ts
```

Expected: PASS, including the existing transition that clears availability
after a failed status request.

- [ ] **Step 5: Commit the startup change**

Stage `web/src/lib/stores/roborev/daemon.svelte.ts`,
`web/src/lib/stores/composition.svelte.ts`,
`web/src/lib/components/AppShell.svelte`, and `web/src/App.test.ts`. Commit with
subject `perf(web): overlap initial review requests`.

### Task 3: Index the existing first-page review ordering

**Files:**

- Modify: `internal/storage/db.go:1298-1330`
- Test: `internal/storage/db_migration_test.go:293-390`
- Test: `internal/storage/db_filter_test.go:1-40`

**Interfaces:**

- Produces: SQLite index `idx_review_jobs_enqueued_position` over normalized
  `enqueued_at` descending and `id` descending.
- Consumes: `sqliteNormalizedTimestampExpr("enqueued_at")` and the unchanged
  `ListJobs` ordering.

- [ ] **Step 1: Confirm this is a new forward migration**

Run `git fetch --tags origin`, compare `internal/storage/db.go` and schema files
with `origin/main`, inspect file history on `origin/main` and tags, and list
`origin/release/*`. Expected: this branch has no prior schema change. Existing
history is immutable, so add one idempotent forward step after all legacy table
rebuilds.

- [ ] **Step 2: Add failing fresh/legacy migration coverage**

Add this helper and call it from a fresh-database test and at the end of
`TestMigrationFromOldSchema`:

```go
func requireJobListPositionIndex(t *testing.T, db *DB) {
	t.Helper()
	var count int
	require.NoError(t, db.QueryRow(`
		SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'index' AND name = 'idx_review_jobs_enqueued_position'
	`).Scan(&count))
	assert.Equal(t, 1, count)
}
```

- [ ] **Step 3: Add a failing first-page query-plan test**

In `db_filter_test.go`, open a test database and run `EXPLAIN QUERY PLAN` for
the actual join, default browser exclusions, and ordering:

```go
rows, err := db.Query(`EXPLAIN QUERY PLAN
	SELECT j.id
	FROM review_jobs j
	JOIN repos r ON r.id = j.repo_id
	LEFT JOIN reviews rv ON rv.job_id = j.id
	WHERE NOT (COALESCE(j.source, '') = 'auto_design'
		AND (j.job_type = ? OR j.status = ?))
		AND COALESCE(j.panel_role, '') != ?
	ORDER BY `+sqliteNormalizedTimestampExpr("j.enqueued_at")+` DESC, j.id DESC
	LIMIT 51`, JobTypeClassify, JobStatusSkipped, PanelRoleMember)
```

Scan the fourth plan column into a `[]string`, join it with newlines, require
`USING INDEX idx_review_jobs_enqueued_position`, and reject
`USE TEMP B-TREE FOR ORDER BY`.

- [ ] **Step 4: Run the storage tests and verify the index is absent**

```bash
go test ./internal/storage -run 'Test(OpenCreatesJobListPositionIndex|MigrationFromOldSchema|ListJobsUsesEnqueuedPositionIndex)' -count=1
```

Expected: FAIL because the named index does not exist and the query plan uses a
temporary ordering B-tree.

- [ ] **Step 5: Create the index after legacy rebuilds**

Add this forward step beside the post-rebuild panel and session indexes:

```go
jobListPositionExpr := sqliteNormalizedTimestampExpr("enqueued_at")
if _, err = db.Exec(`CREATE INDEX IF NOT EXISTS idx_review_jobs_enqueued_position
	ON review_jobs (` + jobListPositionExpr + ` DESC, id DESC)`); err != nil {
	return fmt.Errorf("create idx_review_jobs_enqueued_position: %w", err)
}
```

Do not change `ListJobs`, cursor encoding, or PostgreSQL schemas; this index
serves the daemon's local SQLite list path.

- [ ] **Step 6: Verify and commit the index**

Run:

```bash
go test ./internal/storage -run 'Test(OpenCreatesJobListPositionIndex|MigrationFromOldSchema|ListJobsUsesEnqueuedPositionIndex|ListJobs)' -count=1
```

Expected: PASS with the named index and no temporary order B-tree. Stage the
three storage files and commit with subject
`perf(storage): index review list ordering`.

### Task 4: Verify the combined change and prepare one pull request

**Files:**

- Delete before PR:
  `docs/superpowers/specs/2026-08-29-web-review-failure-and-load-performance-design.md`
- Delete before PR:
  `docs/superpowers/plans/2026-08-29-web-review-failure-and-load-performance.md`

**Interfaces:**

- Consumes: the three independently committed implementation units above.
- Produces: one pushed branch and one GitHub pull request that closes issue
  `#1107`.

- [ ] **Step 1: Run focused checks**

```bash
go test ./internal/daemon ./internal/storage
bun run web:check
bun run web:test
make markdown-ci
```

Expected: all commands PASS.

- [ ] **Step 2: Run repository quality gates**

```bash
go test ./...
go build ./...
make lint-ci
prek run --all-files
```

Expected: all commands PASS. Repository test fixtures isolate Git and runtime
state; none of these commands launches the branch daemon against live data.

- [ ] **Step 3: Build and inspect the user-visible behavior**

```bash
bun run web:build
bun run web:test:e2e
git diff origin/main...HEAD --stat
git diff origin/main...HEAD
```

Expected: the end-to-end failed-review fixture shows `Review failed` and its
synthetic reason. The diff contains only the issue fix, performance changes,
tests, and `docs/web-ui.md`.

- [ ] **Step 4: Remove working documents and commit the cleanup**

Delete the two `docs/superpowers` files listed above with `apply_patch`. Stage
both deletions and commit with subject `chore: remove web UI working documents`.

- [ ] **Step 5: Scrub outgoing public content**

Use the configured private-terms denylist to scan
`git diff origin/main...HEAD`, unpushed commit messages, `docs/web-ui.md`, test
fixtures, and the proposed PR title/body. Also check for absolute home paths,
private hostnames, credentials, and non-public email addresses. Expected: zero
hits.

- [ ] **Step 6: Rebase, push, and open one PR**

```bash
git pull --rebase
git push -u origin kenn-forge/issue-1107-when-agent-fails-the-web-ui-shows-no-review-output
gh pr create --repo kenn-io/roborev --base main --head kenn-forge/issue-1107-when-agent-fails-the-web-ui-shows-no-review-output --title "fix(web): explain failed reviews and load the queue faster" --body "Closes #1107."
git status --short --branch
```

Expected: push succeeds, the PR is open, and status reports the branch is up to
date with its origin tracking branch.
