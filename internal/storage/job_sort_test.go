package storage

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedSortJobs creates six listed jobs whose column values give every sort
// column a distinct order with ties, plus two panel members of the last job.
// It returns the listed jobs' IDs in creation order.
func seedSortJobs(t *testing.T, db *DB, base time.Time) []int64 {
	t.Helper()
	alpha := createRepo(t, db, "/tmp/sort/alpha")
	beta := createRepo(t, db, "/tmp/sort/beta")
	at := func(d time.Duration) string { return base.Add(d).Format(time.RFC3339) }
	run := testUUID("sort-panel")

	type review struct {
		closed  int
		verdict any
		output  string
	}
	jobs := []struct {
		repo                *Repo
		branch, agent       string
		jobType, reviewType string
		panelRole           string
		status              string
		enqueued            string
		started, finished   any
		usage               any
		review              *review
	}{
		{
			beta, "main", "codex", JobTypeReview, "", "", "done", at(3 * time.Hour),
			at(0), at(100 * time.Second), `{"has_cost":true,"cost_usd":0.5}`, &review{1, 1, "No issues found."},
		},
		{
			alpha, "main", "claude-code", JobTypeReview, "security", "", "done", at(time.Hour),
			at(0), at(300 * time.Second), `{"has_cost":true,"cost_usd":0.1}`, &review{0, 0, "High: broken"},
		},
		// Legacy timestamp text without a zone, and a free-form job whose
		// verdict comes from its review text.
		{
			alpha, "dev", "codex", JobTypeFix, "", "", "done", base.Add(2 * time.Hour).Format(time.DateTime),
			at(0), at(100 * time.Second), nil, &review{0, nil, "No issues found."},
		},
		// A review document without a stored verdict has none.
		{
			beta, "", "codex", JobTypeReview, "", "", "running", at(4 * time.Hour),
			at(10*time.Hour - 50*time.Second), nil, `{"has_cost":false,"cost_usd":9}`, &review{0, nil, "No issues found."},
		},
		{
			alpha, "main", "gemini", JobTypeReview, "", "", "queued", at(4 * time.Hour),
			nil, nil, nil, nil,
		},
		// A panel synthesis row: its own run took 200s and cost 0.05.
		{
			beta, "main", "codex", JobTypeSynthesis, "", PanelRoleSynthesis, "done", at(6 * time.Hour),
			at(200 * time.Second), at(400 * time.Second), `{"has_cost":true,"cost_usd":0.05}`, &review{1, 0, "High: broken"},
		},
	}
	ids := make([]int64, 0, len(jobs))
	for i, spec := range jobs {
		sha := fmt.Sprintf("sort-sha%d", i)
		job := enqueueJob(t, db, spec.repo.ID, createCommit(t, db, spec.repo.ID, sha).ID, sha)
		_, err := db.Exec(`UPDATE review_jobs SET branch = ?, agent = ?, job_type = ?, review_type = ?,
			panel_role = NULLIF(?, ''), panel_run_uuid = CASE WHEN ? = '' THEN NULL ELSE ? END,
			status = ?, enqueued_at = ?, started_at = ?, finished_at = ?, token_usage = ? WHERE id = ?`,
			spec.branch, spec.agent, spec.jobType, spec.reviewType, spec.panelRole, spec.panelRole, run,
			spec.status, spec.enqueued, spec.started, spec.finished, spec.usage, job.ID)
		require.NoError(t, err)
		if spec.review != nil {
			_, err = db.Exec(`INSERT INTO reviews (job_id, agent, output, closed, verdict_bool) VALUES (?, ?, ?, ?, ?)`,
				job.ID, spec.agent, spec.review.output, spec.review.closed, spec.review.verdict)
			require.NoError(t, err)
		}
		ids = append(ids, job.ID)
	}
	// Members start before the synthesis row and add 0.20 to its cost.
	for i, usage := range []any{`{"has_cost":true,"cost_usd":0.2}`, nil} {
		sha := fmt.Sprintf("sort-member%d", i)
		member := enqueueJob(t, db, beta.ID, createCommit(t, db, beta.ID, sha).ID, sha)
		_, err := db.Exec(`UPDATE review_jobs SET panel_role = ?, panel_run_uuid = ?, status = 'done',
			started_at = ?, finished_at = ?, token_usage = ? WHERE id = ?`,
			PanelRoleMember, run, at(0), at(150*time.Second), usage, member.ID)
		require.NoError(t, err)
	}
	return ids
}

