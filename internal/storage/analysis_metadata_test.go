package storage

import (
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAnalysisMetadata(t *testing.T) {
	db, repo := setupDBAndRepo(t, "analysis-metadata")

	recorded, err := db.EnqueueJob(EnqueueOpts{
		RepoID:            repo.ID,
		GitRef:            "refactor",
		Label:             "refactor",
		Prompt:            "analyze this",
		Agent:             "test",
		AnalysisType:      "refactor",
		AnalysisFiles:     []string{"pkg/a.go", "pkg/b.go"},
		AnalysisCommitSHA: "abc123",
	})
	require.NoError(t, err)
	legacy, err := db.EnqueueJob(EnqueueOpts{
		RepoID: repo.ID,
		GitRef: "refactor",
		Prompt: "ordinary task",
		Agent:  "test",
	})
	require.NoError(t, err)

	got, err := db.GetJobByID(recorded.ID)
	require.NoError(t, err)
	assert.Equal(t, "refactor", got.AnalysisType)
	assert.Equal(t, []string{"pkg/a.go", "pkg/b.go"}, got.AnalysisFiles)
	assert.Equal(t, "abc123", got.AnalysisCommitSHA)

	old, err := db.GetJobByID(legacy.ID)
	require.NoError(t, err)
	assert.Empty(t, old.AnalysisType)
	assert.Nil(t, old.AnalysisFiles)
	assert.Empty(t, old.AnalysisCommitSHA)

	byType, err := db.ListJobs("", "", 0, 0, WithAnalysisType("refactor"))
	require.NoError(t, err)
	require.Len(t, byType, 1)
	assert.Equal(t, recorded.ID, byType[0].ID)

	byFile, err := db.ListJobs("", "", 0, 0, WithAnalysisFile("pkg/a.go"))
	require.NoError(t, err)
	require.Len(t, byFile, 1)
	assert.Equal(t, recorded.ID, byFile[0].ID)

	boundary, err := db.ListJobs("", "", 0, 0, WithAnalysisFile("pkg"))
	require.NoError(t, err)
	assert.Empty(t, boundary)

	stats, err := db.CountJobStats("", "", WithAnalysisType("refactor"), WithAnalysisFile("pkg/a.go"))
	require.NoError(t, err)
	assert.Equal(t, 1, stats.Queued)

	for _, value := range []string{
		"not-json",
		`"pkg/a.go"`,
		`{"file":"pkg/a.go"}`,
		`["pkg/a.go", 1]`,
		`[null]`,
		`["pkg/a.go", null]`,
	} {
		job, enqueueErr := db.EnqueueJob(EnqueueOpts{RepoID: repo.ID, Prompt: "invalid files", Agent: "test"})
		require.NoError(t, enqueueErr)
		_, updateErr := db.Exec("UPDATE review_jobs SET analysis_files = ? WHERE id = ?", value, job.ID)
		require.NoError(t, updateErr)

		hydrated, getErr := db.GetJobByID(job.ID)
		require.NoError(t, getErr)
		assert.Nil(t, hydrated.AnalysisFiles, value)

		listed, listErr := db.ListJobs("", "", 0, 0, WithAnalysisFile("pkg/a.go"))
		require.NoError(t, listErr, value)
		assert.NotContains(t, jobIDs(listed), job.ID, value)
		_, statsErr := db.CountJobStats("", "", WithAnalysisFile("pkg/a.go"))
		require.NoError(t, statsErr, value)
	}
}

func TestMigrateAddsAnalysisColumns(t *testing.T) {
	db := openTestDB(t)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	for _, name := range []string{"analysis_type", "analysis_files", "analysis_commit_sha"} {
		var count int
		err := db.QueryRow("SELECT COUNT(*) FROM pragma_table_info('review_jobs') WHERE name = ?", name).Scan(&count)
		require.NoError(t, err)
		assert.Equal(t, 1, count, name)
	}
	require.NoError(t, db.migrate())
}

func TestAnalysisMetadataLocalOnlySyncBoundary(t *testing.T) {
	db, repo := setupDBAndRepo(t, "analysis-metadata-sync")

	pulledUUID := uuid.New()
	pulledMachine := uuid.New()
	now := time.Now().UTC()
	require.NoError(t, db.UpsertPulledJob(PulledJob{
		UUID:            pulledUUID,
		GitRef:          "remote-refactor",
		Agent:           "test",
		Reasoning:       "thorough",
		JobType:         JobTypeTask,
		Status:          string(JobStatusDone),
		EnqueuedAt:      now,
		UpdatedAt:       now,
		SourceMachineID: pulledMachine,
	}, repo.ID, nil))

	var id int64
	require.NoError(t, db.QueryRow("SELECT id FROM review_jobs WHERE uuid = ?", pulledUUID).Scan(&id))
	pulled, err := db.GetJobByID(id)
	require.NoError(t, err)
	assert.Empty(t, pulled.AnalysisType)
	assert.Nil(t, pulled.AnalysisFiles)
	assert.Empty(t, pulled.AnalysisCommitSHA)
}

func TestAnalysisMetadataSurvivesClaimAndRetry(t *testing.T) {
	db, repo := setupDBAndRepo(t, "analysis-metadata-lifecycle")

	job, err := db.EnqueueJob(EnqueueOpts{
		RepoID:            repo.ID,
		Prompt:            "analyze this",
		Agent:             "test",
		AnalysisType:      "refactor",
		AnalysisFiles:     []string{"pkg/a.go"},
		AnalysisCommitSHA: "abc123",
	})
	require.NoError(t, err)

	claimed, err := db.ClaimJob("worker-1")
	require.NoError(t, err)
	require.NotNil(t, claimed)
	assert.Equal(t, job.ID, claimed.ID)
	assert.Equal(t, "refactor", claimed.AnalysisType)
	assert.Equal(t, []string{"pkg/a.go"}, claimed.AnalysisFiles)
	assert.Equal(t, "abc123", claimed.AnalysisCommitSHA)

	retried, err := db.RetryJob(job.ID, "worker-1", 2, 0)
	require.NoError(t, err)
	assert.True(t, retried)
	afterRetry, err := db.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Equal(t, "refactor", afterRetry.AnalysisType)
	assert.Equal(t, []string{"pkg/a.go"}, afterRetry.AnalysisFiles)
	assert.Equal(t, "abc123", afterRetry.AnalysisCommitSHA)
}

func jobIDs(jobs []ReviewJob) []int64 {
	ids := make([]int64, 0, len(jobs))
	for _, job := range jobs {
		ids = append(ids, job.ID)
	}
	return ids
}
