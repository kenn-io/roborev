package daemon

import (
	"context"
	"io"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

type workerSessionSpyState struct {
	mu              sync.Mutex
	observedSession string
}

type workerSessionSpy struct {
	name      string
	sessionID string
	state     *workerSessionSpyState
}

func (a *workerSessionSpy) Name() string { return a.name }
func (a *workerSessionSpy) Review(_ context.Context, _, _, _ string, output io.Writer) (string, error) {
	a.state.mu.Lock()
	a.state.observedSession = a.sessionID
	a.state.mu.Unlock()
	if _, err := io.WriteString(output, `{"type":"init","session_id":"new-gemini-session"}`+"\n"); err != nil {
		return "", err
	}
	return string(testutil.ReviewFixtureJSON("No issues found.")), nil
}
func (a *workerSessionSpy) WithReasoning(agent.ReasoningLevel) agent.Agent { return a }
func (a *workerSessionSpy) WithAgentic(bool) agent.Agent                   { return a }
func (a *workerSessionSpy) WithModel(string) agent.Agent                   { return a }
func (a *workerSessionSpy) CommandLine() string                            { return a.name }
func (a *workerSessionSpy) SessionCLI() string                             { return "gemini" }
func (a *workerSessionSpy) WithSessionID(sessionID string) agent.Agent {
	return &workerSessionSpy{name: a.name, sessionID: sessionID, state: a.state}
}

func TestWorkerPassesOnlyMatchingGeminiSession(t *testing.T) {
	t.Parallel()
	for _, storedCLI := range []string{"agy", "gemini"} {
		t.Run(storedCLI, func(t *testing.T) {
			t.Parallel()
			worker := newWorkerTestContext(t, 1)
			state := &workerSessionSpyState{}
			name := "worker-session-" + storedCLI
			agent.RegisterForTest(t, &workerSessionSpy{name: name, state: state})

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
			assert.Nil(t, got.ResumeSourceJobUUID)
		})
	}
}
