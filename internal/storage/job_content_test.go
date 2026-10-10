package storage

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestJobContentIsStoredCompressed(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	defer db.Close()
	repo := createRepo(t, db, t.TempDir())
	prompt := strings.Repeat("diff --git a/main.go b/main.go\n+fmt.Println(\"hello\")\n", 2000)
	job, err := db.EnqueueJob(EnqueueOpts{
		RepoID: repo.ID, GitRef: "dirty", Agent: "codex", Prompt: prompt, DiffContent: "+dirty line\n",
	})
	require.NoError(t, err)

	var storedBytes int
	require.NoError(t, db.QueryRow(`SELECT length(prompt) FROM job_content WHERE job_id = ?`, job.ID).Scan(&storedBytes))
	assert.Less(t, storedBytes, len(prompt)/20)

	loaded, err := db.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Equal(t, prompt, loaded.Prompt)
	require.NotNil(t, loaded.DiffContent)
	assert.Equal(t, "+dirty line\n", *loaded.DiffContent)
}

func TestJobContentKeepsNULBytes(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	defer db.Close()
	_, _, job := createJobChain(t, db, t.TempDir(), "abc123")
	require.NoError(t, db.SaveJobPrompt(job.ID, "before\x00after\xe9"))

	loaded, err := db.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Equal(t, []byte("before\x00after\xe9"), []byte(loaded.Prompt))
}

func TestSaveJobPromptReplacesAndClears(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	defer db.Close()
	_, _, job := createJobChain(t, db, t.TempDir(), "abc123")

	require.NoError(t, db.SaveJobPrompt(job.ID, "first prompt"))
	require.NoError(t, db.SaveJobPrompt(job.ID, "second prompt"))
	loaded, err := db.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Equal(t, "second prompt", loaded.Prompt)

	require.NoError(t, db.SaveJobPrompt(job.ID, ""))
	loaded, err = db.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Empty(t, loaded.Prompt)
}

func TestReviewPromptIsTheJobPrompt(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	defer db.Close()
	_, _, job := createJobChain(t, db, t.TempDir(), "abc123")
	claimJob(t, db, "worker")
	require.NoError(t, db.SaveJobPrompt(job.ID, "review this change"))
	require.NoError(t, db.CompleteJobResult(job.ID, "codex",
		ReviewCompletion{StructuredOutput: reviewFixtureJSON("No issues found.")}))

	review, err := db.GetReviewByJobID(job.ID)
	require.NoError(t, err)
	assert.Equal(t, "review this change", review.Prompt)
}

func TestCorruptJobContentIsAnError(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	defer db.Close()
	_, _, job := createJobChain(t, db, t.TempDir(), "abc123")
	_, err := db.Exec(`INSERT INTO job_content (job_id, prompt) VALUES (?, x'00010203')`, job.ID)
	require.NoError(t, err)

	_, err = db.GetJobByID(job.ID)
	require.ErrorContains(t, err, "zstd_decompress")
}

func TestPruneJobPromptsKeepsRecentAndUnrebuildableJobs(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	defer db.Close()
	repo := createRepo(t, db, t.TempDir())
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

	type fixture struct {
		jobType, status, finishedAt string
		pruned                      bool
	}
	jobs := map[string]fixture{
		"old range":         {JobTypeRange, "done", "2026-08-01T00:00:00Z", true},
		"old failed review": {JobTypeReview, "failed", "2026-08-01T00:00:00-05:00", true},
		"recent range":      {JobTypeRange, "done", "2026-10-08T00:00:00Z", false},
		"old task":          {JobTypeTask, "done", "2026-08-01T00:00:00Z", false},
		"old fix":           {JobTypeFix, "done", "2026-08-01T00:00:00Z", false},
		"old dirty":         {JobTypeDirty, "done", "2026-08-01T00:00:00Z", false},
	}
	ids := map[string]int64{}
	for name, f := range jobs {
		result, err := db.Exec(`INSERT INTO review_jobs (repo_id, git_ref, agent, status, job_type, finished_at)
			VALUES (?, 'a..b', 'codex', ?, ?, ?)`, repo.ID, f.status, f.jobType, f.finishedAt)
		require.NoError(t, err)
		ids[name], err = result.LastInsertId()
		require.NoError(t, err)
		require.NoError(t, db.SaveJobPrompt(ids[name], "prompt for "+name))
	}

	removed, err := db.PruneJobPrompts(t.Context(), now.AddDate(0, 0, -30))
	require.NoError(t, err)
	assert.EqualValues(t, 2, removed)

	for name, f := range jobs {
		job, err := db.GetJobByID(ids[name])
		require.NoError(t, err)
		if f.pruned {
			assert.Empty(t, job.Prompt, name)
		} else {
			assert.Equal(t, "prompt for "+name, job.Prompt, name)
		}
	}
}

func TestPulledJobContentFollowsJobChanges(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	defer db.Close()
	repo := createRepo(t, db, t.TempDir())
	updated := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	pulled := PulledJob{
		UUID: testUUID("pulled-job"), GitRef: "a..b", Agent: "codex", Status: "failed",
		Prompt: "first attempt", JobType: JobTypeRange, EnqueuedAt: updated,
		UpdatedAt: updated, SourceMachineID: testUUID("remote"),
	}
	promptAfterPull := func() string {
		t.Helper()
		_, err := db.upsertPulledJob(pulled, repo.ID, nil)
		require.NoError(t, err)
		var id int64
		require.NoError(t, db.QueryRow(`SELECT id FROM review_jobs WHERE uuid = ?`, pulled.UUID).Scan(&id))
		job, err := db.GetJobByID(id)
		require.NoError(t, err)
		return job.Prompt
	}

	assert.Equal(t, "first attempt", promptAfterPull())

	pulled.Status, pulled.Prompt, pulled.UpdatedAt = "done", "rerun attempt", updated.Add(time.Hour)
	assert.Equal(t, "rerun attempt", promptAfterPull())

	var id int64
	require.NoError(t, db.QueryRow(`SELECT id FROM review_jobs WHERE uuid = ?`, pulled.UUID).Scan(&id))
	require.NoError(t, db.SaveJobPrompt(id, ""))
	assert.Empty(t, promptAfterPull(), "replaying an unchanged job must not restore a removed prompt")
}

