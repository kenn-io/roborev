package storage

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetSummary_Empty(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	s, err := db.GetSummary(SummaryOptions{
		Since: time.Now().Add(-7 * 24 * time.Hour),
	})
	require.NoError(t, err)
	assert.Equal(t, 0, s.Overview.Total)
	assert.Equal(t, 0, s.Verdicts.Total)
	assert.Empty(t, s.Agents)
	assert.Empty(t, s.JobTypes)
	assert.Equal(t, 0, s.Failures.Total)
}

func TestGetSummary_Overview(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, "/tmp/test-repo")
	commit := createCommit(t, db, repo.ID, "abc123")

	j1 := enqueueJob(t, db, repo.ID, commit.ID, "abc123")
	j2 := enqueueJob(t, db, repo.ID, commit.ID, "abc123")
	_ = enqueueJob(t, db, repo.ID, commit.ID, "abc123") // stays queued

	claimJob(t, db, "w1")
	require.NoError(t, db.CompleteJob(j1.ID, "codex", "prompt", "No issues found."))

	claimJob(t, db, "w2")
	require.NoError(t, db.CompleteJob(j2.ID, "codex", "prompt", "- Medium — Bug found"))

	s, err := db.GetSummary(SummaryOptions{
		Since: time.Now().Add(-1 * time.Hour),
	})
	require.NoError(t, err)

	assert.Equal(t, 3, s.Overview.Total)
	assert.Equal(t, 2, s.Overview.Done)
	assert.Equal(t, 1, s.Overview.Queued)
}

func TestGetSummary_Verdicts(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, "/tmp/test-repo")
	commit := createCommit(t, db, repo.ID, "abc123")

	// 2 pass, 1 fail
	for range 2 {
		j := enqueueJob(t, db, repo.ID, commit.ID, "abc123")
		claimJob(t, db, "w1")
		require.NoError(t, db.CompleteJob(j.ID, "codex", "p", "No issues found."))
	}
	j := enqueueJob(t, db, repo.ID, commit.ID, "abc123")
	claimJob(t, db, "w1")
	require.NoError(t, db.CompleteJob(j.ID, "codex", "p", "- High — Security issue"))

	s, err := db.GetSummary(SummaryOptions{
		Since: time.Now().Add(-1 * time.Hour),
	})
	require.NoError(t, err)

	assert.Equal(t, 3, s.Verdicts.Total)
	assert.Equal(t, 2, s.Verdicts.Passed)
	assert.Equal(t, 1, s.Verdicts.Failed)
	assert.InDelta(t, 0.667, s.Verdicts.PassRate, 0.01)
	assert.InDelta(t, 0.0, s.Verdicts.ResolutionRate, 0.01)
}

func TestGetSummary_ResolutionRate(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, "/tmp/test-repo")
	commit := createCommit(t, db, repo.ID, "abc123")

	// 3 failing reviews, close 2 of them (addressed)
	for i := range 3 {
		j := enqueueJob(t, db, repo.ID, commit.ID, "abc123")
		claimJob(t, db, "w1")
		require.NoError(t, db.CompleteJob(j.ID, "codex", "p", "- High — Bug"))
		if i < 2 {
			require.NoError(t, db.MarkReviewClosedByJobID(j.ID, true))
		}
	}

	s, err := db.GetSummary(SummaryOptions{
		Since: time.Now().Add(-1 * time.Hour),
	})
	require.NoError(t, err)

	assert.Equal(t, 3, s.Verdicts.Failed)
	assert.Equal(t, 2, s.Verdicts.Addressed)
	assert.InDelta(t, 0.667, s.Verdicts.ResolutionRate, 0.01)
}

func TestGetSummary_AgentBreakdown(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, "/tmp/test-repo")
	commit := createCommit(t, db, repo.ID, "abc123")

	for _, agent := range []string{"codex", "codex", "claude-code"} {
		j, err := db.EnqueueJob(EnqueueOpts{
			RepoID: repo.ID, CommitID: commit.ID, GitRef: "abc123", Agent: agent,
		})
		require.NoError(t, err)
		claimJob(t, db, "w1")
		require.NoError(t, db.CompleteJob(j.ID, agent, "p", "No issues found."))
	}

	s, err := db.GetSummary(SummaryOptions{
		Since: time.Now().Add(-1 * time.Hour),
	})
	require.NoError(t, err)

	assert.Len(t, s.Agents, 2)
	assert.Equal(t, "codex", s.Agents[0].Agent)
	assert.Equal(t, 2, s.Agents[0].Total)
	assert.Equal(t, "claude-code", s.Agents[1].Agent)
	assert.Equal(t, 1, s.Agents[1].Total)
}

