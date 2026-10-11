package daemon

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
)

func TestApplyRetention(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		days       int
		oldRemoved bool
	}{
		{name: "disabled keeps everything", days: 0},
		{name: "30 days removes older content", days: 30, oldRemoved: true},
		{name: "days beyond the duration range keep everything", days: 200000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
			server, db, tmpDir := newTestServer(t)
			cfg := server.configWatcher.Config()
			cfg.PromptRetentionDays = tt.days
			cfg.JobLogRetentionDays = tt.days

			repo, err := db.GetOrCreateRepo(tmpDir)
			require.NoError(t, err)
			finishedJob := func(finishedAt time.Time) int64 {
				job, err := db.EnqueueJob(storage.EnqueueOpts{RepoID: repo.ID, GitRef: "a..b", Agent: "test"})
				require.NoError(t, err)
				_, err = db.Exec(`UPDATE review_jobs SET status = 'done', finished_at = ? WHERE id = ?`,
					finishedAt.Format(time.RFC3339), job.ID)
				require.NoError(t, err)
				require.NoError(t, db.SaveJobPrompt(job.ID, "range prompt"))
				return job.ID
			}
			oldJob := finishedJob(now.AddDate(0, 0, -45))
			recentJob := finishedJob(now.AddDate(0, 0, -2))

			require.NoError(t, os.MkdirAll(JobLogDir(), 0o700))
			for id, age := range map[int64]time.Duration{oldJob: 45 * 24 * time.Hour, recentJob: time.Hour} {
				require.NoError(t, os.WriteFile(JobLogPath(id), []byte("log"), 0o600))
				stamp := time.Now().Add(-age)
				require.NoError(t, os.Chtimes(JobLogPath(id), stamp, stamp))
			}

			server.applyRetention(t.Context(), now)

			old, err := db.GetJobByID(oldJob)
			require.NoError(t, err)
			recent, err := db.GetJobByID(recentJob)
			require.NoError(t, err)
			assert.Equal(t, "range prompt", recent.Prompt)
			assert.FileExists(t, JobLogPath(recentJob))
			if tt.oldRemoved {
				assert.Empty(t, old.Prompt)
				assert.NoFileExists(t, JobLogPath(oldJob))
			} else {
				assert.Equal(t, "range prompt", old.Prompt)
				assert.FileExists(t, JobLogPath(oldJob))
			}
		})
	}
}

func TestApplyRetentionKeepsPromptsUntilSyncConnects(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	finished := now.AddDate(0, 0, -45).Format(time.RFC3339)
	tests := []struct {
		name               string
		syncEnabledAtStart bool
		syncEnabled        bool
	}{
		{name: "sync on", syncEnabledAtStart: true, syncEnabled: true},
		// The running sync worker keeps its startup settings after a reload.
		{name: "sync turned off by a reload", syncEnabledAtStart: true, syncEnabled: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
			server, db, tmpDir := newTestServer(t)
			server.syncEnabledAtStart = tt.syncEnabledAtStart
			cfg := server.configWatcher.Config()
			cfg.PromptRetentionDays = 30
			cfg.Sync.Enabled = tt.syncEnabled
			// The daemon installs its sync worker before retention starts; this
			// one has not connected yet.
			server.SetSyncWorker(storage.NewSyncWorker(db, cfg.Sync))

			repo, err := db.GetOrCreateRepo(tmpDir)
			require.NoError(t, err)
			job, err := db.EnqueueJob(storage.EnqueueOpts{RepoID: repo.ID, GitRef: "a..b", Agent: "test"})
			require.NoError(t, err)
			// Marked as pushed, possibly to a previous sync database, whose
			// markers the worker clears only once it connects.
			_, err = db.Exec(`UPDATE review_jobs SET status = 'done', finished_at = ?, updated_at = ?, synced_at = ?
				WHERE id = ?`, finished, finished, finished, job.ID)
			require.NoError(t, err)
			require.NoError(t, db.SaveJobPrompt(job.ID, "old prompt"))

			server.applyRetention(t.Context(), now)

			stored, err := db.GetJobByID(job.ID)
			require.NoError(t, err)
			assert.Equal(t, "old prompt", stored.Prompt)
		})
	}
}
