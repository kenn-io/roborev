package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListSearchDocumentsSeeksPastCursor(t *testing.T) {
	t.Parallel()
	// A fresh database has no ANALYZE statistics to guide join ordering.
	db := openTestDB(t)
	defer db.Close()

	plan := queryPlan(t, db, listSearchDocumentsQuery, 25, 50)
	assert.Contains(t, plan, "SEARCH rv USING INTEGER PRIMARY KEY (rowid>?)")
	assert.NotContains(t, plan, "TEMP B-TREE FOR ORDER BY")
}

type searchFeedFixture struct {
	jobType      string
	status       JobStatus
	gitRef       string
	output       string
	structured   string
	panelRole    string
	panelRunUUID string
	legacy       bool
	noCommit     bool
}

func TestSearchEligibilityAndStablePaging(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	defer db.Close()

	repoID, commitID := seedSearchFeedBase(t, db)
	fixtures := []searchFeedFixture{
		{jobType: JobTypeReview, status: JobStatusDone, output: "ordinary"},
		{jobType: JobTypeRange, status: JobStatusApplied, gitRef: "base..head", output: "range", noCommit: true},
		{jobType: JobTypeDirty, status: JobStatusRebased, gitRef: "dirty", output: "dirty"},
		{jobType: JobTypeCompact, status: JobStatusDone, gitRef: "compact", output: "compact", noCommit: true},
		{jobType: JobTypeReview, status: JobStatusDone, output: "panel member", panelRole: PanelRoleMember, panelRunUUID: "00000000-0000-4000-8000-000000000090"},
		{
			jobType: JobTypeSynthesis, status: JobStatusDone,
			structured:   `{"schema_version":2,"summary":"synthesis","verdict":"pass","findings":[],"source_labels":["member"]}`,
			panelRole:    PanelRoleSynthesis,
			panelRunUUID: "00000000-0000-4000-8000-000000000090",
		},
		{jobType: "", status: JobStatusDone, output: "legacy", legacy: true},
		{jobType: JobTypeTask, status: JobStatusDone, gitRef: "task", output: "task", noCommit: true},
		{jobType: JobTypeInsights, status: JobStatusDone, gitRef: "insights", output: "insights", noCommit: true},
		{jobType: JobTypeFix, status: JobStatusDone, output: "fix"},
		{jobType: JobTypeClassify, status: JobStatusDone, output: "classifier"},
		{jobType: JobTypeReview, status: JobStatusFailed, output: "failed"},
		{jobType: JobTypeReview, status: JobStatusQueued, output: "queued"},
		{jobType: JobTypeReview, status: JobStatusDone, output: ""},
	}
	for i, fixture := range fixtures {
		reviewID := seedSearchFeedReview(t, db, repoID, commitID, i+1, fixture)
		var jobID int64
		require.NoError(t, db.QueryRowContext(t.Context(), `SELECT job_id FROM reviews WHERE id = ?`, reviewID).Scan(&jobID))
		source, err := db.GetSearchDocumentForJob(t.Context(), jobID)
		require.NoError(t, err)
		if i < 7 {
			require.NotNil(t, source)
			assert.Equal(t, fixture.output, source.Output)
		} else {
			assert.Nil(t, source)
		}
	}

	first, err := db.ListSearchDocuments(t.Context(), 0, 3)
	require.NoError(t, err)
	require.Len(t, first, 3)
	second, err := db.ListSearchDocuments(t.Context(), first[len(first)-1].ReviewID, 10)
	require.NoError(t, err)

	all := append(first, second...)
	require.Len(t, all, 7)
	assert.Equal(t, []string{"ordinary", "range", "dirty", "compact", "panel member", "", "legacy"}, searchFeedOutputs(all))
	for i := 1; i < len(all); i++ {
		assert.Greater(t, all[i].ReviewID, all[i-1].ReviewID)
	}
	assert.Equal(t, PanelRoleMember, all[4].PanelRole)
	assert.Equal(t, PanelRoleSynthesis, all[5].PanelRole)
	assert.Equal(t, all[4].PanelRunUUID, all[5].PanelRunUUID)
	assert.Equal(t, "synthesis", all[5].StructuredOutput["summary"])

	legacyID := seedSearchFeedReview(t, db, repoID, commitID, 100, searchFeedFixture{
		status: JobStatusDone, gitRef: "main", output: "legacy diff", legacy: true, noCommit: true,
	})
	var legacyJobID int64
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT job_id FROM reviews WHERE id = ?`, legacyID).Scan(&legacyJobID))
	_, err = db.ExecContext(t.Context(), `INSERT INTO job_content(job_id, diff_content) VALUES (?, 'diff')`, legacyJobID)
	require.NoError(t, err)
	legacy, err := db.GetSearchDocumentForJob(t.Context(), legacyJobID)
	require.NoError(t, err)
	require.NotNil(t, legacy)
	_, err = db.ExecContext(t.Context(), `UPDATE job_content SET diff_content = NULL WHERE job_id = ?`, legacyJobID)
	require.NoError(t, err)
	legacy, err = db.GetSearchDocumentForJob(t.Context(), legacyJobID)
	require.NoError(t, err)
	assert.Nil(t, legacy)
}

func TestSearchFeedIncludesGoalReview(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	defer db.Close()

	repoID, commitID := seedSearchFeedBase(t, db)
	reviewID := seedSearchFeedReview(t, db, repoID, commitID, 1, searchFeedFixture{
		jobType:  JobTypeGoalReview,
		status:   JobStatusDone,
		gitRef:   "goal-review",
		output:   "intent review",
		noCommit: true,
	})
	var jobID int64
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT job_id FROM reviews WHERE id = ?`, reviewID).Scan(&jobID))

	byJob, err := db.GetSearchDocumentForJob(t.Context(), jobID)
	require.NoError(t, err)
	require.NotNil(t, byJob)
	assert.Equal(t, "intent review", byJob.Output)

	listed, err := db.ListSearchDocuments(t.Context(), 0, 10)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	assert.Equal(t, reviewID, listed[0].ReviewID)

	key := SearchDocumentKey(byJob.ReviewID, byJob.ReviewUUID)
	found, err := db.GetSearchReviews(t.Context(), []string{key})
	require.NoError(t, err)
	require.Contains(t, found, key)
	assert.Equal(t, reviewID, found[key].ReviewID)
}

