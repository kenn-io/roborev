package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/storage"
)

func TestPostcap3ContinuedFixFailures(t *testing.T) {
	for _, mode := range []string{"discovery", "per-file"} {
		t.Run(mode, func(t *testing.T) {
			resetCLIUse(t)
			cliOutputIsTerminal = func(io.Writer) bool { return true }
			repo := createTestRepo(t, map[string]string{"main.go": "package main\n"})
			t.Chdir(repo.Dir)
			calls := 0
			name := "postcap-failing-agent"
			agent.Register(&agent.FakeAgent{NameStr: name, ReviewFn: func(context.Context, string, string, string, io.Writer) (string, error) {
				calls++
				return "", errors.New("provider unavailable")
			}})
			t.Cleanup(func() { agent.Unregister(name) })
			job := &storage.ReviewJob{ID: 99, GitRef: repo.HeadSHA(), Agent: "test", Status: storage.JobStatusDone, JobType: storage.JobTypeReview, Verdict: new("F")}
			hooks := MockRefineHooks{}
			args := []string{"fix", "--all-branches", "--agent", name}
			if mode == "per-file" {
				hooks.OnEnqueue = func(w http.ResponseWriter, _ *http.Request, _ *mockRefineState) bool {
					w.WriteHeader(http.StatusCreated)
					writeJSON(w, job)
					return true
				}
				args = []string{"analyze", "refactor", "--per-file", "--fix", "main.go", "--agent", "test", "--fix-agent", name}
			}
			md, rec := newCLIUseDaemon(t, hooks, nil)
			md.State.jobs[99] = job
			md.State.reviews[job.GitRef] = &storage.Review{ID: 1, JobID: 99, Output: "Finding", Job: job}

			result := runCLI(t, append(args, "--server", md.Server.URL)...)
			require.NoError(t, result.err)
			assert.Equal(t, 1, calls)
			assert.Contains(t, result.stdout, "Warning:")
			assert.Contains(t, result.stdout, "provider unavailable")
			posts := outcomePosts(rec)
			require.Len(t, posts, 1)
			assert.JSONEq(t, `{"event":"error_shown","properties":{"surface":"cli","kind":"finding_fixed"}}`, posts[0].body)
		})
	}
}
