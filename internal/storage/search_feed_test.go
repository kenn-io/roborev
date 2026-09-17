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
		seedSearchFeedReview(t, db, repoID, commitID, i+1, fixture)
	}

	first, err := db.ListSearchDocuments(context.Background(), 0, 3)
	require.NoError(t, err)
	require.Len(t, first, 3)
	second, err := db.ListSearchDocuments(context.Background(), first[len(first)-1].ReviewID, 10)
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
}

func TestSearchFeedLimitSkipsSemanticallyEmptyStructuredRows(t *testing.T) {
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

	firstPage, err := db.ListSearchDocuments(context.Background(), 0, 1)
	require.NoError(t, err)
	require.Len(t, firstPage, 1)
	assert.Equal(t, firstID, firstPage[0].ReviewID)

	secondPage, err := db.ListSearchDocuments(context.Background(), firstPage[0].ReviewID, 1)
	require.NoError(t, err)
	require.Len(t, secondPage, 1)
	assert.Equal(t, laterID, secondPage[0].ReviewID)
	assert.Equal(t, "later eligible review", secondPage[0].Output)
}

func TestSearchFeedUsesLegacyCommentTargetAndStableResponseOrdering(t *testing.T) {
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
	require.NoError(t, db.QueryRow(`SELECT job_id FROM reviews WHERE id = ?`, reviewID).Scan(&jobID))

	_, err := db.Exec(`
		INSERT INTO responses (id, commit_id, responder, response, uuid, created_at)
		VALUES (90, ?, 'legacy', 'must stay out', '00000000-0000-4000-8000-000000000090', '2026-09-15T10:00:00Z')`, commitID)
	require.NoError(t, err)
	_, err = db.Exec(`
		INSERT INTO responses (id, job_id, responder, response, uuid, created_at)
		VALUES
			(13, ?, 'third', 'third response', '00000000-0000-4000-8000-000000000013', '2026-09-15T12:00:00Z'),
			(11, ?, 'first', 'first response', '00000000-0000-4000-8000-000000000011', '2026-09-15T12:00:00Z'),
			(12, ?, 'second', 'second response', '00000000-0000-4000-8000-000000000012', '2026-09-15T12:00:00Z')`, jobID, jobID, jobID)
	require.NoError(t, err)

	source, err := db.GetSearchDocument(context.Background(), "00000000-0000-4000-8000-000000000101")
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
	ordinary, err := db.GetSearchDocument(context.Background(), "00000000-0000-4000-8000-000000000102")
	require.NoError(t, err)
	require.Len(t, ordinary.Responses, 1)
	require.NotNil(t, ordinary.Responses[0].UUID)
	assert.Equal(t, "must stay out", ordinary.Responses[0].Response)
}

func TestSearchFeedSelectsOnlyAllowlistedSourceColumns(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repoID, commitID := seedSearchFeedBase(t, db)
	reviewID := seedSearchFeedReview(t, db, repoID, commitID, 1, searchFeedFixture{
		jobType: JobTypeReview,
		status:  JobStatusDone,
		output:  "safe review output",
	})
	var jobID int64
	require.NoError(t, db.QueryRow(`SELECT job_id FROM reviews WHERE id = ?`, reviewID).Scan(&jobID))
	_, err := db.Exec(`UPDATE review_jobs SET
		prompt = 'excluded-prompt', diff_content = 'excluded-diff', patch = 'excluded-patch',
		command_line = 'excluded-command', token_usage = 'excluded-token-data',
		worktree_path = 'excluded-worktree-path', error = 'excluded-log'
		WHERE id = ?`, jobID)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE reviews SET prompt = 'excluded-review-prompt' WHERE id = ?`, reviewID)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE repos SET root_path = 'excluded-repository-path', identity = 'excluded-remote-identity' WHERE id = ?`, repoID)
	require.NoError(t, err)

	source, err := db.GetSearchDocument(context.Background(), "00000000-0000-4000-8000-000000000101")
	require.NoError(t, err)
	encoded, err := json.Marshal(source)
	require.NoError(t, err)
	for _, excluded := range []string{
		"excluded-prompt", "excluded-diff", "excluded-patch", "excluded-command",
		"excluded-token-data", "excluded-worktree-path", "excluded-log",
		"excluded-review-prompt", "excluded-repository-path", "excluded-remote-identity",
	} {
		assert.NotContains(t, string(encoded), excluded)
	}
}

func TestSearchDocumentLookupSupportsUUIDAndLegacyLocalKey(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repoID, commitID := seedSearchFeedBase(t, db)
	modernID := seedSearchFeedReview(t, db, repoID, commitID, 1, searchFeedFixture{jobType: JobTypeReview, status: JobStatusDone, output: "modern"})
	legacyID := seedSearchFeedReview(t, db, repoID, commitID, 2, searchFeedFixture{jobType: "", status: JobStatusDone, output: "legacy", legacy: true})

	modern, err := db.GetSearchDocument(context.Background(), "00000000-0000-4000-8000-000000000101")
	require.NoError(t, err)
	assert.Equal(t, modernID, modern.ReviewID)
	legacy, err := db.GetSearchDocument(context.Background(), fmt.Sprintf("local:%d", legacyID))
	require.NoError(t, err)
	assert.Equal(t, legacyID, legacy.ReviewID)
	_, err = db.GetSearchDocument(context.Background(), "missing")
	assert.ErrorIs(t, err, sql.ErrNoRows)
}

func seedSearchFeedBase(t *testing.T, db *DB) (int64, int64) {
	t.Helper()
	repoResult, err := db.Exec(`INSERT INTO repos (root_path, name, identity) VALUES ('/synthetic/widgets', 'widgets', 'https://example.invalid/widgets.git')`)
	require.NoError(t, err)
	repoID, err := repoResult.LastInsertId()
	require.NoError(t, err)
	commitResult, err := db.Exec(`INSERT INTO commits (repo_id, sha, author, subject, timestamp)
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
	jobResult, err := db.Exec(`
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
	reviewResult, err := db.Exec(`
		INSERT INTO reviews (job_id, agent, prompt, output, structured_output, created_at, closed, uuid)
		VALUES (?, 'codex', 'review prompt', ?, ?, '2026-09-15T11:00:00Z', 0, ?)`,
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
