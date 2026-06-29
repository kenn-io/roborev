package storage

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExportReviewsContentProfile(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	assert := assert.New(t)

	repo := createRepo(t, db, filepath.Join(t.TempDir(), "repo"))
	require.NoError(t, db.SetRepoIdentity(repo.ID, "github.com/acme/widgets"))
	commit := createCommit(t, db, repo.ID, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	job := enqueueJob(t, db, repo.ID, commit.ID, commit.SHA)
	claimJob(t, db, "w1")
	require.NoError(t, db.CompleteJob(job.ID, "codex", "SYSTEM PROMPT MUST NOT EXPORT", "No issues found."))
	_, err := db.Exec(`
		UPDATE review_jobs
		SET branch = 'main',
		    model = 'gpt-test',
		    enqueued_at = '2026-06-28T19:00:00-05:00',
		    token_usage = '{"input_tokens":123,"total_output_tokens":45,"cost_usd":0.67,"has_cost":true}',
		    started_at = '2026-06-29T00:00:00Z',
		    finished_at = '2026-06-29T00:00:02Z',
		    diff_content = 'RAW DIFF MUST NOT EXPORT',
		    command_line = 'TOKEN MUST NOT EXPORT'
		WHERE id = ?`, job.ID)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE reviews SET created_at = '2026-06-29 00:00:03' WHERE job_id = ?`, job.ID)
	require.NoError(t, err)

	page, err := db.ExportReviews(ExportReviewsOptions{Profile: ExportProfileContent, Limit: 10})
	require.NoError(t, err)
	require.Len(t, page.Reviews, 1)

	got := page.Reviews[0]
	assert.Equal("done", got.Status)
	assert.Equal("pass", got.Verdict)
	assert.Equal("repo", got.Project)
	assert.Equal("github.com/acme/widgets", got.Repo)
	assert.Equal("main", derefString(got.Branch))
	assert.Equal(commit.SHA, derefString(got.CommitSHA))
	assert.Equal("codex", got.Agent)
	assert.Equal("gpt-test", derefString(got.Model))
	assert.Equal("2026-06-29T00:00:00Z", got.CreatedAt)
	assert.Equal(int64(123), derefInt64(got.Cost.TokensIn))
	assert.Equal(int64(45), derefInt64(got.Cost.TokensOut))
	assert.InDelta(0.67, derefFloat64(got.Cost.USD), 1e-9)
	assert.Equal("No issues found.", derefString(got.Content))
	assert.Equal(int64(2000), derefInt64(got.DurationMS))
	assert.Equal("2026-06-29T00:00:03Z", got.CompletedAt)
	assert.Empty(got.Subagents)

	encoded, err := json.Marshal(page)
	require.NoError(t, err)
	body := string(encoded)
	assert.NotContains(body, "SYSTEM PROMPT MUST NOT EXPORT")
	assert.NotContains(body, "RAW DIFF MUST NOT EXPORT")
	assert.NotContains(body, "TOKEN MUST NOT EXPORT")
	assert.NotContains(body, "diff_content")
	assert.NotContains(body, "command_line")
	assert.NotContains(body, "prompt")
	assert.NotContains(body, "token_usage")
	assert.NotContains(body, "input_tokens")
	assert.NotContains(body, "total_output_tokens")
	assert.NotContains(body, "cost_usd")
	assert.NotContains(body, "has_cost")
}

func TestExportReviewsMetadataProfileOmitsContent(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, filepath.Join(t.TempDir(), "repo"))
	commit := createCommit(t, db, repo.ID, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	job := enqueueJob(t, db, repo.ID, commit.ID, commit.SHA)
	claimJob(t, db, "w1")
	require.NoError(t, db.CompleteJob(job.ID, "codex", "prompt", "No issues found. SECRET_OUTPUT"))

	page, err := db.ExportReviews(ExportReviewsOptions{Profile: ExportProfileMetadata, Limit: 10})
	require.NoError(t, err)
	require.Len(t, page.Reviews, 1)
	assert.Nil(t, page.Reviews[0].Content)
	assert.Equal(t, "repo", page.Reviews[0].Repo)

	encoded, err := json.Marshal(page)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "SECRET_OUTPUT")
	assert.NotContains(t, string(encoded), filepath.Dir(repo.RootPath))
}

func TestExportReviewsRepoFilterUsesExportedRepoIdentifier(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, filepath.Join(t.TempDir(), "repo"))
	seedCompletedExportReview(t, db, repo.ID, "1111111111111111111111111111111111111111", "2026-06-29 00:00:00", false)

	byRootPath, err := db.ExportReviews(ExportReviewsOptions{Profile: ExportProfileMetadata, Repo: repo.RootPath, Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, byRootPath.Reviews)

	byFallbackName, err := db.ExportReviews(ExportReviewsOptions{Profile: ExportProfileMetadata, Repo: repo.Name, Limit: 10})
	require.NoError(t, err)
	require.Len(t, byFallbackName.Reviews, 1)
	assert.Equal(t, repo.Name, byFallbackName.Reviews[0].Repo)

	require.NoError(t, db.SetRepoIdentity(repo.ID, "github.com/acme/widgets"))
	byOldFallbackName, err := db.ExportReviews(ExportReviewsOptions{Profile: ExportProfileMetadata, Repo: repo.Name, Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, byOldFallbackName.Reviews)

	byIdentity, err := db.ExportReviews(ExportReviewsOptions{Profile: ExportProfileMetadata, Repo: "github.com/acme/widgets", Limit: 10})
	require.NoError(t, err)
	require.Len(t, byIdentity.Reviews, 1)
	assert.Equal(t, "github.com/acme/widgets", byIdentity.Reviews[0].Repo)
}

func TestExportReviewsFiltersAndOrdering(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	assert := assert.New(t)

	repo := createRepo(t, db, filepath.Join(t.TempDir(), "repo"))
	older := seedCompletedExportReview(t, db, repo.ID, "1111111111111111111111111111111111111111", "2026-06-28 23:59:59", false)
	includedA := seedCompletedExportReview(t, db, repo.ID, "2222222222222222222222222222222222222222", "2026-06-29 00:00:00", false)
	includedB := seedCompletedExportReview(t, db, repo.ID, "3333333333333333333333333333333333333333", "2026-06-29T00:00:00Z", true)
	unfinished := enqueueJob(t, db, repo.ID, createCommit(t, db, repo.ID, "4444444444444444444444444444444444444444").ID, "4444444444444444444444444444444444444444")
	_, err := db.Exec(`UPDATE review_jobs SET status = 'done', job_type = 'task' WHERE id = ?`, older.ID)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE review_jobs SET status = 'queued' WHERE id = ?`, unfinished.ID)
	require.NoError(t, err)
	for i, jobType := range []string{JobTypeTask, JobTypeInsights, JobTypeFix, JobTypeClassify, JobTypeCompact} {
		excluded := seedCompletedExportReview(t, db, repo.ID, fmt.Sprintf("%040x", 50+i), "2026-06-29 00:00:00", false)
		_, err = db.Exec(`UPDATE review_jobs SET job_type = ? WHERE id = ?`, jobType, excluded.JobID)
		require.NoError(t, err)
	}
	empty := enqueueJob(t, db, repo.ID, createCommit(t, db, repo.ID, "5555555555555555555555555555555555555555").ID, "5555555555555555555555555555555555555555")
	claimJob(t, db, "w-empty")
	require.NoError(t, db.CompleteJob(empty.ID, "codex", "prompt", ""))
	_, err = db.Exec(`UPDATE reviews SET created_at = '2026-06-29 00:00:00' WHERE job_id = ?`, empty.ID)
	require.NoError(t, err)

	since := time.Date(2026, 6, 29, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC)
	page, err := db.ExportReviews(ExportReviewsOptions{
		Profile: ExportProfileContent,
		Since:   since,
		Until:   until,
		Limit:   10,
	})
	require.NoError(t, err)
	require.Len(t, page.Reviews, 2)
	assert.Equal([]string{includedA.UUID, includedB.UUID}, []string{page.Reviews[0].ReviewID, page.Reviews[1].ReviewID})

	closedOnly, err := db.ExportReviews(ExportReviewsOptions{Profile: ExportProfileContent, ClosedOnly: true, Limit: 10})
	require.NoError(t, err)
	require.Len(t, closedOnly.Reviews, 1)
	assert.Equal(includedB.UUID, closedOnly.Reviews[0].ReviewID)
}

