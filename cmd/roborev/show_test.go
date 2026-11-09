package main

// Tests for the show command

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
	"go.kenn.io/roborev/internal/version"
)

func TestShowOpenFailedReviewHint(t *testing.T) {
	for _, tc := range []struct {
		name    string
		closed  bool
		verdict *int
		jobType string
	}{
		{name: "closed failed", closed: true, verdict: new(0), jobType: storage.JobTypeReview},
		{name: "open passing", verdict: new(1), jobType: storage.JobTypeReview},
		{name: "unknown verdict", jobType: storage.JobTypeReview},
		{name: "fix output", verdict: new(0), jobType: storage.JobTypeFix},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Nil(t, openReviewHint(storage.Review{
				JobID: 7, Closed: tc.closed, VerdictBool: tc.verdict,
				Job: &storage.ReviewJob{ID: 7, JobType: tc.jobType},
			}, "roborev"))
		})
	}
	for _, tc := range []struct {
		name     string
		prompt   bool
		wantHint bool
	}{
		{name: "open failed", wantHint: true},
		{name: "prompt", prompt: true},
	} {
		for _, jsonOutput := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/json=%t", tc.name, jsonOutput), func(t *testing.T) {
				isolateDaemonSelection(t)
				mockReviewDaemon(t, storage.Review{
					JobID: 7, Agent: "test", Output: "Finding", Prompt: "Instructions",
					VerdictBool: new(0),
					Job:         &storage.ReviewJob{ID: 7, JobType: storage.JobTypeReview},
				})
				ep := getDaemonEndpoint()
				getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) {
					return &daemon.RuntimeInfo{Network: ep.Network, Address: ep.Address, Version: version.Version}, nil
				}
				args := []string{"job", "show", "--job", "7"}
				content := "Finding"
				if tc.prompt {
					args = append(args, "--prompt")
					content = "Instructions"
				}
				if jsonOutput {
					args = append(args, "--json")
				}
				run := runCLI(t, args...)
				require.NoError(t, run.err)
				if jsonOutput {
					assert.Empty(t, run.stderr)
					var result map[string]json.RawMessage
					require.NoError(t, json.Unmarshal([]byte(run.stdout), &result))
					if tc.wantHint {
						assert.JSONEq(t, `[{"code":"review_open","message":"review 7 is open. After its findings are fixed or answered, record what changed and close it","next":"roborev job comment --job 7 -m \"<what changed>\" && roborev job close 7"}]`, string(result["hints"]))
					} else {
						assert.NotContains(t, result, "hints")
					}
					return
				}
				assert.Equal(t, "Review for job 7 (by test)\n------------------------------------------------------------\n"+content+"\n", run.stdout)
				if tc.wantHint {
					assert.Equal(t, "hint: review 7 is open. After its findings are fixed or answered, record what changed and close it: roborev job comment --job 7 -m \"<what changed>\" && roborev job close 7\n", run.stderr)
				} else {
					assert.Empty(t, run.stderr)
				}
			})
		}
	}
}