func TestSearchFeedLimitSkipsSemanticallyEmptyStructuredRows(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	defer db.Close()

	repoID, commitID := seedSearchFeedBase(t, db)
	firstID := seedSearchFeedReview(t, db, repoID, commitID, 1, searchFeedFixture{
		jobType: JobTypeReview,
		status:  JobStatusDone,
		output:  "first eligible review",
	})
	seedSearchFeedReview(t, db, repoID, commitID, 2, searchFeedFixture{
		jobType:    JobTypeReview,
		status:     JobStatusDone,
		structured: `{}`,
	})
	laterID := seedSearchFeedReview(t, db, repoID, commitID, 3, searchFeedFixture{
		jobType: JobTypeReview,
		status:  JobStatusDone,
		output:  "later eligible review",
	})

	firstPage, err := db.ListSearchDocuments(t.Context(), 0, 1)
	require.NoError(t, err)
	require.Len(t, firstPage, 1)
	assert.Equal(t, firstID, firstPage[0].ReviewID)

	secondPage, err := db.ListSearchDocuments(t.Context(), firstPage[0].ReviewID, 1)
	require.NoError(t, err)
	require.Len(t, secondPage, 1)
	assert.Equal(t, laterID, secondPage[0].ReviewID)
	assert.Equal(t, "later eligible review", secondPage[0].Output)
}

