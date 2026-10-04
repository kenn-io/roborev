package daemon

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/storage"
)

func TestWorkerIsolatedCheckoutCleanupOnFailureOrCancel(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		name := "failure"
		if cancel {
			name = "cancellation"
		}
		t.Run(name, func(t *testing.T) {
			assert := assert.New(t)
			t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
			tc := newWorkerTestContext(t, 1)
			tc.Pool.cfgGetter.Config().IsolateReviews = true
			var path string
			var jobID int64
			var cancellationDelivered bool
			const name = "isolated-failing-reviewer"
			agent.RegisterForTest(t, &agent.FakeAgent{NameStr: name, ReviewFn: func(ctx context.Context, repoPath, ref, prompt string, out io.Writer) (string, error) {
				path = repoPath
				if cancel {
					if err := tc.DB.CancelJob(jobID); err != nil {
						return "", err
					}
					cancellationDelivered = tc.Pool.CancelJob(jobID)
					return "", ctx.Err()
				}
				return "", errors.New("synthetic agent failure")
			}})
			job := tc.createJobWithAgent(t, tc.GitRepo.HeadSHA(), name)
			jobID = job.ID
			_, err := tc.DB.Exec("UPDATE review_jobs SET retry_count=? WHERE id=?", maxRetries, job.ID)
			require.NoError(t, err)
			claimed, err := tc.DB.ClaimJob(testWorkerID)
			require.NoError(t, err)
			tc.Pool.processJob(testWorkerID, claimed)
			require.NotEmpty(t, path, "agent must run in an owned checkout")
			assert.NotEqual(tc.TmpDir, path)
			assert.NoDirExists(path)
			got, err := tc.DB.GetJobByID(job.ID)
			require.NoError(t, err)
			expected := storage.JobStatusFailed
			if cancel {
				assert.True(cancellationDelivered)
				expected = storage.JobStatusCanceled
			}
			assert.Equal(expected, got.Status)
		})
	}
}
