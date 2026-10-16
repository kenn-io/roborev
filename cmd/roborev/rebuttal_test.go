package main

import (
	"encoding/json/v2"
	"io"
	"net/http"
	"testing"
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
		respond                    bool
	}{
		{name: "valid", valid: true},
		{name: "respond", valid: true, respond: true},
		{name: "invalid"},
		{name: "close fails", valid: true, closeFails: true},
		{name: "stale original review", valid: true, staleOriginalReview: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetCLIUse(t)
			cliOutputIsTerminal = func(io.Writer) bool { return true }
			var closes []string
			originalReviewUUID := uuid.New()
			valid := tc.valid
			var actions []string
			daemonFromHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/telemetry/events":
					body, _ := io.ReadAll(r.Body)
					closes = append(closes, string(body))
					w.WriteHeader(http.StatusAccepted)
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
			cmd := commentCmd()
			if tc.respond {
				cmd = respondCmd()
			}
			cmd.SetArgs([]string{"42", "-m", "The caller checks it.", "--rebuttal", "--finding", "Missing guard.", "--close"})
			err := cmd.Execute()
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
