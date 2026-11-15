package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/storage"
)

func TestWorkerPassesOnlyMatchingGeminiSession(t *testing.T) {
	t.Parallel()
	geminiStream, err := os.ReadFile(filepath.Join("..", "agent", "testdata", "gemini-stream.jsonl"))
	require.NoError(t, err)
	geminiInit, _, found := strings.Cut(string(geminiStream), "\n")
	require.True(t, found)
	geminiInit = strings.Replace(geminiInit, "d91bc4f1-01d9-4c9f-9b07-23ae0ecb39f2", "new-gemini-session", 1)
	for _, storedCLI := range []string{"agy", "gemini"} {
		t.Run(storedCLI, func(t *testing.T) {
			t.Parallel()
			worker := newWorkerTestContext(t, 1)
			state := &sessionStreamingTestAgentState{}
			name := "worker-session-" + storedCLI
			agent.RegisterForTest(t, &sessionStreamingTestAgent{
				name: name, streamLine: geminiInit,
				sessionCLI: "gemini", state: state,
			})

			job := worker.createJobWithAgent(t, worker.GitRepo.HeadSHA(), name)
			_, err := worker.DB.ExecContext(t.Context(), `
				UPDATE review_jobs
				SET session_id = 'old-gemini-session', session_cli = ?, session_resumed = 1,
				    resume_source_job_uuid = '123e4567-e89b-12d3-a456-426614174000'
				WHERE id = ?`, storedCLI, job.ID)
			require.NoError(t, err)
			claimed, err := worker.DB.ClaimJob(testWorkerID)
			require.NoError(t, err)
			require.Equal(t, job.ID, claimed.ID)

			worker.Pool.processJob(testWorkerID, claimed)

			got := worker.assertJobStatus(t, job.ID, storage.JobStatusDone)
			wantIncoming := "old-gemini-session"
			if storedCLI != "gemini" {
				wantIncoming = ""
			}
			state.mu.Lock()
			observedSession := state.observedSession
			state.mu.Unlock()
			assert.Equal(t, wantIncoming, observedSession)
			assert.Equal(t, "new-gemini-session", got.SessionID)
			assert.Equal(t, "gemini", got.SessionCLI)
		})
	}
}