func TestListJobsSortOrderAndPaging(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ids := seedSortJobs(t, db, base)
	asOf := base.Add(10 * time.Hour)

	// Expected orders are indexes into the seeded jobs, worked out by hand
	// from the values in seedSortJobs.
	tests := []struct {
		column JobSortColumn
		asc    []int
		desc   []int
	}{
		{JobSortID, []int{0, 1, 2, 3, 4, 5}, []int{5, 4, 3, 2, 1, 0}},
		// (repo name, branch)
		{JobSortRepo, []int{2, 1, 4, 3, 0, 5}, []int{5, 0, 3, 4, 1, 2}},
		{JobSortAgent, []int{1, 0, 2, 3, 5, 4}, []int{4, 5, 3, 2, 0, 1}},
		// "default" < "fix \u00b7 default" < "panel" < "security"
		{JobSortReviewType, []int{0, 3, 4, 2, 5, 1}, []int{1, 5, 2, 4, 3, 0}},
		{JobSortStatus, []int{0, 1, 2, 5, 4, 3}, []int{3, 4, 5, 2, 1, 0}},
		// '' < F < P
		{JobSortVerdict, []int{3, 4, 1, 5, 0, 2}, []int{2, 0, 5, 1, 4, 3}},
		// no review (-1) < open < closed
		{JobSortClosed, []int{4, 1, 2, 3, 0, 5}, []int{5, 0, 3, 2, 1, 4}},
		// -1, 50 (running to asOf), 100, 100, 300, 400 (panel from member start)
		{JobSortElapsed, []int{4, 3, 0, 2, 1, 5}, []int{5, 1, 2, 0, 3, 4}},
		// -1 x3, 0.10, 0.25 (panel with member), 0.50
		{JobSortCost, []int{2, 3, 4, 1, 5, 0}, []int{0, 5, 1, 4, 3, 2}},
		{JobSortEnqueuedAt, []int{1, 2, 0, 3, 4, 5}, []int{5, 4, 3, 0, 2, 1}},
	}
	for _, tt := range tests {
		for _, ascending := range []bool{true, false} {
			order := tt.desc
			if ascending {
				order = tt.asc
			}
			want := make([]int64, len(order))
			for i, index := range order {
				want[i] = ids[index]
			}
			name := fmt.Sprintf("%s ascending=%t", tt.column, ascending)
			t.Run(name, func(t *testing.T) {
				sort := WithSort(JobSort{Column: tt.column, Ascending: ascending, AsOf: asOf})
				members := WithExcludePanelRole(PanelRoleMember)

				all, err := db.ListJobs("", "", 0, 0, sort, members)
				require.NoError(t, err)
				assert.Equal(t, want, jobIDs(all), "single listing")

				// Pages of two cross every tie group boundary.
				var paged []int64
				opts := []ListJobsOption{sort, members}
				for range len(want) {
					page, err := db.ListJobs("", "", 2, 0, opts...)
					require.NoError(t, err)
					if len(page) == 0 {
						break
					}
					paged = append(paged, jobIDs(page)...)
					last := page[len(page)-1]
					opts = []ListJobsOption{
						sort, members,
						WithSortAfter(JobSortPosition{Keys: last.SortKey, JobID: last.ID}),
					}
				}
				assert.Equal(t, want, paged, "paged listing")
			})
		}
	}
}

func TestListJobsVerdictSortKeyMatchesListedVerdict(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	seedSortJobs(t, db, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))

	jobs, err := db.ListJobs("", "", 0, 0, WithSort(JobSort{Column: JobSortVerdict}))
	require.NoError(t, err)
	require.NotEmpty(t, jobs)
	for _, job := range jobs {
		listed := ""
		if job.Verdict != nil {
			listed = *job.Verdict
		}
		assert.Equal(t, []any{listed}, job.SortKey, "job %d", job.ID)
	}
}

func TestListJobsRejectsSortPositionForAnotherColumn(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })

	// A repo position has two keys; an agent sort takes one.
	_, err := db.ListJobs("", "", 10, 0,
		WithSort(JobSort{Column: JobSortAgent}),
		WithSortAfter(JobSortPosition{Keys: []any{"alpha", "main"}, JobID: 1}))
	require.ErrorIs(t, err, ErrInvalidSortPosition)
}

