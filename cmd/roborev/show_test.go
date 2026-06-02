package main

// Tests for the show command

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
)

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
				ID: 1, JobID: 42, Output: "LGTM", Agent: "test",
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
				ID: 1, JobID: 42, Output: "Test review output", Agent: "codex",
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
			ID: 1, JobID: 42, Output: "LGTM", Agent: "test",
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
		ID: 1, JobID: 42, Output: "LGTM", Agent: "test",
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
		ID: 1, JobID: 42, Output: "Found issues", Agent: "test",
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
				ID: 1, JobID: 42, Output: "Dirty review output", Agent: "test",
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

func TestShowNoComments(t *testing.T) {
	repo := newTestGitRepo(t)
	repo.CommitFile("file.txt", "content", "initial commit")

	mockReviewDaemon(t, storage.Review{
		ID: 1, JobID: 42, Output: "LGTM", Agent: "test",
	})

	chdir(t, repo.Dir)
	output := runShowCmd(t, "--job", "42")

	assert.Contains(t, output, "LGTM")
	assert.NotContains(t, output, "Comments")
}