func TestGetSummary_JobTypes(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, "/tmp/test-repo")
	commit := createCommit(t, db, repo.ID, "abc123")

	enqueueJob(t, db, repo.ID, commit.ID, "abc123")

	_, err := db.EnqueueJob(EnqueueOpts{
		RepoID: repo.ID, GitRef: "analyze", Agent: "codex",
		Prompt: "analyze this", JobType: JobTypeTask,
	})
	require.NoError(t, err)

	s, err := db.GetSummary(SummaryOptions{
		Since: time.Now().Add(-1 * time.Hour),
	})
	require.NoError(t, err)

	assert.Len(t, s.JobTypes, 2)
	typeMap := make(map[string]int)
	for _, jt := range s.JobTypes {
		typeMap[jt.Type] = jt.Count
	}
	assert.Equal(t, 1, typeMap["review"])
	assert.Equal(t, 1, typeMap["task"])
}

func TestGetSummary_Failures(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, "/tmp/test-repo")
	commit := createCommit(t, db, repo.ID, "abc123")

	errors := []string{
		"quota exceeded: rate limit hit",
		"timeout: deadline exceeded",
		"exit status 1",
	}
	for _, errMsg := range errors {
		j := enqueueJob(t, db, repo.ID, commit.ID, "abc123")
		claimJob(t, db, "w1")
		_, err := db.FailJob(j.ID, "w1", errMsg)
		require.NoError(t, err)
	}

	s, err := db.GetSummary(SummaryOptions{
		Since: time.Now().Add(-1 * time.Hour),
	})
	require.NoError(t, err)

	assert.Equal(t, 3, s.Failures.Total)
	assert.Equal(t, 1, s.Failures.Errors["quota"])
	assert.Equal(t, 1, s.Failures.Errors["timeout"])
	assert.Equal(t, 1, s.Failures.Errors["crash"])
}

func TestGetSummary_RepoFilter(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo1 := createRepo(t, db, "/tmp/repo1")
	repo2 := createRepo(t, db, "/tmp/repo2")
	c1 := createCommit(t, db, repo1.ID, "aaa111")
	c2 := createCommit(t, db, repo2.ID, "bbb222")

	enqueueJob(t, db, repo1.ID, c1.ID, "aaa111")
	enqueueJob(t, db, repo1.ID, c1.ID, "aaa111")
	enqueueJob(t, db, repo2.ID, c2.ID, "bbb222")

	// All repos
	s, err := db.GetSummary(SummaryOptions{
		Since: time.Now().Add(-1 * time.Hour),
	})
	require.NoError(t, err)
	assert.Equal(t, 3, s.Overview.Total)

	// Filtered to repo1
	s, err = db.GetSummary(SummaryOptions{
		RepoPath: repo1.RootPath,
		Since:    time.Now().Add(-1 * time.Hour),
	})
	require.NoError(t, err)
	assert.Equal(t, 2, s.Overview.Total)
}

func TestGetSummary_RepoFilterNormalizesWindowsSeparators(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, `C:\Users\dev\Projects\repo`)
	commit := createCommit(t, db, repo.ID, "aaa111")
	enqueueJob(t, db, repo.ID, commit.ID, "aaa111")

	s, err := db.GetSummary(SummaryOptions{
		RepoPath: `C:\Users\dev\Projects\repo`,
		Since:    time.Now().Add(-1 * time.Hour),
	})
	require.NoError(t, err)
	assert.Equal(t, "C:/Users/dev/Projects/repo", s.RepoPath)
	assert.Equal(t, 1, s.Overview.Total)
}

func TestGetSummary_BranchFilter(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, "/tmp/test-repo")
	commit := createCommit(t, db, repo.ID, "abc123")

	for _, branch := range []string{"main", "main", "feature"} {
		_, err := db.EnqueueJob(EnqueueOpts{
			RepoID: repo.ID, CommitID: commit.ID, GitRef: "abc123",
			Agent: "codex", Branch: branch,
		})
		require.NoError(t, err)
	}

	s, err := db.GetSummary(SummaryOptions{
		Branch: "main",
		Since:  time.Now().Add(-1 * time.Hour),
	})
	require.NoError(t, err)
	assert.Equal(t, 2, s.Overview.Total)
}