func TestExportReviewsPagination(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	assert := assert.New(t)

	repo := createRepo(t, db, filepath.Join(t.TempDir(), "repo"))
	first := seedCompletedExportReview(t, db, repo.ID, "1111111111111111111111111111111111111111", "2026-06-29 00:00:00", false)
	second := seedCompletedExportReview(t, db, repo.ID, "2222222222222222222222222222222222222222", "2026-06-29 00:00:01", false)
	third := seedCompletedExportReview(t, db, repo.ID, "3333333333333333333333333333333333333333", "2026-06-29 00:00:02", false)

	page1, err := db.ExportReviews(ExportReviewsOptions{Profile: ExportProfileContent, Limit: 2})
	require.NoError(t, err)
	require.Len(t, page1.Reviews, 2)
	assert.True(page1.Truncated)
	require.NotNil(t, page1.NextCursor)
	assert.Equal([]string{first.UUID, second.UUID}, []string{page1.Reviews[0].ReviewID, page1.Reviews[1].ReviewID})

	page2, err := db.ExportReviews(ExportReviewsOptions{Profile: ExportProfileContent, Cursor: *page1.NextCursor, Limit: 2})
	require.NoError(t, err)
	require.Len(t, page2.Reviews, 1)
	assert.False(page2.Truncated)
	assert.Nil(page2.NextCursor)
	assert.Equal(third.UUID, page2.Reviews[0].ReviewID)
}