func TestShowHintFollowupTargetsSelectedDaemon(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("commands use the Unix shell on this platform")
	}
	for _, network := range []string{"tcp", "unix"} {
		t.Run(network, func(t *testing.T) {
			isolateDaemonSelection(t)
			selectedWrites, defaultWrites := make(chan string, 2), make(chan string, 2)
			handler := func(name string, writes chan<- string) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/api/ping":
						writeJSON(w, map[string]any{"ok": true, "service": "roborev", "version": version.Version})
					case "/api/review":
						assert.Equal(t, "7", r.URL.Query().Get("job_id"))
						writeJSON(w, storage.Review{JobID: 7, Output: name, VerdictBool: new(0), Job: &storage.ReviewJob{ID: 7, JobType: storage.JobTypeReview}})
					case "/api/comment", "/api/review/close":
						writes <- r.URL.Path
						if r.URL.Path == "/api/comment" {
							w.WriteHeader(http.StatusCreated)
						}
						writeJSON(w, map[string]any{})
					default:
						writeJSON(w, map[string]any{})
					}
				})
			}
			defaultServer := httptest.NewServer(handler("unrelated review", defaultWrites))
			t.Cleanup(defaultServer.Close)
			getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) {
				return &daemon.RuntimeInfo{Network: "tcp", Address: strings.TrimPrefix(defaultServer.URL, "http://"), Version: version.Version}, nil
			}
			selected := httptest.NewUnstartedServer(handler("selected review", selectedWrites))
			addr := ""
			if network == "unix" {
				// Keep the socket below the Unix path length limit on macOS too.
				dir, err := os.MkdirTemp("/tmp", "rr-show-*")
				require.NoError(t, err)
				t.Cleanup(func() { assert.NoError(t, os.RemoveAll(dir)) })
				addr = filepath.Join(dir, "daemon's socket")
				listener, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", addr)
				require.NoError(t, err)
				require.NoError(t, selected.Listener.Close())
				selected.Listener = listener
				addr = "unix://" + addr
			}
			selected.Start()
			t.Cleanup(selected.Close)
			if network == "tcp" {
				addr = selected.URL
			}
			args := []string{"--server", addr, "job", "show", "--job", "7"}
			run := runCLI(t, args...)
			require.NoError(t, run.err)
			require.Contains(t, run.stdout, "selected review")
			parts := strings.SplitN(run.stderr, ": roborev ", 2)
			require.Len(t, parts, 2)
			next := "roborev " + strings.TrimSpace(parts[1])
			commands := strings.Split(next, " && ")
			require.Len(t, commands, 2)
			for _, command := range commands {
				parsed, err := exec.CommandContext(t.Context(), "sh", "-c", `roborev() { printf '%s\000' "$@"; }; `+command).Output()
				require.NoError(t, err)
				followup := runCLI(t, strings.Split(strings.TrimSuffix(string(parsed), "\x00"), "\x00")...)
				require.NoError(t, followup.err)
			}
			assert.Empty(t, defaultWrites)
			require.Len(t, selectedWrites, 2)
			assert.Equal(t, "/api/comment", <-selectedWrites)
			assert.Equal(t, "/api/review/close", <-selectedWrites)
		})
	}
}

func TestShowCommandArgParsing(t *testing.T) {
	tests := []struct {
		name         string
		args         []string
		setupRepo    func(*TestGitRepo)
		wantQueryHas []string
		wantQueryNot []string
		validate     func(t *testing.T, repo *TestGitRepo, query string)
	}{
		{
			name: "numeric ref resolvable in repo treated as SHA not job ID",
			args: []string{"12345"},
			setupRepo: func(r *TestGitRepo) {
				r.Run("tag", "12345")
			},
			wantQueryHas: []string{"sha="},
			wantQueryNot: []string{"job_id="},
			validate: func(t *testing.T, repo *TestGitRepo, query string) {
				tagSHA := repo.Run("rev-parse", "12345")
				assert.Contains(t, query, tagSHA[:7])
			},
		},
		{
			name:         "numeric non-resolvable treated as job ID",
			args:         []string{"99999"},
			wantQueryHas: []string{"job_id=99999"},
			wantQueryNot: []string{"sha="},
		},
		{
			name:         "non-numeric argument treated as SHA",
			args:         []string{"abc123def"},
			wantQueryHas: []string{"sha=abc123def"},
			wantQueryNot: []string{"job_id="},
		},
		{
			name: "flag --job forces job ID interpretation even when ref is resolvable",
			args: []string{"--job", "12345"},
			setupRepo: func(r *TestGitRepo) {
				r.Run("tag", "12345")
			},
			wantQueryHas: []string{"job_id=12345"},
			wantQueryNot: []string{"sha="},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := newTestGitRepo(t)
			repo.CommitFile("file.txt", "content", "initial commit")
			if tt.setupRepo != nil {
				tt.setupRepo(repo)
			}

			getQuery := mockReviewDaemon(t, storage.Review{
				VerdictBool: testutil.ReviewFixtureVerdict("LGTM"),
				ID:          1, JobID: 42, Output: "LGTM", Agent: "test",
			})

			chdir(t, repo.Dir)
			_ = runShowCmd(t, tt.args...)

			q := getQuery()
			for _, s := range tt.wantQueryHas {
				assert.Contains(t, q, s)
			}
			for _, s := range tt.wantQueryNot {
				assert.NotContains(t, q, s)
			}

			if tt.validate != nil {
				tt.validate(t, repo, q)
			}
		})
	}
}