func TestSearchFeedUsesLegacyCommentTargetAndStableResponseOrdering(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	defer db.Close()

	repoID, commitID := seedSearchFeedBase(t, db)
	reviewID := seedSearchFeedReview(t, db, repoID, commitID, 1, searchFeedFixture{
		jobType: JobTypeDirty,
		status:  JobStatusDone,
		gitRef:  "dirty",
		output:  "dirty review",
	})
	var jobID int64
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT job_id FROM reviews WHERE id = ?`, reviewID).Scan(&jobID))

	_, err := db.ExecContext(t.Context(), `
		INSERT INTO responses (id, commit_id, responder, response, uuid, created_at)
		VALUES (90, ?, 'legacy', 'must stay out', '00000000-0000-4000-8000-000000000090', '2026-09-15T10:00:00Z')`, commitID)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `
		INSERT INTO responses (id, job_id, responder, response, uuid, created_at)
		VALUES
			(13, ?, 'third', 'third response', '00000000-0000-4000-8000-000000000013', '2026-09-15T12:00:00Z'),
			(11, ?, 'first', 'first response', '00000000-0000-4000-8000-000000000011', '2026-09-15T12:00:00Z'),
			(12, ?, 'second', 'second response', '00000000-0000-4000-8000-000000000012', '2026-09-15T12:00:00Z')`, jobID, jobID, jobID)
	require.NoError(t, err)

	source, err := feedDocument(db, "00000000-0000-4000-8000-000000000101")
	require.NoError(t, err)
	require.Len(t, source.Responses, 3)
	for _, response := range source.Responses {
		require.NotNil(t, response.UUID)
	}
	assert.Equal(t, []string{"first response", "second response", "third response"}, []string{
		source.Responses[0].Response,
		source.Responses[1].Response,
		source.Responses[2].Response,
	})
	for _, response := range source.Responses {
		assert.NotEqual(t, "must stay out", response.Response)
	}

	seedSearchFeedReview(t, db, repoID, commitID, 2, searchFeedFixture{
		jobType: JobTypeReview,
		status:  JobStatusDone,
		output:  "ordinary review",
	})
	ordinary, err := feedDocument(db, "00000000-0000-4000-8000-000000000102")
	require.NoError(t, err)
	require.Len(t, ordinary.Responses, 1)
	require.NotNil(t, ordinary.Responses[0].UUID)
	assert.Equal(t, "must stay out", ordinary.Responses[0].Response)

	// Incremental refreshes must attach the same complete, ordered comments.
	got, err := db.GetSearchDocumentForJob(t.Context(), jobID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, source.Responses, got.Responses)
	got, err = db.GetSearchDocumentForJob(t.Context(), ordinary.JobID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, ordinary.Responses, got.Responses)

	_, err = db.ExecContext(t.Context(), `DELETE FROM reviews WHERE job_id = ?`, jobID)
	require.NoError(t, err)
	got, err = db.GetSearchDocumentForJob(t.Context(), jobID)
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestSearchFeedSelectsOnlyAllowlistedSourceColumns(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	defer db.Close()

	repoID, commitID := seedSearchFeedBase(t, db)
	reviewID := seedSearchFeedReview(t, db, repoID, commitID, 1, searchFeedFixture{
		jobType: JobTypeReview,
		status:  JobStatusDone,
		output:  "safe review output",
	})
	var jobID int64
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT job_id FROM reviews WHERE id = ?`, reviewID).Scan(&jobID))
	_, err := db.ExecContext(t.Context(), `UPDATE review_jobs SET
		command_line = 'excluded-command', token_usage = 'excluded-token-data',
		worktree_path = 'excluded-worktree-path', error = 'excluded-log'
		WHERE id = ?`, jobID)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `INSERT INTO job_content (job_id, prompt, diff_content, patch)
		VALUES (?, zstd_compress('excluded-prompt'), zstd_compress('excluded-diff'), zstd_compress('excluded-patch'))`, jobID)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `UPDATE repos SET root_path = 'excluded-repository-path', identity = 'excluded-remote-identity' WHERE id = ?`, repoID)
	require.NoError(t, err)

	source, err := feedDocument(db, "00000000-0000-4000-8000-000000000101")
	require.NoError(t, err)
	encoded, err := json.Marshal(source)
	require.NoError(t, err)
	for _, excluded := range []string{
		"excluded-prompt", "excluded-diff", "excluded-patch", "excluded-command",
		"excluded-token-data", "excluded-worktree-path", "excluded-log",
		"excluded-repository-path", "excluded-remote-identity",
	} {
		assert.NotContains(t, string(encoded), excluded)
	}
}

func TestSearchDocumentLookupSupportsUUIDAndLegacyLocalKey(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	defer db.Close()

	repoID, commitID := seedSearchFeedBase(t, db)
	modernID := seedSearchFeedReview(t, db, repoID, commitID, 1, searchFeedFixture{jobType: JobTypeReview, status: JobStatusDone, output: "modern"})
	legacyID := seedSearchFeedReview(t, db, repoID, commitID, 2, searchFeedFixture{jobType: "", status: JobStatusDone, output: "legacy", legacy: true})

	legacyKey := fmt.Sprintf("local:%d", legacyID)
	found, err := db.GetSearchReviews(t.Context(), []string{
		"00000000-0000-4000-8000-000000000101", legacyKey, "missing", "local:999999",
	})
	require.NoError(t, err)
	require.Len(t, found, 2)
	assert.Equal(t, modernID, found["00000000-0000-4000-8000-000000000101"].ReviewID)
	assert.Equal(t, legacyID, found[legacyKey].ReviewID)
}

// feedDocument returns the search feed's source for one document key.
func feedDocument(db *DB, key string) (*SearchReviewSource, error) {
	sources, err := db.ListSearchDocuments(context.Background(), 0, 1000)
	if err != nil {
		return nil, err
	}
	for i := range sources {
		if SearchDocumentKey(sources[i].ReviewID, sources[i].ReviewUUID) == key {
			return &sources[i], nil
		}
	}
	return nil, sql.ErrNoRows
}

