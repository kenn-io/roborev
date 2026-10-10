package storage

import (
	"database/sql"
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

	removed, err := db.PruneJobPrompts(t.Context(), now.AddDate(0, 0, -30), false)
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

func TestPruneJobPromptsKeepsUnpushedPromptsWhileSyncing(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	defer db.Close()
	repo := createRepo(t, db, t.TempDir())
	cutoff := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	jobs := map[string]struct {
		updatedAt, syncedAt any
		prunedWhileSyncing  bool
	}{
		"pushed":             {"2026-08-01T00:00:00Z", "2026-08-01T00:00:05Z", true},
		"never pushed":       {"2026-08-01T00:00:00Z", nil, false},
		"changed after push": {"2026-08-02T00:00:00Z", "2026-08-01T00:00:05Z", false},
	}
	insertAll := func() map[string]int64 {
		_, err := db.Exec(`DELETE FROM job_content`)
		require.NoError(t, err)
		ids := map[string]int64{}
		for name, f := range jobs {
			result, err := db.Exec(`INSERT INTO review_jobs (repo_id, git_ref, agent, status, job_type, finished_at,
					updated_at, synced_at)
				VALUES (?, 'a..b', 'codex', 'done', 'range', '2026-08-01T00:00:00Z', ?, ?)`,
				repo.ID, f.updatedAt, f.syncedAt)
			require.NoError(t, err)
			ids[name], err = result.LastInsertId()
			require.NoError(t, err)
			require.NoError(t, db.SaveJobPrompt(ids[name], "prompt for "+name))
		}
		return ids
	}

	ids := insertAll()
	removed, err := db.PruneJobPrompts(t.Context(), cutoff, true)
	require.NoError(t, err)
	assert.EqualValues(t, 1, removed)
	for name, f := range jobs {
		job, err := db.GetJobByID(ids[name])
		require.NoError(t, err)
		if f.prunedWhileSyncing {
			assert.Empty(t, job.Prompt, name)
		} else {
			assert.Equal(t, "prompt for "+name, job.Prompt, name)
		}
	}

	insertAll()
	removed, err = db.PruneJobPrompts(t.Context(), cutoff, false)
	require.NoError(t, err)
	assert.EqualValues(t, len(jobs), removed, "without sync every old prompt is removed")
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

	// A fast rerun can finish within the same second as the version pulled
	// before it, so only the prompt and the fraction of a second differ.
	pulled.Prompt, pulled.UpdatedAt = "same-second rerun", pulled.UpdatedAt.Add(300*time.Millisecond)
	assert.Equal(t, "same-second rerun", promptAfterPull())

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

// openLegacyContentDB creates a database at schema version 1, the layout
// before job_content: payloads in review_jobs, a second prompt copy in
// reviews, and a third in the legacy_reviews archive that earlier releases
// created.
func openLegacyContentDB(t *testing.T) (string, *DB) {
	t.Helper()
	require.NoError(t, registerContentFunctions())
	path := filepath.Join(t.TempDir(), "reviews.db")
	raw, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	db := &DB{DB: raw}
	require.NoError(t, db.runSchemaMigrations(t.Context(), schemaMigrations[:1]))
	_, err = db.Exec(`CREATE TABLE legacy_reviews (
		archive_id INTEGER PRIMARY KEY,
		id INTEGER, job_id INTEGER NOT NULL, agent TEXT NOT NULL, prompt TEXT NOT NULL,
		output TEXT NOT NULL, created_at TEXT NOT NULL, closed INTEGER NOT NULL,
		reviewed_file_count INTEGER, excluded_file_count INTEGER, verdict_bool INTEGER,
		structured_output TEXT, uuid TEXT UNIQUE, updated_by_machine_id TEXT,
		updated_at TEXT, synced_at TEXT, migration_error TEXT NOT NULL, resolved_at TEXT)`)
	require.NoError(t, err)
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
	clearedSinceEarlierRun := insertJob("i..j", JobTypeRange, "", nil, nil)
	// A rerun that failed before saving its prompt keeps only the archived
	// review of its earlier attempt.
	failedRerun := insertJob("k..l", JobTypeRange, "", nil, nil)
	_, err := db.Exec(`UPDATE review_jobs SET status = 'failed' WHERE id = ?`, failedRerun)
	require.NoError(t, err)
	// A partly finished earlier run left stale rows behind.
	_, err = db.Exec(`CREATE TABLE job_content (job_id INTEGER PRIMARY KEY, prompt BLOB, diff_content BLOB, patch BLOB)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO job_content (job_id, prompt) VALUES (?, zstd_compress('stale')), (?, zstd_compress('stale'))`,
		fix, clearedSinceEarlierRun)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO legacy_reviews (job_id, agent, prompt, output, created_at, closed, migration_error)
		VALUES (?, 'codex', 'earlier attempt prompt', 'archived output', datetime('now'), 0, 'pending')`, failedRerun)
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
		for _, id := range []int64{clearedSinceEarlierRun, failedRerun} {
			job, err = db.GetJobByID(id)
			require.NoError(t, err)
			assert.Empty(t, job.Prompt, "job %d", id)
		}

		var archived string
		require.NoError(t, db.QueryRow(`SELECT output FROM legacy_reviews WHERE job_id = ?`, withJobPrompt).Scan(&archived))
		assert.Equal(t, "archived output", archived)
		legacy, err := hasColumn(t.Context(), db, "reviews", "prompt")
		require.NoError(t, err)
		assert.False(t, legacy)
		require.NoError(t, db.Close())
	}
}

func TestMigrateJobContentRestoresPanelMemberInstructions(t *testing.T) {
	t.Parallel()
	path, db := openLegacyContentDB(t)
	repo := createRepo(t, db, t.TempDir())
	insertMember := func(prebuilt bool, config string) int64 {
		result, err := db.Exec(`INSERT INTO review_jobs (repo_id, git_ref, agent, status, job_type, prompt,
				prompt_prebuilt, panel_role, panel_name, panel_member_name, panel_member_config_json)
			VALUES (?, 'a..b', 'codex', 'done', 'range', 'review the diff in {{diff_file}}', ?, 'member', 'ci', 'security', ?)`,
			repo.ID, prebuilt, config)
		require.NoError(t, err)
		id, err := result.LastInsertId()
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO reviews (job_id, agent, prompt, output, structured_output)
			VALUES (?, 'codex', 'review the diff in /tmp/snapshot.diff plus instructions', '', ?)`,
			id, string(reviewFixtureJSON("No issues found.")))
		require.NoError(t, err)
		return id
	}
	prebuilt := insertMember(true, `{"name":"security","instructions":"Focus on SQL injection."}`)
	withoutInstructions := insertMember(true, `{"name":"security"}`)
	unparseable := insertMember(true, `not json`)
	require.NoError(t, db.Close())

	for range 2 {
		db, err := Open(path)
		require.NoError(t, err)
		for id, want := range map[int64]string{
			prebuilt: "review the diff in {{diff_file}}\n\n" +
				"## Additional reviewer instructions (panel: ci / member: security)\nFocus on SQL injection.\n",
			withoutInstructions: "review the diff in {{diff_file}}",
			unparseable:         "review the diff in {{diff_file}}",
		} {
			job, err := db.GetJobByID(id)
			require.NoError(t, err)
			assert.Equal(t, want, job.Prompt, "job %d", id)
		}
		require.NoError(t, db.Close())
	}
}