func TestShowOutputFormat(t *testing.T) {
	tests := []struct {
		name           string
		argsFunc       func(sha string) []string
		wantOutputFunc func(shortSHA string) string
		notOutput      string
	}{
		{
			name:           "job ID shows 'Review for job X (by agent)'",
			argsFunc:       func(sha string) []string { return []string{"--job", "42"} },
			wantOutputFunc: func(shortSHA string) string { return "Review for job 42 (by codex)" },
			notOutput:      "(job 42, by",
		},
		{
			name:           "SHA shows 'Review for abc123 (job X, by agent)'",
			argsFunc:       func(sha string) []string { return []string{sha} },
			wantOutputFunc: func(shortSHA string) string { return fmt.Sprintf("Review for %s (job 42, by codex)", shortSHA) },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := newTestGitRepo(t)
			commitSHA := repo.CommitFile("file.txt", "content", "initial commit")
			shortSHA := commitSHA[:7]

			mockReviewDaemon(t, storage.Review{
				VerdictBool: testutil.ReviewFixtureVerdict("Test review output"),
				ID:          1, JobID: 42, Output: "Test review output", Agent: "codex",
			})

			chdir(t, repo.Dir)

			args := tt.argsFunc(commitSHA)
			output := runShowCmd(t, args...)

			expected := tt.wantOutputFunc(shortSHA)

			assert.Contains(t, output, expected)
			if tt.notOutput != "" {
				assert.NotContains(t, output, tt.notOutput)
			}
		})
	}
}

func TestShowTokenUsageSummary(t *testing.T) {
	for _, tc := range []struct {
		name, usage, want string
	}{
		{"sessions only", `{"cost_usd":0,"provider_session_ids":["plan","fix"],"expected_provider_sessions":2}`, ""},
		{"priced", `{"cost_usd":0.42,"has_cost":true}`, "Tokens: ~$0.42"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mockReviewDaemon(t, storage.Review{
				ID: 1, JobID: 42, Output: "Fix completed", Agent: "test",
				Job: &storage.ReviewJob{ID: 42, TokenUsage: tc.usage},
			})
			output := runShowCmd(t, "--job", "42")
			if tc.want == "" {
				assert.NotContains(t, output, "Tokens:")
			} else {
				assert.Contains(t, output, tc.want)
			}
		})
	}
}

func TestShowOutsideGitRepo(t *testing.T) {
	t.Run("no args outside git repo returns guidance error", func(t *testing.T) {
		nonGitDir := t.TempDir()
		chdir(t, nonGitDir)

		mockReviewDaemon(t, storage.Review{})

		cmd := showCmd()
		cmd.SetArgs([]string{})
		err := cmd.Execute()
		assertErrorContains(t, err, "not in a git repository")
		assertErrorContains(t, err, "job ID")
	})

	t.Run("job ID outside git repo still works", func(t *testing.T) {
		nonGitDir := t.TempDir()
		chdir(t, nonGitDir)

		getQuery := mockReviewDaemon(t, storage.Review{
			VerdictBool: testutil.ReviewFixtureVerdict("LGTM"),
			ID:          1, JobID: 42, Output: "LGTM", Agent: "test",
		})

		output := runShowCmd(t, "--job", "42")
		q := getQuery()
		assert.Contains(t, q, "job_id=42")
		assert.Contains(t, output, "LGTM")
	})
}