func seedSearchFeedBase(t *testing.T, db *DB) (int64, int64) {
	t.Helper()
	repoResult, err := db.ExecContext(t.Context(), `INSERT INTO repos (root_path, name, identity) VALUES ('/synthetic/widgets', 'widgets', 'https://example.invalid/widgets.git')`)
	require.NoError(t, err)
	repoID, err := repoResult.LastInsertId()
	require.NoError(t, err)
	commitResult, err := db.ExecContext(t.Context(), `INSERT INTO commits (repo_id, sha, author, subject, timestamp)
		VALUES (?, 'abc1234567890abcdef1234567890abcdef12345', 'Example Author', 'Improve retry persistence', '2026-09-15T09:00:00Z')`, repoID)
	require.NoError(t, err)
	commitID, err := commitResult.LastInsertId()
	require.NoError(t, err)
	return repoID, commitID
}

func seedSearchFeedReview(t *testing.T, db *DB, repoID, commitID int64, sequence int, fixture searchFeedFixture) int64 {
	t.Helper()
	gitRef := fixture.gitRef
	if gitRef == "" {
		gitRef = "abc1234567890abcdef1234567890abcdef12345"
	}
	var selectedCommit any = commitID
	if fixture.noCommit {
		selectedCommit = nil
	}
	jobUUID := fmt.Sprintf("00000000-0000-4000-8000-%012d", sequence)
	jobResult, err := db.ExecContext(t.Context(), `
		INSERT INTO review_jobs
			(repo_id, commit_id, git_ref, branch, agent, status, enqueued_at, finished_at,
			 job_type, review_type, uuid, panel_role, panel_run_uuid)
		VALUES (?, ?, ?, 'feature/search', 'codex', ?, '2026-09-15T10:00:00Z',
			'2026-09-15T11:00:00Z', ?, 'security', ?, ?, NULLIF(?, ''))`,
		repoID, selectedCommit, gitRef, fixture.status, fixture.jobType, jobUUID,
		fixture.panelRole, fixture.panelRunUUID)
	require.NoError(t, err)
	jobID, err := jobResult.LastInsertId()
	require.NoError(t, err)

	var reviewUUID any = fmt.Sprintf("00000000-0000-4000-8000-%012d", 100+sequence)
	if fixture.legacy {
		reviewUUID = nil
	}
	var structured any
	if fixture.structured != "" {
		structured = fixture.structured
	}
	reviewResult, err := db.ExecContext(t.Context(), `
		INSERT INTO reviews (job_id, agent, output, structured_output, created_at, closed, uuid)
		VALUES (?, 'codex', ?, ?, '2026-09-15T11:00:00Z', 0, ?)`,
		jobID, fixture.output, structured, reviewUUID)
	require.NoError(t, err)
	reviewID, err := reviewResult.LastInsertId()
	require.NoError(t, err)
	return reviewID
}

func searchFeedOutputs(sources []SearchReviewSource) []string {
	outputs := make([]string, len(sources))
	for i := range sources {
		outputs[i] = sources[i].Output
	}
	return outputs
}

