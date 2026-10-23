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

	"go.kenn.io/roborev/internal/findingid"
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
			want: "--rebuttal requires --finding with an excerpt of the original review, or --finding-id with a finding selector",
		},
		{
			name: "finding requires rebuttal",
			opts: rebuttalOptions{finding: "finding"},
			want: "--finding, --finding-id and --close require --rebuttal",
		},
		{
			name: "finding selector requires rebuttal",
			opts: rebuttalOptions{findingID: "f:abc"},
			want: "--finding, --finding-id and --close require --rebuttal",
		},
		{
			name: "close requires rebuttal",
			opts: rebuttalOptions{close: true},
			want: "--finding, --finding-id and --close require --rebuttal",
		},
		{
			name: "an excerpt and a selector are alternatives",
			opts: rebuttalOptions{enabled: true, finding: "finding", findingID: "f:abc"},
			want: "--finding and --finding-id select the finding two ways; use one",
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

func TestCommentRebuttalSelectsAFindingBySelector(t *testing.T) {
	resetCLIUse(t)
	findings := sampleFindings()
	originalReviewUUID := uuid.New()
	var (
		mu       sync.Mutex
		rebuttal map[string]any
		recorded string
	)
	record := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		assert.NoError(t, json.UnmarshalRead(r.Body, &request))
		mu.Lock()
		recorded, _ = request["comment"].(string)
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	})
	queue := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		assert.NoError(t, json.UnmarshalRead(r.Body, &rebuttal))
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		job := storage.ReviewJob{ID: 8, UUID: new(uuid.New()), RepoPath: t.TempDir(), Agent: "test", OriginalReviewUUID: &originalReviewUUID}
		assert.NoError(t, json.MarshalWrite(w, job))
	})
	list := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "42", r.URL.Query().Get("job_id"))
		assert.NoError(t, json.MarshalWrite(w, findings))
	})
	NewMockDaemon(t, MockRefineHooks{
		OnComment: func(w http.ResponseWriter, r *http.Request, _ *mockRefineState) bool {
			record.ServeHTTP(w, r)
			return true
		},
		OnGetJobs: func(w http.ResponseWriter, _ *http.Request, _ *mockRefineState) bool {
			_ = json.MarshalWrite(w, map[string]any{"jobs": []storage.ReviewJob{{ID: 8, Status: storage.JobStatusDone}}})
			return true
		},
		OnReview: func(w http.ResponseWriter, _ *http.Request, _ *mockRefineState) bool {
			_ = json.MarshalWrite(w, storage.Review{Output: `{"outcome":"confirmed","explanation":"Verified the value is nil on that path."}`})
			return true
		},
		OnUnhandled: func(w http.ResponseWriter, r *http.Request, _ *mockRefineState) bool {
			switch r.URL.Path {
			case "/api/review/findings":
				list.ServeHTTP(w, r)
			case "/api/rebuttal":
				queue.ServeHTTP(w, r)
			default:
				return false
			}
			return true
		},
	})

	cmd := newRootCmd()
	cmd.SetArgs([]string{"comment", "42", "-m", "It cannot be nil here.", "--rebuttal", "--finding-id", findings.Findings[1].ID})
	var err error
	captureStdoutStderr(t, func() { err = cmd.Execute() })
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, findings.Findings[1].ID, rebuttal["finding_id"], "the daemon gets the full selector")
	assert.NotContains(t, rebuttal, "finding", "a selector and an excerpt are not sent together")
	assert.Contains(t, recorded, "Problem: Typo.", "the recorded assessment quotes the finding that was selected")
	assert.Contains(t, recorded, "Fix: Spell it.")
	assert.NotContains(t, recorded, "Nil dereference.")
}

func TestCommentRebuttalRequiresACompleteSelector(t *testing.T) {
	complete := sampleFindings().Findings[0].ID
	require.NoError(t, rebuttalOptions{enabled: true, findingID: complete}.validate())
	require.NoError(t, rebuttalOptions{enabled: true, findingID: strings.ToUpper(complete)}.validate(), "capitals are read as lower case")
	require.NoError(t, rebuttalOptions{enabled: true, findingID: " " + complete + "\n"}.validate(), "surrounding space from a paste is not part of the selector")

	for name, selector := range map[string]string{
		"shortened":   complete[:len(complete)-4],
		"bare prefix": "f:",
		"an excerpt":  "Missing guard.",
		"split":       complete[:10] + " " + complete[10:],
	} {
		err := rebuttalOptions{enabled: true, findingID: selector}.validate()
		require.ErrorIs(t, err, findingid.ErrInvalid, name)
		require.ErrorContains(t, err, "complete selector copied from 'roborev show'", name)
	}
}

func TestCommentRebuttalRejectsAnUnusableSelectorWithoutQueueing(t *testing.T) {
	findings := sampleFindings()
	foreign, err := findingid.New(uuid.New(), 0, strings.Repeat("cd", 32))
	require.NoError(t, err)
	for name, tc := range map[string]struct {
		selector, want string
		listed         bool
	}{
		"shortened":                           {findings.Findings[0].ID[:len(findings.Findings[0].ID)-4], "complete selector", false},
		"not selector text":                   {"Typo.", "complete selector", false},
		"well formed, but not in this review": {foreign.String(), "no finding matches", true},
	} {
		t.Run(name, func(t *testing.T) {
			resetCLIUse(t)
			var listings, rebuttals int
			list := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				listings++
				assert.NoError(t, json.MarshalWrite(w, findings))
			})
			NewMockDaemon(t, MockRefineHooks{
				OnUnhandled: func(w http.ResponseWriter, r *http.Request, _ *mockRefineState) bool {
					switch r.URL.Path {
					case "/api/review/findings":
						list.ServeHTTP(w, r)
					case "/api/rebuttal":
						rebuttals++
					default:
						return false
					}
					return true
				},
			})
			cmd := newRootCmd()
			cmd.SetArgs([]string{"comment", "42", "-m", "Explanation", "--rebuttal", "--finding-id", tc.selector})
			var runErr error
			captureStdoutStderr(t, func() { runErr = cmd.Execute() })
			require.ErrorContains(t, runErr, tc.want)
			assert.Zero(t, rebuttals, "nothing is queued for a selector the CLI cannot resolve")
			if tc.listed {
				assert.Equal(t, 1, listings)
			} else {
				assert.Zero(t, listings, "a malformed selector is refused before the daemon is asked")
			}
		})
	}
}
