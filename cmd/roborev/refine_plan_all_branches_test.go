package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func TestRefineAllBranchesPreservesCallerChangesDuringPlanning(t *testing.T) {
	for _, branchChange := range []bool{false, true} {
		t.Run(map[bool]string{false: "file edits", true: "branch switch"}[branchChange], func(t *testing.T) {
			originalUnsafe := agent.AllowUnsafeAgents()
			t.Cleanup(func() { agent.SetAllowUnsafeAgents(originalUnsafe) })
			repo := newPlanTestRepo(t, map[string]string{"source.go": "package source\n"})
			repo.Run("branch", "-M", "main")
			repo.Run("checkout", "-b", "feature")
			head := repo.CommitFile("next.go", "package source\n", "Add next source")
			repo.Run("branch", "caller")
			repo.Run("checkout", "main")
			chdir(t, repo.Dir)
			verdict := "F"
			job := storage.ReviewJob{ID: 7, GitRef: head, Branch: "feature", Status: storage.JobStatusDone, JobType: storage.JobTypeReview, Verdict: &verdict}
			review := &storage.Review{
				ID: 7, JobID: 7, Job: &job,
				VerdictBool: testutil.ReviewFixtureVerdict("High: missing cancellation"),
				Output:      "High: missing cancellation",
			}
			_ = newMockDaemonBuilder(t).
				WithJobs([]storage.ReviewJob{job}).
				WithHandler("/api/review", func(w http.ResponseWriter, _ *http.Request) {
					writeJSON(w, review)
				}).
				WithHandler("/api/comments", func(w http.ResponseWriter, _ *http.Request) {
					writeJSON(w, map[string]any{"responses": []storage.Response{}})
				}).Build()

			calls := 0
			name := "refine-all-branches-state-test"
			agent.Register(&agent.FakeAgent{NameStr: name, ReviewFn: func(_ context.Context, path, _, _ string, _ io.Writer) (string, error) {
				calls++
				assert.NotEqual(t, repo.Dir, path)
				if branchChange {
					repo.Run("checkout", "caller")
				} else {
					require.NoError(t, os.WriteFile(filepath.Join(repo.Dir, "source.go"), []byte("package source\n// caller edit\n"), 0o600))
					require.NoError(t, os.WriteFile(filepath.Join(repo.Dir, "notes.txt"), []byte("Caller notes\n"), 0o600))
				}
				return "", fmt.Errorf("planning provider unavailable")
			}})
			t.Cleanup(func() { agent.Unregister(name) })
			cmd := &cobra.Command{}
			cmd.SetContext(t.Context())
			err := runRefineAllBranches(cmd, refineOptions{
				plan: true, allBranches: true, quiet: true, agentName: name,
				maxIterations: 2, unsafeFlagChanged: true, allowUnsafeAgents: false,
			})
			var stateErr *planningStateError
			require.ErrorAs(t, err, &stateErr)
			assert := assert.New(t)
			assert.Equal(1, calls)
			assert.Equal(head, repo.HeadSHA())
			if branchChange {
				assert.Equal("caller", repo.Run("branch", "--show-current"))
			} else {
				assert.Equal("feature", repo.Run("branch", "--show-current"))
				source, sourceErr := os.ReadFile(filepath.Join(repo.Dir, "source.go"))
				require.NoError(t, sourceErr)
				assert.Equal("package source\n// caller edit\n", string(source))
				notes, notesErr := os.ReadFile(filepath.Join(repo.Dir, "notes.txt"))
				require.NoError(t, notesErr)
				assert.Equal("Caller notes\n", string(notes))
			}
		})
	}
}