func TestExportReviewsCursorPaginatesSameCompletedTimestamp(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, filepath.Join(t.TempDir(), "repo"))
	first := seedCompletedExportReview(t, db, repo.ID, "1111111111111111111111111111111111111111", "2026-06-29 00:00:00", false)
	second := seedCompletedExportReview(t, db, repo.ID, "2222222222222222222222222222222222222222", "2026-06-29 00:00:00", false)

	page1, err := db.ExportReviews(ExportReviewsOptions{Profile: ExportProfileContent, Limit: 1})
	require.NoError(t, err)
	require.Len(t, page1.Reviews, 1)
	require.NotNil(t, page1.NextCursor)
	assert.Equal(t, first.UUID, page1.Reviews[0].ReviewID)

	page2, err := db.ExportReviews(ExportReviewsOptions{Profile: ExportProfileContent, Cursor: *page1.NextCursor, Limit: 1})
	require.NoError(t, err)
	require.Len(t, page2.Reviews, 1)
	assert.Equal(t, second.UUID, page2.Reviews[0].ReviewID)
}

func TestExportReviewsDefaultLimitIsBounded(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, filepath.Join(t.TempDir(), "repo"))
	for i := range exportDefaultPageLimit + 1 {
		seedCompletedExportReview(t, db, repo.ID, fmt.Sprintf("%040x", i+1), "2026-06-29 00:00:00", false)
	}

	page, err := db.ExportReviews(ExportReviewsOptions{Profile: ExportProfileContent})
	require.NoError(t, err)
	assert.Len(t, page.Reviews, exportDefaultPageLimit)
	assert.True(t, page.Truncated)
	assert.NotNil(t, page.NextCursor)
}

func TestExportReviewsEmptyPageUsesEmptyReviewsArray(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	page, err := db.ExportReviews(ExportReviewsOptions{Profile: ExportProfileContent, Limit: 10})
	require.NoError(t, err)
	assert.NotNil(t, page.Reviews)

	encoded, err := json.Marshal(page)
	require.NoError(t, err)
	assert.Contains(t, string(encoded), `"reviews":[]`)
	assert.NotContains(t, string(encoded), `"reviews":null`)
}

func TestExportReviewsRejectsInvalidCursorTimestamp(t *testing.T) {
	cursor := base64.RawURLEncoding.EncodeToString([]byte(`{"completed_at":"not-a-time","review_id":"r1"}`))
	db := openTestDB(t)
	defer db.Close()

	_, err := db.ExportReviews(ExportReviewsOptions{Profile: ExportProfileContent, Cursor: cursor, Limit: 10})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid export cursor timestamp")
}

