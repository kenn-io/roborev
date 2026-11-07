package storage

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/require"
)

func BenchmarkPulledRows(b *testing.B) {
	template, err := getTemplatePath()
	require.NoError(b, err)
	data, err := os.ReadFile(template)
	require.NoError(b, err)
	now := time.Now().UTC().Truncate(time.Microsecond)
	jobs := make([]PulledJob, 1000)
	reviews := make([]PulledReview, len(jobs))
	responses := make([]PulledResponse, len(jobs))
	owner := uuid.New()
	for i := range jobs {
		jobs[i] = PulledJob{
			UUID: uuid.New(), RepoIdentity: "https://example.com/repo.git",
			CommitSHA: fmt.Sprintf("%040x", i), CommitTimestamp: now,
			GitRef: "main", Agent: "test", JobType: JobTypeReview, Status: "done",
			EnqueuedAt: now, UpdatedAt: now, SourceMachineID: owner,
		}
		reviews[i] = PulledReview{
			UUID: uuid.New(), JobUUID: jobs[i].UUID, Agent: "test",
			JobEnqueuedAt: now, JobSourceMachineID: owner,
			StructuredOutput: reviewFixtureJSON("pulled finding"),
			CreatedAt:        now, UpdatedAt: now, UpdatedByMachineID: owner,
		}
		responses[i] = PulledResponse{
			UUID: uuid.New(), JobUUID: jobs[i].UUID, Responder: "user", Response: "fixed",
			CreatedAt: now, SourceMachineID: owner,
		}
	}
	dir := b.TempDir()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		b.StopTimer()
		path := filepath.Join(dir, fmt.Sprintf("%d.db", n))
		require.NoError(b, os.WriteFile(path, data, 0o600))
		db, err := Open(path)
		require.NoError(b, err)
		worker := NewSyncWorker(db, testSyncConfig())
		session, err := db.newPullSession(b.Context())
		require.NoError(b, err)
		b.StartTimer()
		for i := range jobs {
			require.NoError(b, worker.pullJob(b.Context(), session, jobs[i]))
			if (i+1)%100 == 0 {
				require.NoError(b, session.commit(b.Context()))
			}
		}
		for i := range reviews {
			require.NoError(b, worker.pullReview(b.Context(), session, reviews[i]))
			if (i+1)%100 == 0 {
				require.NoError(b, session.commit(b.Context()))
			}
		}
		for i := range responses {
			require.NoError(b, worker.pullResponse(b.Context(), session, responses[i]))
			if (i+1)%100 == 0 {
				require.NoError(b, session.commit(b.Context()))
			}
		}
		b.StopTimer()
		session.close()
		require.NoError(b, db.Close())
		require.NoError(b, os.Remove(path))
		b.StartTimer()
	}
}