func TestShowJSONOutput(t *testing.T) {
	repo := newTestGitRepo(t)
	repo.CommitFile("file.txt", "content", "initial commit")

	mockReviewDaemon(t, storage.Review{
		WebURL:      "https://reviews.example/team/reviews/42",
		VerdictBool: testutil.ReviewFixtureVerdict("LGTM"),
		ID:          1, JobID: 42, Output: "LGTM", Agent: "test",
	})

	chdir(t, repo.Dir)

	output := runShowCmd(t, "--job", "42", "--json")

	t.Run("outputs valid JSON", func(t *testing.T) {
		var parsed storage.Review
		err := json.Unmarshal([]byte(output), &parsed)
		require.NoError(t, err, "invalid JSON output")
		assert.EqualValues(t, 42, parsed.JobID)
		assert.Equal(t, "LGTM", parsed.Output)
		assert.Equal(t, "test", parsed.Agent)
		assert.Equal(t, "https://reviews.example/team/reviews/42", parsed.WebURL)
	})

	t.Run("skips formatted header", func(t *testing.T) {
		assert.NotContains(t, output, "Review for")
		assert.NotContains(t, output, "---")
	})
}

func TestShowIncludesComments(t *testing.T) {
	repo := newTestGitRepo(t)
	repo.CommitFile("file.txt", "content", "initial commit")

	review := storage.Review{
		VerdictBool: testutil.ReviewFixtureVerdict("Found issues"),
		ID:          1, JobID: 42, Output: "Found issues", Agent: "test",
	}
	responses := []storage.Response{
		{
			ID:        1,
			Responder: "alice",
			Response:  "This is expected",
			CreatedAt: time.Date(2025, 6, 1, 10, 30, 0, 0, time.UTC),
		},
	}

	daemonFromHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/review":
			json.NewEncoder(w).Encode(review)
		case "/api/comments":
			json.NewEncoder(w).Encode(map[string]any{
				"responses": responses,
			})
		}
	}))

	chdir(t, repo.Dir)
	output := runShowCmd(t, "--job", "42")

	assert.Contains(t, output, "Found issues")
	assert.Contains(t, output, "--- Comments ---")
	assert.Contains(t, output, "alice")
	assert.Contains(t, output, "This is expected")
}

func TestShowDirtyReviewSkipsBaseCommitLegacyComments(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "text", args: []string{"--job", "42"}},
		{name: "json", args: []string{"--job", "42", "--json"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			repo := newTestGitRepo(t)
			repo.CommitFile("file.txt", "content", "initial commit")
			commitID := int64(42)
			review := storage.Review{
				VerdictBool: testutil.ReviewFixtureVerdict("Dirty review output"),
				ID:          1, JobID: 42, Output: "Dirty review output", Agent: "test",
				Job: &storage.ReviewJob{
					ID:       42,
					CommitID: &commitID,
					GitRef:   "dirty",
					JobType:  storage.JobTypeDirty,
				},
			}
			var sawLegacy bool
			daemonFromHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/review":
					assert.NoError(json.NewEncoder(w).Encode(review))
				case "/api/comments":
					switch r.URL.RawQuery {
					case "job_id=42":
						assert.NoError(json.NewEncoder(w).Encode(map[string]any{
							"responses": []storage.Response{{
								ID:        1,
								Responder: "alice",
								Response:  "dirty job comment",
								CreatedAt: time.Date(2026, 6, 1, 10, 30, 0, 0, time.UTC),
							}},
						}))
					case "commit_id=42":
						sawLegacy = true
						assert.NoError(json.NewEncoder(w).Encode(map[string]any{
							"responses": []storage.Response{{
								ID:        2,
								Responder: "bob",
								Response:  "base commit comment",
								CreatedAt: time.Date(2026, 6, 1, 10, 31, 0, 0, time.UTC),
							}},
						}))
					default:
						http.Error(w, "unexpected query: "+r.URL.RawQuery, http.StatusBadRequest)
					}
				}
			}))

			chdir(t, repo.Dir)
			output := runShowCmd(t, tt.args...)

			assert.False(sawLegacy, "dirty show must not fetch legacy comments for the base commit")
			assert.Contains(output, "dirty job comment")
			assert.NotContains(output, "base commit comment")
		})
	}
}

