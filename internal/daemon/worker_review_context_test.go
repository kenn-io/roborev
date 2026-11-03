package daemon

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/testutil"
)

func TestProcessJobUsesRepoReviewContextCount(t *testing.T) {
	const parentReview = "parent review finding for context"
	tests := []struct {
		name         string
		repoConfig   string
		wantPrevious bool
	}{
		{"repo unset inherits global", "", true},
		{"repo zero disables context", "review_context_count = 0\n", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tc := newWorkerTestContext(t, 1)
			require.Equal(t, 3, tc.Pool.cfgGetter.Config().ReviewContextCount)
			reviewer := &agent.FakeAgent{
				NameStr: "context-count-review",
				ReviewFn: func(context.Context, string, string, string, io.Writer) (string, error) {
					return string(testutil.ReviewFixtureJSON("No issues found.")), nil
				},
			}
			agent.RegisterForTest(t, reviewer)

			parent := tc.GitRepo.CommitFile("parent.txt", "parent\n", "parent")
			testutil.CreateCompletedReview(t, tc.DB, tc.Repo.ID, parent, "test", parentReview)
			if tt.repoConfig != "" {
				require.NoError(t, os.WriteFile(filepath.Join(tc.TmpDir, ".roborev.toml"), []byte(tt.repoConfig), 0o644))
			}
			head := tc.GitRepo.CommitFile("head.txt", "head\n", "head")
			job := tc.createJobWithAgent(t, head, reviewer.NameStr)
			claimed, err := tc.DB.ClaimJob(testWorkerID)
			require.NoError(t, err)
			require.Equal(t, job.ID, claimed.ID)
			tc.Pool.processJob(testWorkerID, claimed)

			review, err := tc.DB.GetReviewByJobID(job.ID)
			require.NoError(t, err)
			if tt.wantPrevious {
				assert.Contains(t, review.Prompt, "## Previous Reviews")
				assert.Contains(t, review.Prompt, parentReview)
			} else {
				assert.NotContains(t, review.Prompt, "## Previous Reviews")
				assert.NotContains(t, review.Prompt, parentReview)
			}
		})
	}
}
