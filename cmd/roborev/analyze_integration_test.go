//go:build integration

package main

import (
	"context"
	"io"
	"net/http"
	"os"
	"regexp"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/daemon"
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
	for _, outcome := range []string{"no change", "commit", "quiet commit"} {
		t.Run(outcome, func(t *testing.T) {
			committed := outcome != "no change"
			quiet := outcome == "quiet commit"
			resetCLIUse(t)
			cliOutputIsTerminal = func(w io.Writer) bool {
				return w == os.Stderr
			}
			t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
			repo := createTestRepo(t, map[string]string{"main.go": "package main\n"})
			before := repo.HeadSHA()
			ts, state := newMockServer(t, MockServerOpts{
				JobIDStart:     99,
				ReviewOutput:   "## CODE SMELLS\n- Found duplicated code in main.go",
				DoneAfterPolls: 2,
			})
			bodies := make(chan string, 1)
			handler := ts.Config.Handler
			ts.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == daemon.TelemetryEventsPath {
					body, _ := io.ReadAll(r.Body)
					bodies <- string(body)
					w.WriteHeader(http.StatusAccepted)
					return
				}
				handler.ServeHTTP(w, r)
			})
			patchServerAddr(t, ts.URL)
			ep := mustParseEndpoint(t, ts.URL)
			cliUseEndpoint.Store(&ep)
			seedCLIAppOpenedReceipt(t, ep, `{"sent":true}`)
			name := "analyze-fix-commit-test"
			agent.RegisterForTest(t, &agent.FakeAgent{NameStr: name, ReviewFn: func(context.Context, string, string, string, io.Writer) (string, error) {
				if committed {
					repo.CommitFile("main.go", "package main\n// fixed\n", "Fix duplicate code")
				}
				return "Done", nil
			}})
			cmd := &cobra.Command{}
			opts := analyzeOptions{agentName: name, fix: true, fixAgent: name, reasoning: "fast", quiet: quiet}
			var err error
			output, confirmation := captureStdoutStderr(t, func() {
				err = runAnalyzeAndFix(cmd, mustParseEndpoint(t, ts.URL), repo.Dir, 99, analyze.GetType("refactor"), opts)
			})
			require.NoError(t, err)
			if committed {
				assert.NotEqual(t, before, repo.HeadSHA())
			}
			assert.GreaterOrEqual(t, atomic.LoadInt32(&state.JobsCount), int32(2))
			assert.NotZero(t, atomic.LoadInt32(&state.ReviewCount))
			assert.Equal(t, int32(1), atomic.LoadInt32(&state.CloseCount))
			if quiet {
				assert.Empty(t, output)
				assert.Empty(t, confirmation)
			} else {
				assert.Contains(t, confirmation, "CODE SMELLS")
				assert.Regexp(t, regexp.MustCompile(`Analysis job \d+ closed`), confirmation)
			}
			if outcome == "commit" {
				// The action report crosses the mock daemon socket.
				require.Eventually(t, func() bool { return len(bodies) == 1 }, 5*time.Second, 10*time.Millisecond)
				require.Len(t, bodies, 1)
				assert.JSONEq(t, `{"event":"finding_fixed","properties":{"surface":"cli"}}`, <-bodies)
			} else {
				drainCLIReports()
				assert.Empty(t, bodies)
			}
		})
	}
}