func TestShowPromptForJob(t *testing.T) {
	for _, status := range []storage.JobStatus{storage.JobStatusQueued, storage.JobStatusRunning, storage.JobStatusFailed, storage.JobStatusCanceled} {
		t.Run(string(status), func(t *testing.T) {
			daemonFromHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/review":
					http.Error(w, "not found", http.StatusNotFound)
				case "/api/jobs":
					assert.Equal(t, "id=42", r.URL.RawQuery)
					json.NewEncoder(w).Encode(map[string]any{"jobs": []storage.ReviewJob{{ID: 42, Agent: "codex", Status: status, Prompt: "Review this diff please"}}})
				default:
					http.Error(w, "unexpected path: "+r.URL.Path, http.StatusBadRequest)
				}
			}))
			output := runShowCmd(t, "--job", "42", "--prompt")
			assert.Contains(t, output, "\nReview this diff please\n")
			assert.Contains(t, output, "codex")
		})
	}

	daemonFromHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/review":
			http.Error(w, "not found", http.StatusNotFound)
		case "/api/jobs":
			json.NewEncoder(w).Encode(map[string]any{"jobs": []storage.ReviewJob{{ID: 42, Agent: "codex", Status: storage.JobStatusFailed, Prompt: "Review this diff please"}}})
		default:
			http.Error(w, "unexpected path: "+r.URL.Path, http.StatusBadRequest)
		}
	}))
	var review storage.Review
	require.NoError(t, json.Unmarshal([]byte(runShowCmd(t, "--job", "42", "--prompt", "--json")), &review))
	assert.Equal(t, "Review this diff please", review.Prompt)
	cmd := showCmd()
	cmd.SetArgs([]string{"--job", "42"})
	assertErrorContains(t, cmd.Execute(), "no review found")

	t.Run("completed review uses stored review prompt", func(t *testing.T) {
		mockReviewDaemon(t, storage.Review{JobID: 42, Agent: "codex", Prompt: "Completed review instructions", Job: &storage.ReviewJob{ID: 42, Status: storage.JobStatusDone}})
		assert.Contains(t, runShowCmd(t, "--job", "42", "--prompt"), "\nCompleted review instructions\n")
	})

	t.Run("queued job without a stored prompt still reports not found", func(t *testing.T) {
		daemonFromHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/review":
				http.Error(w, "not found", http.StatusNotFound)
			case "/api/jobs":
				json.NewEncoder(w).Encode(map[string]any{
					"jobs": []storage.ReviewJob{
						{
							ID:     42,
							Agent:  "codex",
							Status: storage.JobStatusQueued,
							Prompt: "",
						},
					},
				})
			}
		}))

		cmd := showCmd()
		cmd.SetArgs([]string{"--job", "42", "--prompt"})
		err := cmd.Execute()
		assertErrorContains(t, err, "no review found")
	})
}

func TestShowNoComments(t *testing.T) {
	repo := newTestGitRepo(t)
	repo.CommitFile("file.txt", "content", "initial commit")

	mockReviewDaemon(t, storage.Review{
		VerdictBool: testutil.ReviewFixtureVerdict("LGTM"),
		ID:          1, JobID: 42, Output: "LGTM", Agent: "test",
	})

	chdir(t, repo.Dir)
	output := runShowCmd(t, "--job", "42")

	assert.Contains(t, output, "LGTM")
	assert.NotContains(t, output, "Comments")
}