func TestExportReviewsPanelSubagents(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	assert := assert.New(t)

	repo := createRepo(t, db, filepath.Join(t.TempDir(), "repo"))
	panelRun := "panel-run-1"
	memberA := seedPanelExportJob(t, db, repo.ID, panelRun, "member", "security", 0, "codex", "security", "2026-06-29 00:00:01", "- High — issue")
	memberB := seedPanelExportJob(t, db, repo.ID, panelRun, "member", "style", 1, "claude", "style", "2026-06-29 00:00:02", "No issues found.")
	synthesis := seedPanelExportJob(t, db, repo.ID, panelRun, "synthesis", "", 0, "codex", "", "2026-06-29 00:00:03", "- Medium — synthesized")
	_, err := db.Exec(`
		UPDATE review_jobs
		SET token_usage = '{"input_tokens":7,"total_output_tokens":9,"cost_usd":0.12,"has_cost":true}'
		WHERE id = ?`, memberA.JobID)
	require.NoError(t, err)
	queuedMember, err := db.EnqueueJob(EnqueueOpts{
		RepoID:           repo.ID,
		GitRef:           "aaaa..dddd",
		Agent:            "codex",
		PanelRunUUID:     panelRun,
		PanelRole:        PanelRoleMember,
		PanelMemberName:  "queued",
		PanelMemberIndex: 2,
	})
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE review_jobs SET job_type = 'range' WHERE id = ?`, queuedMember.ID)
	require.NoError(t, err)
	seedPanelMemberWithoutExportableReview(t, db, repo.ID, panelRun, "failed", 3, JobStatusFailed)
	seedPanelMemberWithoutExportableReview(t, db, repo.ID, panelRun, "skipped", 4, JobStatusSkipped)
	seedPanelMemberWithoutExportableReview(t, db, repo.ID, panelRun, "running", 5, JobStatusRunning)
	emptyMember := seedPanelExportJob(t, db, repo.ID, panelRun, "member", "empty", 6, "codex", "", "2026-06-29 00:00:04", "")
	require.Nil(t, emptyMember.VerdictBool)

	page, err := db.ExportReviews(ExportReviewsOptions{Profile: ExportProfileContent, Limit: 10})
	require.NoError(t, err)
	require.Len(t, page.Reviews, 1)
	got := page.Reviews[0]
	assert.Equal(synthesis.UUID, got.ReviewID)
	assert.Equal("dddddddddddddddddddddddddddddddddddddddd", derefString(got.CommitSHA))
	assert.Nil(got.PRNumber)
	assert.Nil(got.PRURL)
	require.Len(t, got.Subagents, 2)
	assert.Equal(memberA.UUID, got.Subagents[0].ReviewID)
	assert.Equal("security", got.Subagents[0].Name)
	assert.Equal("fail", got.Subagents[0].Verdict)
	assert.Equal("security", derefString(got.Subagents[0].ReviewType))
	assert.Equal("- High — issue", derefString(got.Subagents[0].Content))
	assert.Equal(int64(7), derefInt64(got.Subagents[0].Cost.TokensIn))
	assert.Equal(int64(9), derefInt64(got.Subagents[0].Cost.TokensOut))
	assert.InDelta(0.12, derefFloat64(got.Subagents[0].Cost.USD), 1e-9)
	assert.Equal(memberB.UUID, got.Subagents[1].ReviewID)
	assert.Equal("style", got.Subagents[1].Name)
	assert.Equal("pass", got.Subagents[1].Verdict)

	metadata, err := db.ExportReviews(ExportReviewsOptions{Profile: ExportProfileMetadata, Limit: 10})
	require.NoError(t, err)
	require.Len(t, metadata.Reviews, 1)
	require.Len(t, metadata.Reviews[0].Subagents, 2)
	assert.Nil(metadata.Reviews[0].Content)
	assert.Nil(metadata.Reviews[0].Subagents[0].Content)
}

func TestExportReviewsSynthesisWithoutMembersUsesEmptySubagentsArray(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, filepath.Join(t.TempDir(), "repo"))
	synthesis := seedPanelExportJob(t, db, repo.ID, "panel-run-empty", "synthesis", "", 0, "codex", "", "2026-06-29 00:00:03", "No issues found.")

	page, err := db.ExportReviews(ExportReviewsOptions{Profile: ExportProfileContent, Limit: 10})
	require.NoError(t, err)
	require.Len(t, page.Reviews, 1)
	assert.Equal(t, synthesis.UUID, page.Reviews[0].ReviewID)
	assert.NotNil(t, page.Reviews[0].Subagents)

	encoded, err := json.Marshal(page)
	require.NoError(t, err)
	assert.Contains(t, string(encoded), `"subagents":[]`)
	assert.NotContains(t, string(encoded), `"subagents":null`)
}

func TestExportReviewsCISynthesisFields(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, filepath.Join(t.TempDir(), "repo"))
	panelRun := "ci-panel-run"
	synthesis := seedPanelExportJob(t, db, repo.ID, panelRun, "synthesis", "", 0, "codex", "", "2026-06-29 00:00:03", "- Medium — synthesized")
	_, err := db.Exec(`
		UPDATE review_jobs
		SET ci_base_branch = 'main',
		    git_ref = 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa..bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb'
		WHERE id = ?`, synthesis.JobID)
	require.NoError(t, err)
	_, err = db.Exec(`
		INSERT INTO ci_pr_panels (github_repo, pr_number, head_sha, panel_run_uuid, synthesis_job_id)
		VALUES ('acme/widgets', 42, 'cccccccccccccccccccccccccccccccccccccccc', ?, ?)`,
		panelRun, synthesis.JobID)
	require.NoError(t, err)

	page, err := db.ExportReviews(ExportReviewsOptions{Profile: ExportProfileContent, Limit: 10})
	require.NoError(t, err)
	require.Len(t, page.Reviews, 1)
	got := page.Reviews[0]
	assert.Equal(t, "main", derefString(got.Branch))
	assert.Equal(t, "cccccccccccccccccccccccccccccccccccccccc", derefString(got.CommitSHA))
	assert.Equal(t, int64(42), derefInt64(got.PRNumber))
	assert.Equal(t, "https://github.com/acme/widgets/pull/42", derefString(got.PRURL))
}

func TestExportReviewsCapsContent(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, filepath.Join(t.TempDir(), "repo"))
	commit := createCommit(t, db, repo.ID, "cccccccccccccccccccccccccccccccccccccccc")
	job := enqueueJob(t, db, repo.ID, commit.ID, commit.SHA)
	claimJob(t, db, "w1")
	require.NoError(t, db.CompleteJob(job.ID, "codex", "prompt", strings.Repeat("x", exportContentMaxBytes+100)))

	page, err := db.ExportReviews(ExportReviewsOptions{Profile: ExportProfileContent, Limit: 10})
	require.NoError(t, err)
	require.Len(t, page.Reviews, 1)
	require.NotNil(t, page.Reviews[0].Content)
	assert.LessOrEqual(t, len(*page.Reviews[0].Content), exportContentMaxBytes+len(exportTruncationMarker))
	assert.True(t, strings.HasSuffix(*page.Reviews[0].Content, exportTruncationMarker))
}

func TestOpenBackfillsVerdictBoolForExport(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := Open(dbPath)
	require.NoError(t, err)

	repo := createRepo(t, db, filepath.Join(t.TempDir(), "repo"))
	commit := createCommit(t, db, repo.ID, "dddddddddddddddddddddddddddddddddddddddd")
	job := enqueueJob(t, db, repo.ID, commit.ID, commit.SHA)
	claimJob(t, db, "w1")
	require.NoError(t, db.CompleteJob(job.ID, "codex", "prompt", "No issues found."))
	_, err = db.Exec(`UPDATE reviews SET verdict_bool = NULL WHERE job_id = ?`, job.ID)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	reopened, err := Open(dbPath)
	require.NoError(t, err)
	defer reopened.Close()

	page, err := reopened.ExportReviews(ExportReviewsOptions{Profile: ExportProfileMetadata, Limit: 10})
	require.NoError(t, err)
	require.Len(t, page.Reviews, 1)
	assert.Equal(t, "pass", page.Reviews[0].Verdict)
}

func TestUpsertPulledReviewWithEmptyOutputIsNotExportable(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, filepath.Join(t.TempDir(), "repo"))
	commit := createCommit(t, db, repo.ID, "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee")
	job := enqueueJob(t, db, repo.ID, commit.ID, commit.SHA)
	_, err := db.Exec(`UPDATE review_jobs SET status = 'done' WHERE id = ?`, job.ID)
	require.NoError(t, err)
	require.NoError(t, db.UpsertPulledReview(PulledReview{
		UUID:               "pulled-review",
		JobUUID:            job.UUID,
		Agent:              "codex",
		Prompt:             "prompt",
		Output:             "",
		UpdatedByMachineID: "remote",
		CreatedAt:          time.Date(2026, 6, 29, 0, 0, 0, 0, time.UTC),
		UpdatedAt:          time.Date(2026, 6, 29, 0, 0, 1, 0, time.UTC),
	}))

	page, err := db.ExportReviews(ExportReviewsOptions{Profile: ExportProfileMetadata, Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, page.Reviews)
}

func seedCompletedExportReview(t *testing.T, db *DB, repoID int64, sha, completedAt string, closed bool) *Review {
	t.Helper()
	commit := createCommit(t, db, repoID, sha)
	job := enqueueJob(t, db, repoID, commit.ID, sha)
	markExportJobRunning(t, db, job.ID)
	require.NoError(t, db.CompleteJob(job.ID, "codex", "prompt", "No issues found."))
	_, err := db.Exec(`UPDATE reviews SET uuid = ?, created_at = ?, closed = ? WHERE job_id = ?`, "review-"+sha, completedAt, boolInt(closed), job.ID)
	require.NoError(t, err)
	review, err := db.GetReviewByJobID(job.ID)
	require.NoError(t, err)
	return review
}

func seedPanelMemberWithoutExportableReview(t *testing.T, db *DB, repoID int64, panelRun, memberName string, memberIndex int, status JobStatus) {
	t.Helper()
	job, err := db.EnqueueJob(EnqueueOpts{
		RepoID:           repoID,
		GitRef:           "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa..dddddddddddddddddddddddddddddddddddddddd",
		Agent:            "codex",
		PanelRunUUID:     panelRun,
		PanelRole:        PanelRoleMember,
		PanelMemberName:  memberName,
		PanelMemberIndex: memberIndex,
	})
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE review_jobs SET job_type = 'range', status = ? WHERE id = ?`, status, job.ID)
	require.NoError(t, err)
}

