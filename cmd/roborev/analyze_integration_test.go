//go:build integration

package main

import (
	"context"
	"net/http"
	"regexp"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/prompt/analyze"
	"go.kenn.io/roborev/internal/storage"
)

func TestWaitForAnalysisJob(t *testing.T) {
	const testJobID = 42
	tests := []struct {
		name       string
		setupOpts  MockServerOpts
		wantErr    bool
		wantErrMsg string
		wantReview string
	}{
		{
			name: "immediate success",
			setupOpts: MockServerOpts{
				JobIDStart:        testJobID + 1,
				JobStatusSequence: []storage.JobStatus{storage.JobStatusDone},
				ReviewOutput:      "Analysis complete: found 3 issues",
			},
			wantReview: "Analysis complete: found 3 issues",
		},
		{
			name: "queued then done",
			setupOpts: MockServerOpts{
				JobIDStart: testJobID + 1,
				JobStatusSequence: []storage.JobStatus{
					storage.JobStatusQueued,
					storage.JobStatusRunning,
					storage.JobStatusDone,
				},
				ReviewOutput: "All good",
			},
			wantReview: "All good",
		},
		{
			name: "job failed",
			setupOpts: MockServerOpts{
				JobIDStart:        testJobID + 1,
				JobStatusSequence: []storage.JobStatus{storage.JobStatusFailed},
				JobError:          "agent crashed",
			},
			wantErr:    true,
			wantErrMsg: "agent crashed",
		},
		{
			name: "job canceled",
			setupOpts: MockServerOpts{
				JobIDStart:        testJobID + 1,
				JobStatusSequence: []storage.JobStatus{storage.JobStatusCanceled},
			},
			wantErr:    true,
			wantErrMsg: "canceled",
		},
		{
			name: "job not found",
			setupOpts: MockServerOpts{
				JobIDStart:  testJobID + 1,
				JobNotFound: true,
			},
			wantErr:    true,
			wantErrMsg: "not found",
		},
		{
			name: "empty responses",
			setupOpts: MockServerOpts{
				JobIDStart: testJobID + 1,
				OnJobs: func(w http.ResponseWriter, r *http.Request) {
					http.Error(w, "no mock responses configured", http.StatusInternalServerError)
				},
			},
			wantErr:    true,
			wantErrMsg: "no mock responses configured",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts, _ := newMockServer(t, tt.setupOpts)

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			review, err := waitForAnalysisJob(ctx, mustParseEndpoint(t, ts.URL), testJobID)

			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErrMsg)
				return
			}

			require.NoError(t, err)
			require.NotNil(t, review, "expected review, got nil")
			assert.Equal(t, tt.wantReview, review.Output)
		})
	}
}

func TestRunAnalyzeAndFix_Integration(t *testing.T) {
	// This tests the full workflow with mocked daemon and test agent
	repo := createTestRepo(t, map[string]string{
		"main.go": "package main\n",
	})

	ts, state := newMockServer(t, MockServerOpts{
		JobIDStart:     99,
		ReviewOutput:   "## CODE SMELLS\n- Found duplicated code in main.go",
		DoneAfterPolls: 2,
	})

	cmd, output := newTestCmd(t)

	analysisType := analyze.GetType("refactor")
	opts := analyzeOptions{
		agentName: "test",
		fix:       true,
		fixAgent:  "test",
		reasoning: "fast",
	}

	err := runAnalyzeAndFix(cmd, mustParseEndpoint(t, ts.URL), repo.Dir, 99, analysisType, opts)
	require.NoError(t, err, "runAnalyzeAndFix failed: %v")

	// Verify the workflow was executed
	assert.GreaterOrEqual(t, atomic.LoadInt32(&state.JobsCount), int32(2))
	assert.NotZero(t, atomic.LoadInt32(&state.ReviewCount))
	assert.NotZero(t, atomic.LoadInt32(&state.CloseCount))

	// Verify output contains analysis result
	outputStr := output.String()
	assert.Contains(t, outputStr, "CODE SMELLS")
	assert.Regexp(t, regexp.MustCompile(`Analysis job \d+ closed`), outputStr)
}