func TestSearchChangesLogsEveryFeedSource(t *testing.T) {
	t.Parallel()
	type changeCase struct {
		name  string
		sql   string
		want  []int64
		setup string
	}
	cases := []changeCase{
		{"review insert", `INSERT INTO reviews (job_id, agent, output) VALUES (2, 'test', 'second')`, []int64{2}, `DELETE FROM reviews WHERE job_id = 2`},
		{"review update", `UPDATE reviews SET closed = 1 WHERE job_id = 1`, []int64{1}, ""},
		{"review uuid", `UPDATE reviews SET uuid = '00000000-0000-4000-8000-000000000001' WHERE job_id = 1`, []int64{1}, ""},
		{"review delete", `DELETE FROM reviews WHERE job_id = 1`, []int64{1}, ""},
		{"repo rename", `UPDATE repos SET name = 'renamed' WHERE id = 1`, []int64{1, 2}, ""},
		{"job comment insert", `INSERT INTO responses (id, job_id, responder, response) VALUES (3, 1, 'author', 'new')`, []int64{1}, ""},
		{"job comment update", `UPDATE responses SET response = 'edited' WHERE id = 1`, []int64{1}, ""},
		{"job comment delete", `DELETE FROM responses WHERE id = 1`, []int64{1}, ""},
		{"commit comment insert", `INSERT INTO responses (id, commit_id, responder, response) VALUES (4, 1, 'author', 'new')`, []int64{1, 2}, ""},
		{"commit comment update", `UPDATE responses SET response = 'edited' WHERE id = 2`, []int64{1, 2}, ""},
		{"commit comment delete", `DELETE FROM responses WHERE id = 2`, []int64{1, 2}, ""},
		{"diff clear", `UPDATE job_content SET diff_content = NULL WHERE job_id = 1`, []int64{1}, ""},
		{"content delete", `DELETE FROM job_content WHERE job_id = 1`, []int64{1}, ""},
		{"content insert", `INSERT INTO job_content (job_id, prompt) VALUES (1, 'prompt')`, []int64{1}, `DELETE FROM job_content WHERE job_id = 1`},
		{"prompt only", `UPDATE job_content SET prompt = 'edited' WHERE job_id = 1`, nil, ""},
		{"worker only", `UPDATE review_jobs SET worker_id = 'worker' WHERE id = 1`, nil, ""},
		{"review synced", `UPDATE reviews SET synced_at = CURRENT_TIMESTAMP WHERE job_id = 1`, nil, ""},
		{"comment synced", `UPDATE responses SET synced_at = CURRENT_TIMESTAMP WHERE id = 1`, nil, ""},
		{"comment source", `UPDATE responses SET source = 'remote' WHERE id = 1`, nil, ""},
	}
	for _, column := range []string{"uuid", "repo_id", "branch", "git_ref", "commit_id", "review_type", "panel_role", "panel_run_uuid", "agent", "finished_at", "status", "job_type"} {
		cases = append(cases, changeCase{"job update column " + column, fmt.Sprintf("UPDATE review_jobs SET %s = %s WHERE id = 1", column, column), []int64{1}, ""})
	}
	cases = append(cases, changeCase{"job delete", `DELETE FROM review_jobs WHERE id = 2`, []int64{2}, ""})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := openTestDB(t)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			repoID, commitID := seedSearchFeedBase(t, db)
			seedSearchFeedReview(t, db, repoID, commitID, 1, searchFeedFixture{jobType: JobTypeReview, status: JobStatusDone, output: "original"})
			seedSearchFeedReview(t, db, repoID, commitID, 2, searchFeedFixture{jobType: JobTypeReview, status: JobStatusDone, output: "legacy", legacy: true, noCommit: true})
			_, err := db.ExecContext(t.Context(), `
				INSERT INTO job_content (job_id, prompt, diff_content) VALUES (1, 'prompt', 'diff');
				INSERT INTO responses (id, job_id, responder, response) VALUES (1, 1, 'author', 'comment');
				INSERT INTO responses (id, commit_id, responder, response) VALUES (2, 1, 'author', 'commit comment');`)
			require.NoError(t, err)
			if tc.setup != "" {
				_, err := db.ExecContext(t.Context(), tc.setup)
				require.NoError(t, err)
			}
			head, err := db.SearchChangesHead(t.Context())
			require.NoError(t, err)
			require.NoError(t, db.DeleteSearchChanges(t.Context(), head))
			_, err = db.ExecContext(t.Context(), tc.sql)
			require.NoError(t, err)
			var jobs []int64
			for {
				page, through, err := db.ListSearchChanges(t.Context(), 100)
				require.NoError(t, err)
				jobs = append(jobs, page...)
				if through == 0 {
					break
				}
				require.NoError(t, db.DeleteSearchChanges(t.Context(), through))
			}
			assert.ElementsMatch(t, tc.want, jobs)
		})
	}
}

func TestSearchChangesPreservesWritesAfterPage(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	_, err := db.ExecContext(t.Context(), `INSERT INTO search_changes(job_id) VALUES (1), (1), (2)`)
	require.NoError(t, err)
	jobs, through, err := db.ListSearchChanges(t.Context(), 2)
	require.NoError(t, err)
	assert.Equal(t, []int64{1}, jobs)
	_, err = db.ExecContext(t.Context(), `INSERT INTO search_changes(job_id) VALUES (3)`)
	require.NoError(t, err)
	require.NoError(t, db.DeleteSearchChanges(t.Context(), through))
	jobs, through, err = db.ListSearchChanges(t.Context(), 10)
	require.NoError(t, err)
	assert.Equal(t, []int64{2, 3}, jobs)
	require.NoError(t, db.DeleteSearchChanges(t.Context(), through))
	_, err = db.ExecContext(t.Context(), `INSERT INTO search_changes(job_id) VALUES (4)`)
	require.NoError(t, err)
	head, err := db.SearchChangesHead(t.Context())
	require.NoError(t, err)
	assert.Greater(t, head, through)
}
