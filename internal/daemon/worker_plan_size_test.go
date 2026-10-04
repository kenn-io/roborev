package daemon

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/prompt"
)

func TestWorkerPlanUsesCallerPromptTransport(t *testing.T) {
	for _, phase := range []string{"planning", "implementation"} {
		t.Run(phase, func(t *testing.T) {
			tc := newWorkerTestContext(t, 1)
			// Untracked local policy controls transport in both disposable worktrees.
			require.NoError(t, os.WriteFile(filepath.Join(tc.TmpDir, ".roborev.toml"), []byte("max_prompt_size = 1000\n"), 0o600))
			planning := "Analyze cancellation"
			plan := "Check cancellation"
			if phase == "planning" {
				planning += strings.Repeat("context", 300)
			} else {
				plan += strings.Repeat("step", 600)
			}
			calls := 0
			var snapshotPaths []string
			name := "worker-plan-limit-" + phase
			agent.Register(&agent.FakeAgent{NameStr: name, ReviewFn: func(_ context.Context, path, ref, p string, _ io.Writer) (string, error) {
				calls++
				text := p
				if rest, ok := strings.CutPrefix(p, "Read the complete task prompt from "); ok {
					quoted, _, _ := strings.Cut(rest, " and carry out")
					file, err := strconv.Unquote(quoted)
					require.NoError(t, err)
					rel, err := filepath.Rel(path, file)
					require.NoError(t, err)
					assert.True(t, filepath.IsLocal(rel))
					snapshotPaths = append(snapshotPaths, file)
					body, err := os.ReadFile(file)
					require.NoError(t, err)
					text = string(body)
				}
				if calls == 1 {
					assert.Equal(t, planning, text)
					return plan, nil
				}
				assert.Contains(t, text, "## Plan\n\n"+plan)
				require.NoError(t, os.WriteFile(filepath.Join(path, "fixed.txt"), []byte("fixed"), 0o600))
				return "Fixed", nil
			}})
			t.Cleanup(func() { agent.Unregister(name) })
			job := enqueuePlanTestJob(t, tc, name, prompt.EncodeFixPlan(planning, "## Review Findings\nHigh: missing cancellation"))
			tc.Pool.processJob(testWorkerID, job)
			assert.Equal(t, 2, calls)
			require.Len(t, snapshotPaths, 1)
			_, statErr := os.Stat(snapshotPaths[0])
			require.ErrorIs(t, statErr, os.ErrNotExist)
			updated, err := tc.DB.GetJobByID(job.ID)
			require.NoError(t, err)
			require.NotNil(t, updated.Patch)
			assert.Contains(t, *updated.Patch, "fixed.txt")
		})
	}
}
