package main

import (
	"encoding/json/v2"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
)

func TestCommentRebuttalRecordsBeforeClose(t *testing.T) {
	for _, tc := range []struct {
		name                                    string
		valid, kataFailure, staleOriginalReview bool
	}{
		{name: "valid", valid: true},
		{name: "invalid"},
		{name: "Kata failure", valid: true, kataFailure: true},
		{name: "stale original review", valid: true, staleOriginalReview: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			originalReviewUUID := uuid.New()
			valid := tc.valid
			if tc.kataFailure {
				if runtime.GOOS == "windows" {
					t.Skip("shell fixture")
				}
				dir := t.TempDir()
				require.NoError(t, os.WriteFile(filepath.Join(dir, "kata"), []byte("#!/bin/sh\ncat >/dev/null\nexit 1\n"), 0o755))
				t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			}
			var actions []string
			daemonFromHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
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
				}
			}))
			cmd := commentCmd()
			args := []string{"42", "-m", "The caller checks it.", "--rebuttal", "--finding", "Missing guard.", "--close"}
			if tc.kataFailure {
				args = append(args, "--kata", "abc4", "--reply-to", "c:abc123")
			}
			cmd.SetArgs(args)
			err := cmd.Execute()
			if tc.staleOriginalReview {
				require.ErrorContains(t, err, "original review left open")
				assert.Equal(t, []string{"rebuttal", "assessment"}, actions)
			} else if tc.kataFailure {
				require.ErrorContains(t, err, "recorded locally, Kata reply failed")
				assert.Equal(t, []string{"rebuttal", "assessment"}, actions)
			} else if valid {
				require.NoError(t, err)
				assert.Equal(t, []string{"rebuttal", "assessment", "close"}, actions)
			} else {
				require.Error(t, err)
				assert.Equal(t, []string{"rebuttal"}, actions)
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
			want: "--finding, --close, --kata and --reply-to require --rebuttal",
		},
		{
			name: "close requires rebuttal",
			opts: rebuttalOptions{close: true},
			want: "--finding, --close, --kata and --reply-to require --rebuttal",
		},
		{
			name: "Kata issue requires reply target",
			opts: rebuttalOptions{enabled: true, finding: "finding", kataIssue: "abc4"},
			want: "--kata and --reply-to must be supplied together",
		},
		{
			name: "reply target requires Kata issue",
			opts: rebuttalOptions{enabled: true, finding: "finding", replyTo: "c:abc123"},
			want: "--kata and --reply-to must be supplied together",
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

func TestCommentRebuttalKataReplyWorkingDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	for _, useWorktree := range []bool{false, true} {
		name := "repository fallback"
		if useWorktree {
			name = "original worktree"
		}
		t.Run(name, func(t *testing.T) {
			repoDir := t.TempDir()
			worktreeDir := ""
			wantDir := repoDir
			if useWorktree {
				worktreeDir = t.TempDir()
				wantDir = worktreeDir
			}
			binDir := t.TempDir()
			cwdFile := filepath.Join(t.TempDir(), "cwd.txt")
			t.Setenv("KATA_REPLY_CWD", cwdFile)
			require.NoError(t, os.WriteFile(filepath.Join(binDir, "kata"), []byte("#!/bin/sh\ncat >/dev/null\npwd -P > \"$KATA_REPLY_CWD\"\n"), 0o755))
			t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			originalReviewUUID := uuid.New()
			daemonFromHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/rebuttal":
					w.WriteHeader(http.StatusCreated)
					assert.NoError(t, json.MarshalWrite(w, storage.ReviewJob{ID: 8, UUID: new(uuid.New()), OriginalReviewUUID: &originalReviewUUID, RepoPath: repoDir, WorktreePath: worktreeDir, Agent: "test"}))
				case "/api/jobs":
					assert.NoError(t, json.MarshalWrite(w, map[string]any{"jobs": []storage.ReviewJob{{ID: 8, Status: storage.JobStatusDone}}}))
				case "/api/review":
					assert.NoError(t, json.MarshalWrite(w, storage.Review{Output: `{"outcome":"refuted","explanation":"The caller checks it."}`}))
				case "/api/comment":
					w.WriteHeader(http.StatusCreated)
				}
			}))
			cmd := commentCmd()
			cmd.SetArgs([]string{"42", "-m", "The caller checks it.", "--rebuttal", "--finding", "Missing guard.", "--kata", "abc4", "--reply-to", "c:abc123"})
			require.NoError(t, cmd.Execute())
			cwd, err := os.ReadFile(cwdFile)
			require.NoError(t, err)
			physicalDir, err := filepath.EvalSymlinks(wantDir)
			require.NoError(t, err)
			assert.Equal(t, physicalDir+"\n", string(cwd))
		})
	}
}