func TestGetSummary_SinceFilter(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, "/tmp/test-repo")
	commit := createCommit(t, db, repo.ID, "abc123")

	enqueueJob(t, db, repo.ID, commit.ID, "abc123")

	// Since in the future — nothing
	s, err := db.GetSummary(SummaryOptions{
		Since: time.Now().Add(1 * time.Hour),
	})
	require.NoError(t, err)
	assert.Equal(t, 0, s.Overview.Total)

	// Since in the past — finds it
	s, err = db.GetSummary(SummaryOptions{
		Since: time.Now().Add(-1 * time.Hour),
	})
	require.NoError(t, err)
	assert.Equal(t, 1, s.Overview.Total)
}

func TestGetSummary_RFC3339Timestamps(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, "/tmp/test-repo")
	commit := createCommit(t, db, repo.ID, "abc123")

	j := enqueueJob(t, db, repo.ID, commit.ID, "abc123")
	rfc3339Time := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)
	_, err := db.Exec("UPDATE review_jobs SET enqueued_at = ? WHERE id = ?", rfc3339Time, j.ID)
	require.NoError(t, err)

	s, err := db.GetSummary(SummaryOptions{
		Since: time.Now().Add(-2 * time.Hour),
	})
	require.NoError(t, err)
	assert.Equal(t, 1, s.Overview.Total)

	s, err = db.GetSummary(SummaryOptions{
		Since: time.Now(),
	})
	require.NoError(t, err)
	assert.Equal(t, 0, s.Overview.Total)
}

func TestGetSummary_VerdictExcludesNonReviewJobs(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, "/tmp/test-repo")
	commit := createCommit(t, db, repo.ID, "abc123")

	// Normal review (pass)
	j1 := enqueueJob(t, db, repo.ID, commit.ID, "abc123")
	claimJob(t, db, "w1")
	require.NoError(t, db.CompleteJob(j1.ID, "codex", "p", "No issues found."))

	// Normal review (fail)
	j2 := enqueueJob(t, db, repo.ID, commit.ID, "abc123")
	claimJob(t, db, "w1")
	require.NoError(t, db.CompleteJob(j2.ID, "codex", "p", "- High — Bug found"))

	// Task job (no meaningful verdict)
	j3, err := db.EnqueueJob(EnqueueOpts{
		RepoID: repo.ID, GitRef: "analyze", Agent: "codex",
		Prompt: "analyze this", JobType: JobTypeTask,
	})
	require.NoError(t, err)
	claimJob(t, db, "w1")
	require.NoError(t, db.CompleteJob(j3.ID, "codex", "", "some analysis output"))

	// Fix job (no meaningful verdict)
	j4, err := db.EnqueueJob(EnqueueOpts{
		RepoID: repo.ID, CommitID: commit.ID, GitRef: "abc123",
		Agent: "codex", JobType: JobTypeFix, ParentJobID: j2.ID,
	})
	require.NoError(t, err)
	claimJob(t, db, "w1")
	require.NoError(t, db.CompleteJob(j4.ID, "codex", "", "applied fix"))

	s, err := db.GetSummary(SummaryOptions{
		Since: time.Now().Add(-1 * time.Hour),
	})
	require.NoError(t, err)

	assert.Equal(t, 4, s.Overview.Done)
	assert.Equal(t, 2, s.Verdicts.Total)
	assert.Equal(t, 1, s.Verdicts.Passed)
	assert.Equal(t, 1, s.Verdicts.Failed)
	assert.Equal(t, s.Verdicts.Passed+s.Verdicts.Failed, s.Verdicts.Total)

	require.Len(t, s.Agents, 1)
	assert.Equal(t, "codex", s.Agents[0].Agent)
	assert.Equal(t, 4, s.Agents[0].Total)
	assert.Equal(t, 1, s.Agents[0].Passed)
	assert.Equal(t, 1, s.Agents[0].Failed)
}