func TestListJobsElapsedPagesKeepJobsThatChangeBetweenPages(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ids := seedSortJobs(t, db, base)
	asOf := base.Add(10 * time.Hour)
	sort := WithSort(JobSort{Column: JobSortElapsed, AsOf: asOf})
	members := WithExcludePanelRole(PanelRoleMember)

	first, err := db.ListJobs("", "", 2, 0, sort, members)
	require.NoError(t, err)
	assert.Equal(t, []int64{ids[5], ids[1]}, jobIDs(first))

	// After the first page, the running job (50s at asOf) finishes an hour
	// later, and the queued job starts and runs for two hours. Measured at
	// their real times, both would sort above the cursor and be skipped.
	_, err = db.Exec(`UPDATE review_jobs SET status = 'done', finished_at = ? WHERE id = ?`,
		asOf.Add(time.Hour).Format(time.RFC3339), ids[3])
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE review_jobs SET status = 'done', started_at = ?, finished_at = ? WHERE id = ?`,
		asOf.Add(time.Minute).Format(time.RFC3339), asOf.Add(2*time.Hour).Format(time.RFC3339), ids[4])
	require.NoError(t, err)

	last := first[len(first)-1]
	rest, err := db.ListJobs("", "", 0, 0, sort, members,
		WithSortAfter(JobSortPosition{Keys: last.SortKey, JobID: last.ID}))
	require.NoError(t, err)
	assert.Equal(t, []int64{ids[2], ids[0], ids[3], ids[4]}, jobIDs(rest))
}

func TestListJobsElapsedPagesKeepJobStartedInTheSameSecondAsReference(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	_, jobs := seedJobs(t, db, "/tmp/sort/same-second", 3)
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	asOf := base.Add(500 * time.Millisecond)
	_, err := db.Exec(`UPDATE review_jobs SET status = 'done', started_at = ?, finished_at = ? WHERE id = ?`,
		preciseTimestampAt(base.Add(-100*time.Second)), preciseTimestampAt(base), jobs[0].ID)
	require.NoError(t, err)
	sort := WithSort(JobSort{Column: JobSortElapsed, AsOf: asOf})

	// The finished job, then the two queued jobs by descending ID.
	first, err := db.ListJobs("", "", 2, 0, sort)
	require.NoError(t, err)
	assert.Equal(t, []int64{jobs[0].ID, jobs[2].ID}, jobIDs(first))

	// The last queued job starts 300ms after the reference time, in the same
	// second. As of the reference time it has not started.
	_, err = db.Exec(`UPDATE review_jobs SET status = 'running', started_at = ? WHERE id = ?`,
		preciseTimestampAt(asOf.Add(300*time.Millisecond)), jobs[1].ID)
	require.NoError(t, err)

	last := first[len(first)-1]
	rest, err := db.ListJobs("", "", 0, 0, sort,
		WithSortAfter(JobSortPosition{Keys: last.SortKey, JobID: last.ID}))
	require.NoError(t, err)
	assert.Equal(t, []int64{jobs[1].ID}, jobIDs(rest))
}

func TestListJobsElapsedPagesKeepJobCompletedInTheSameSecondAsReference(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	_, jobs := seedJobs(t, db, "/tmp/sort/complete-same-second", 2)
	asOf := time.Now()
	// As of asOf, the running job has run 10s and the done job ran 9.5s.
	_, err := db.Exec(`UPDATE review_jobs SET status = 'running', started_at = ? WHERE id = ?`,
		preciseTimestampAt(asOf.Add(-10*time.Second)), jobs[0].ID)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE review_jobs SET status = 'done', started_at = ?, finished_at = ? WHERE id = ?`,
		preciseTimestampAt(asOf.Add(-20*time.Second)), preciseTimestampAt(asOf.Add(-10500*time.Millisecond)), jobs[1].ID)
	require.NoError(t, err)
	sort := WithSort(JobSort{Column: JobSortElapsed, Ascending: true, AsOf: asOf})

	first, err := db.ListJobs("", "", 1, 0, sort)
	require.NoError(t, err)
	assert.Equal(t, []int64{jobs[1].ID}, jobIDs(first))

	// The running job completes just after asOf, usually in the same second.
	// A finish time cut to whole seconds would end it before asOf, drop its
	// elapsed time to the done job's 9s, and its lower ID would then sort it
	// before the cursor and off the next page.
	require.NoError(t, db.CompleteJob(jobs[0].ID, "test", `{"schema_version":2,"summary":"No issues found.","verdict":"pass","findings":[]}`))

	last := first[len(first)-1]
	rest, err := db.ListJobs("", "", 0, 0, sort,
		WithSortAfter(JobSortPosition{Keys: last.SortKey, JobID: last.ID}))
	require.NoError(t, err)
	assert.Equal(t, []int64{jobs[0].ID}, jobIDs(rest))
}