func TestMigrateJobContentMarksRecoveredPromptsForSync(t *testing.T) {
	t.Parallel()
	path, db := openLegacyContentDB(t)
	repo := createRepo(t, db, t.TempDir())
	machine := testUUID("local-machine")
	synced := "2026-09-01T12:00:00Z"
	insertSyncedJob := func(label, jobPrompt, reviewPrompt string) int64 {
		result, err := db.Exec(`INSERT INTO review_jobs (uuid, repo_id, git_ref, agent, status, job_type, prompt,
				source_machine_id, updated_at, synced_at)
			VALUES (?, ?, 'a..b', 'codex', 'done', 'range', ?, ?, ?, ?)`,
			testUUID(label), repo.ID, jobPrompt, machine, synced, synced)
		require.NoError(t, err)
		id, err := result.LastInsertId()
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO reviews (job_id, agent, prompt, output, structured_output)
			VALUES (?, 'codex', ?, '', ?)`, id, reviewPrompt, string(reviewFixtureJSON("No issues found.")))
		require.NoError(t, err)
		return id
	}
	// An older client pushed this job without a prompt and stopped before
	// pushing the review, which held the only copy.
	recovered := insertSyncedJob("recovered", "", "only the review kept this prompt")
	insertSyncedJob("unchanged", "job prompt", "job prompt with preamble")
	require.NoError(t, db.Close())

	db, err := Open(path)
	require.NoError(t, err)
	defer db.Close()
	jobs, err := db.GetJobsToSync(machine, 100)
	require.NoError(t, err)
	prompts := map[int64]string{}
	for _, job := range jobs {
		prompts[job.ID] = job.Prompt
	}
	assert.Equal(t, map[int64]string{recovered: "only the review kept this prompt"}, prompts)
}
