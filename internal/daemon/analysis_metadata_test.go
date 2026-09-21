package daemon

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func TestListJobsFiltersByRecordedAnalysis(t *testing.T) {
	server, db, _ := newTestServer(t)
	repo, err := db.GetOrCreateRepo("/tmp/analysis-list")
	require.NoError(t, err)

	refactor, err := db.EnqueueJob(storage.EnqueueOpts{
		RepoID:            repo.ID,
		GitRef:            "refactor",
		Label:             "refactor",
		Prompt:            "refactor prompt",
		Agent:             "test",
		AnalysisType:      "refactor",
		AnalysisFiles:     []string{"pkg/a.go", "pkg/b.go"},
		AnalysisCommitSHA: "refactor-sha",
	})
	require.NoError(t, err)
	_, err = db.EnqueueJob(storage.EnqueueOpts{
		RepoID:        repo.ID,
		GitRef:        "complexity",
		Label:         "complexity",
		Prompt:        "complexity prompt",
		Agent:         "test",
		AnalysisType:  "complexity",
		AnalysisFiles: []string{"pkg/b.go"},
	})
	require.NoError(t, err)
	legacy, err := db.EnqueueJob(storage.EnqueueOpts{
		RepoID: repo.ID,
		GitRef: "refactor",
		Prompt: "ordinary task",
		Agent:  "test",
	})
	require.NoError(t, err)

	typeOnly := fetchJobs(t, server, "analysis_type=refactor&limit=0")
	require.Len(t, typeOnly.Jobs, 1)
	assert.Equal(t, refactor.ID, typeOnly.Jobs[0].ID)
	assert.Equal(t, "refactor", typeOnly.Jobs[0].AnalysisType)
	assert.Equal(t, []string{"pkg/a.go", "pkg/b.go"}, typeOnly.Jobs[0].AnalysisFiles)
	assert.Equal(t, 1, typeOnly.Stats.Queued)

	fileOnly := fetchJobs(t, server, "analysis_file="+url.QueryEscape("pkg/a.go")+"&limit=0")
	require.Len(t, fileOnly.Jobs, 1)
	assert.Equal(t, refactor.ID, fileOnly.Jobs[0].ID)
	assert.Equal(t, 1, fileOnly.Stats.Queued)

	assert.Empty(t, fetchJobs(t, server, "analysis_file="+url.QueryEscape("pkg")+"&limit=0").Jobs)

	all := fetchJobs(t, server, "analysis_type=&analysis_file=&limit=0")
	assert.Len(t, all.Jobs, 3)
	assert.Contains(t, daemonJobIDs(all.Jobs), legacy.ID)
	assert.Equal(t, 3, all.Stats.Queued)
}

func TestEnqueueAnalysisMetadataThroughPrompt(t *testing.T) {
	server, db, _ := newTestServer(t)
	repo := testutil.NewGitRepo(t)
	repo.CommitFile("a.go", "package a", "add a")

	job := enqueueViaHTTP(t, server, EnqueueRequest{
		RepoPath:          repo.Path(),
		GitRef:            "refactor",
		CustomPrompt:      "analyze",
		Agent:             "test",
		JobType:           storage.JobTypeTask,
		AnalysisType:      "refactor",
		AnalysisFiles:     []string{"pkg/a.go"},
		AnalysisCommitSHA: "head-sha",
	})

	stored, err := db.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Equal(t, "refactor", stored.AnalysisType)
	assert.Equal(t, []string{"pkg/a.go"}, stored.AnalysisFiles)
	assert.Equal(t, "head-sha", stored.AnalysisCommitSHA)

	reviewJob := enqueueViaHTTP(t, server, EnqueueRequest{
		RepoPath:          repo.Path(),
		GitRef:            repo.HeadSHA(),
		Agent:             "test",
		JobType:           storage.JobTypeReview,
		AnalysisType:      "refactor",
		AnalysisFiles:     []string{"pkg/a.go"},
		AnalysisCommitSHA: "must-not-copy",
	})
	storedReview, err := db.GetJobByID(reviewJob.ID)
	require.NoError(t, err)
	assert.Empty(t, storedReview.AnalysisType)
	assert.Nil(t, storedReview.AnalysisFiles)
	assert.Empty(t, storedReview.AnalysisCommitSHA)
}

func daemonJobIDs(jobs []storage.ReviewJob) []int64 {
	ids := make([]int64, 0, len(jobs))
	for _, job := range jobs {
		ids = append(ids, job.ID)
	}
	return ids
}