func TestEnqueueJobRollsBackWhenContentWriteFails(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	defer db.Close()
	repo := createRepo(t, db, t.TempDir())
	_, err := db.Exec(`CREATE TRIGGER reject_content BEFORE INSERT ON job_content
		BEGIN SELECT RAISE(ABORT, 'content write rejected'); END`)
	require.NoError(t, err)

	_, err = db.EnqueueJob(EnqueueOpts{RepoID: repo.ID, GitRef: "dirty", Agent: "codex", DiffContent: "+change\n"})
	require.ErrorContains(t, err, "content write rejected")

	var jobs int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM review_jobs`).Scan(&jobs))
	assert.Zero(t, jobs)
}

// openLegacyContentDB creates a database in the layout used before
// job_content: payloads in review_jobs, a second prompt copy in reviews, and a
// third in legacy_reviews.
func openLegacyContentDB(t *testing.T) (string, *DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "reviews.db")
	db, err := Open(path)
	require.NoError(t, err)
	for _, stmt := range []string{
		`ALTER TABLE review_jobs ADD COLUMN prompt TEXT`,
		`ALTER TABLE review_jobs ADD COLUMN diff_content TEXT`,
		`ALTER TABLE review_jobs ADD COLUMN patch TEXT`,
		`ALTER TABLE reviews ADD COLUMN prompt TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE legacy_reviews ADD COLUMN prompt TEXT NOT NULL DEFAULT ''`,
	} {
		_, err := db.Exec(stmt)
		require.NoError(t, err)
	}
	return path, db
}

func TestMigrateJobContentMovesLegacyPayloads(t *testing.T) {
	t.Parallel()
	path, db := openLegacyContentDB(t)
	repo := createRepo(t, db, t.TempDir())
	insertJob := func(gitRef, jobType, prompt string, diff, patch any) int64 {
		result, err := db.Exec(`INSERT INTO review_jobs (repo_id, git_ref, agent, status, job_type, prompt, diff_content, patch)
			VALUES (?, ?, 'codex', 'done', ?, ?, ?, ?)`, repo.ID, gitRef, jobType, prompt, diff, patch)
		require.NoError(t, err)
		id, err := result.LastInsertId()
		require.NoError(t, err)
		return id
	}
	insertReview := func(jobID int64, prompt string) {
		_, err := db.Exec(`INSERT INTO reviews (job_id, agent, prompt, output, structured_output)
			VALUES (?, 'codex', ?, '', ?)`, jobID, prompt, string(reviewFixtureJSON("No issues found.")))
		require.NoError(t, err)
	}
	withJobPrompt := insertJob("a..b", JobTypeRange, "job prompt", nil, nil)
	insertReview(withJobPrompt, "job prompt with preamble")
	reviewPromptOnly := insertJob("c..d", JobTypeRange, "", nil, nil)
	insertReview(reviewPromptOnly, "only the review kept this prompt")
	archivedOnly := insertJob("g..h", JobTypeRange, "", nil, nil)
	dirty := insertJob("dirty", JobTypeDirty, "dirty prompt", "+frozen diff\n", nil)
	fix := insertJob("e..f", JobTypeFix, "fix prompt", nil, "patch body")
	// A partly finished earlier run left a stale row behind.
	_, err := db.Exec(`INSERT INTO job_content (job_id, prompt) VALUES (?, zstd_compress('stale'))`, fix)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO legacy_reviews (job_id, agent, prompt, output, created_at, closed, migration_error)
		VALUES (?, 'codex', 'archived prompt', 'archived output', datetime('now'), 0, 'pending')`, withJobPrompt)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO legacy_reviews (job_id, agent, prompt, output, created_at, closed, migration_error)
		VALUES (?, 'codex', 'only the archive kept this prompt', 'archived output', datetime('now'), 0, 'pending')`, archivedOnly)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	for range 2 {
		db, err = Open(path)
		require.NoError(t, err)

		job, err := db.GetJobByID(withJobPrompt)
		require.NoError(t, err)
		assert.Equal(t, "job prompt", job.Prompt)
		review, err := db.GetReviewByJobID(reviewPromptOnly)
		require.NoError(t, err)
		assert.Equal(t, "only the review kept this prompt", review.Prompt)
		job, err = db.GetJobByID(archivedOnly)
		require.NoError(t, err)
		assert.Equal(t, "only the archive kept this prompt", job.Prompt)
		diff, err := db.GetJobDiffContent(dirty)
		require.NoError(t, err)
		assert.Equal(t, "+frozen diff\n", diff)
		job, err = db.GetJobByID(fix)
		require.NoError(t, err)
		assert.Equal(t, "fix prompt", job.Prompt)
		require.NotNil(t, job.Patch)
		assert.Equal(t, "patch body", *job.Patch)

		var archived string
		require.NoError(t, db.QueryRow(`SELECT output FROM legacy_reviews WHERE job_id = ?`, withJobPrompt).Scan(&archived))
		assert.Equal(t, "archived output", archived)
		legacy, err := hasColumn(t.Context(), db, "reviews", "prompt")
		require.NoError(t, err)
		assert.False(t, legacy)
		require.NoError(t, db.Close())
	}
}
