package main

import (
	"encoding/json/v2"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
)

func TestCommentRebuttalRecordsBeforeClose(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		valid, staleOriginalReview bool
		closeFails                 bool
	}{
		{name: "valid", valid: true},
		{name: "invalid"},
		{name: "close fails", valid: true, closeFails: true},
		{name: "stale original review", valid: true, staleOriginalReview: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetCLIUse(t)
			var closes []string
			var mu sync.Mutex
			originalReviewUUID := uuid.New()
			valid := tc.valid
			var actions []string
			daemonFromHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/telemetry/events":
					body, _ := io.ReadAll(r.Body)
					if strings.Contains(string(body), `"review_closed"`) {
						mu.Lock()
						closes = append(closes, string(body))
						mu.Unlock()
					}
					w.WriteHeader(http.StatusAccepted)
					_, _ = io.WriteString(w, `{"status":"queued"}`)
				case "/api/rebuttal":
					actions = append(actions, "rebuttal")
					w.WriteHeader(http.StatusCreated)
					j := storage.ReviewJob{ID: 8, UUID: new(uuid.New()), RepoPath: t.TempDir(), Agent: "test"}
					j.OriginalReviewUUID = &originalReviewUUID
					assert.NoError(t, json.MarshalWrite(w, j))
				case "/api/jobs":
					assert.NoError(t, json.MarshalWrite(w, map[string]any{"jobs": []storage.ReviewJob{{ID: 8, Status: storage.JobStatusDone}}}))
				case "/api/review":
					output := `{"outcome":"refuted","explanation":"Verified the caller checks this input before reaching the path."}`
					if !valid {
						output = "not an assessment"
					}
					assert.NoError(t, json.MarshalWrite(w, storage.Review{Output: output}))
				case "/api/comment":
					var request map[string]any
					assert.NoError(t, json.UnmarshalRead(r.Body, &request))
					assert.Equal(t, originalReviewUUID.String(), request["expected_review_uuid"])
					actions = append(actions, "assessment")
					if tc.staleOriginalReview {
						w.WriteHeader(http.StatusConflict)
						return
					}
					w.WriteHeader(http.StatusCreated)
				case "/api/review/close":
					var request map[string]any
					assert.NoError(t, json.UnmarshalRead(r.Body, &request))
					assert.Equal(t, originalReviewUUID.String(), request["expected_review_uuid"])
					actions = append(actions, "close")
					if tc.closeFails {
						w.WriteHeader(http.StatusConflict)
					}
				}
			}))
			cmd := newRootCmd()
			cmd.SetArgs([]string{"comment", "42", "-m", "The caller checks it.", "--rebuttal", "--finding", "Missing guard.", "--close"})
			var err error
			captureStdoutStderr(t, func() {
				cliOutputIsTerminal = func(out io.Writer) bool { return out == os.Stderr }
				err = cmd.Execute()
			})
			if tc.staleOriginalReview {
				require.ErrorContains(t, err, "original review left open")
				assert.Equal(t, []string{"rebuttal", "assessment"}, actions)
			} else if tc.closeFails {
				require.ErrorContains(t, err, "close failed")
			} else if valid {
				require.NoError(t, err)
				assert.Equal(t, []string{"rebuttal", "assessment", "close"}, actions)
			} else {
				require.Error(t, err)
				assert.Equal(t, []string{"rebuttal"}, actions)
			}
			if valid && !tc.staleOriginalReview && !tc.closeFails {
				// The close report crosses the mock daemon socket.
				require.Eventually(t, func() bool {
					mu.Lock()
					defer mu.Unlock()
					return len(closes) == 1
				}, 5*time.Second, 10*time.Millisecond)
			} else {
				drainCLIReports()
			}
			mu.Lock()
			defer mu.Unlock()
			if valid && !tc.staleOriginalReview && !tc.closeFails {
				require.Len(t, closes, 1)
				assert.JSONEq(t, `{"event":"review_closed","properties":{"surface":"cli"}}`, closes[0])
			} else {
				assert.Empty(t, closes)
			}
		})
	}
}

func TestCommentRebuttalRejectsIncompleteOptions(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts rebuttalOptions
		want string
	}{
		{
			name: "rebuttal requires a finding",
			opts: rebuttalOptions{enabled: true},
			want: "--rebuttal requires --finding with an excerpt of the original review",
		},
		{
			name: "finding requires rebuttal",
			opts: rebuttalOptions{finding: "finding"},
			want: "--finding and --close require --rebuttal",
		},
		{
			name: "close requires rebuttal",
			opts: rebuttalOptions{close: true},
			want: "--finding and --close require --rebuttal",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.opts.validate()
			require.EqualError(t, err, tc.want)
		})
	}
}

func TestCommentInvalidJobIDNamesRequestedMode(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"abc123", "-m", "explanation", "--rebuttal", "--finding", "finding"}, "--rebuttal requires a numeric review job ID"},
	} {
		cmd := commentCmd()
		cmd.SetArgs(tc.args)
		require.ErrorContains(t, cmd.Execute(), tc.want)
	}
}