func TestGetSummary_RepoBreakdown(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo1 := createRepo(t, db, "/tmp/repo1")
	repo2 := createRepo(t, db, "/tmp/repo2")
	c1 := createCommit(t, db, repo1.ID, "aaa111")
	c2 := createCommit(t, db, repo2.ID, "bbb222")

	// repo1: 2 jobs (1 pass, 1 fail)
	j := enqueueJob(t, db, repo1.ID, c1.ID, "aaa111")
	claimJob(t, db, "w1")
	require.NoError(t, db.CompleteJob(j.ID, "codex", "p", "No issues found."))

	j = enqueueJob(t, db, repo1.ID, c1.ID, "aaa111")
	claimJob(t, db, "w1")
	require.NoError(t, db.CompleteJob(j.ID, "codex", "p", "- High — Bug"))

	// repo2: 1 job (pass)
	j = enqueueJob(t, db, repo2.ID, c2.ID, "bbb222")
	claimJob(t, db, "w1")
	require.NoError(t, db.CompleteJob(j.ID, "codex", "p", "No issues found."))

	// All repos query includes repo breakdown
	s, err := db.GetSummary(SummaryOptions{
		Since:    time.Now().Add(-1 * time.Hour),
		AllRepos: true,
	})
	require.NoError(t, err)

	require.Len(t, s.Repos, 2)
	// Sorted by total desc
	assert.Equal(t, repo1.RootPath, s.Repos[0].Path)
	assert.Equal(t, 2, s.Repos[0].Total)
	assert.Equal(t, 1, s.Repos[0].Passed)
	assert.Equal(t, 1, s.Repos[0].Failed)

	assert.Equal(t, repo2.RootPath, s.Repos[1].Path)
	assert.Equal(t, 1, s.Repos[1].Total)
	assert.Equal(t, 1, s.Repos[1].Passed)
	assert.Equal(t, 0, s.Repos[1].Failed)
}

func TestGetSummary_RepoBreakdownOmittedForSingleRepo(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, "/tmp/test-repo")
	commit := createCommit(t, db, repo.ID, "abc123")
	enqueueJob(t, db, repo.ID, commit.ID, "abc123")

	s, err := db.GetSummary(SummaryOptions{
		RepoPath: repo.RootPath,
		Since:    time.Now().Add(-1 * time.Hour),
	})
	require.NoError(t, err)
	assert.Empty(t, s.Repos)
}

func TestBackfillVerdictBool(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := createRepo(t, db, "/tmp/test-repo")
	commit := createCommit(t, db, repo.ID, "abc123")

	// Create reviews with verdict_bool set (normal path)
	j1 := enqueueJob(t, db, repo.ID, commit.ID, "abc123")
	claimJob(t, db, "w1")
	require.NoError(t, db.CompleteJob(j1.ID, "codex", "p", "No issues found."))

	j2 := enqueueJob(t, db, repo.ID, commit.ID, "abc123")
	claimJob(t, db, "w1")
	require.NoError(t, db.CompleteJob(j2.ID, "codex", "p", "- High — Bug"))

	// Simulate legacy rows by nullifying verdict_bool
	_, err := db.Exec(`UPDATE reviews SET verdict_bool = NULL`)
	require.NoError(t, err)

	// Verify summary sees nothing before backfill
	s, err := db.GetSummary(SummaryOptions{Since: time.Now().Add(-1 * time.Hour)})
	require.NoError(t, err)
	assert.Equal(t, 0, s.Verdicts.Total)

	// Backfill
	count, err := db.BackfillVerdictBool()
	require.NoError(t, err)
	assert.Equal(t, 2, count)

	// Verify summary now sees the verdicts
	s, err = db.GetSummary(SummaryOptions{Since: time.Now().Add(-1 * time.Hour)})
	require.NoError(t, err)
	assert.Equal(t, 2, s.Verdicts.Total)
	assert.Equal(t, 1, s.Verdicts.Passed)
	assert.Equal(t, 1, s.Verdicts.Failed)

	// Running again is a no-op
	count, err = db.BackfillVerdictBool()
	require.NoError(t, err)
	assert.Equal(t, 0, count)
}

func TestPercentile(t *testing.T) {
	tests := []struct {
		name   string
		values []float64
		p      float64
		want   float64
	}{
		{"empty", nil, 0.5, 0},
		{"single", []float64{42}, 0.5, 42},
		{"two_p50", []float64{10, 20}, 0.5, 15},
		{"three_p50", []float64{10, 20, 30}, 0.5, 20},
		{"five_p90", []float64{1, 2, 3, 4, 5}, 0.9, 4.6},
		{"unsorted", []float64{5, 1, 3, 2, 4}, 0.5, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := percentile(tt.values, tt.p)
			assert.InDelta(t, tt.want, got, 0.01)
		})
	}
}

func TestCategorizeError(t *testing.T) {
	tests := []struct {
		err  string
		want string
	}{
		{"quota exceeded", "quota"},
		{"rate limit hit (429)", "quota"},
		{"context deadline exceeded", "timeout"},
		{"process killed by signal", "crash"},
		{"exit status 1", "crash"},
		{"something unknown", "other"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			assert.Equal(t, tt.want, categorizeError(tt.err))
		})
	}
}