func seedPanelExportJob(
	t *testing.T,
	db *DB,
	repoID int64,
	panelRun string,
	role string,
	memberName string,
	memberIndex int,
	agentName string,
	reviewType string,
	completedAt string,
	output string,
) *Review {
	t.Helper()
	job, err := db.EnqueueJob(EnqueueOpts{
		RepoID:           repoID,
		GitRef:           "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa..dddddddddddddddddddddddddddddddddddddddd",
		Agent:            agentName,
		ReviewType:       reviewType,
		PanelRunUUID:     panelRun,
		PanelRole:        role,
		PanelMemberName:  memberName,
		PanelMemberIndex: memberIndex,
	})
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE review_jobs SET job_type = ?, started_at = '2026-06-29T00:00:00Z' WHERE id = ?`, panelJobType(role), job.ID)
	require.NoError(t, err)
	markExportJobRunning(t, db, job.ID)
	require.NoError(t, db.CompleteJob(job.ID, agentName, "prompt", output))
	_, err = db.Exec(`UPDATE reviews SET created_at = ? WHERE job_id = ?`, completedAt, job.ID)
	require.NoError(t, err)
	review, err := db.GetReviewByJobID(job.ID)
	require.NoError(t, err)
	return review
}

func markExportJobRunning(t *testing.T, db *DB, jobID int64) {
	t.Helper()
	_, err := db.Exec(`UPDATE review_jobs SET status = 'running', worker_id = 'export-test', started_at = '2026-06-29T00:00:00Z' WHERE id = ?`, jobID)
	require.NoError(t, err)
}

func panelJobType(role string) string {
	if role == PanelRoleSynthesis {
		return JobTypeSynthesis
	}
	return JobTypeRange
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func derefString(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

func derefInt64(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}

func derefFloat64(v *float64) float64 {
	if v == nil {
		return 0
	}
	return *v
}
