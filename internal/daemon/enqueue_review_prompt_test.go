package daemon

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func TestEnqueueReviewPromptExecutesFrozenRevision(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"commit", "range", "retry", "rerun", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			assert := assert.New(t)
			server, db, _ := newTestServer(t)
			server.workerPool.retryBackoff = 0
			repo := testutil.NewGitRepo(t)
			base := repo.CommitFile("file.txt", "base\n", "base")
			head := repo.CommitFile("file.txt", "review target\n", "target")
			repo.CommitFile(".roborev.toml", "isolate_reviews = false\nreview_guidelines = 'CHECKOUT_POLICY_CANARY'\n", "local settings")
			repo.Run("tag", "review-target", head)
			ref, frozenRef, jobType := "review-target", head, storage.JobTypeReview
			if kind == "range" {
				ref, frozenRef, jobType = base+"..review-target", base+".."+head, storage.JobTypeRange
			}
			const supplied = "Review this change for data loss. SUPPLIED_POLICY_CANARY\n"
			var paths, prompts []string
			var jobID int64
			name := "supplied-review-" + kind
			agent.RegisterForTest(t, &agent.FakeAgent{
				NameStr: name,
				ReviewFn: func(ctx context.Context, root, gotRef, body string, _ io.Writer) (string, error) {
					paths, prompts = append(paths, root), append(prompts, body)
					assert.NotEqual(repo.Path(), root)
					assert.Equal(frozenRef, gotRef)
					assert.Equal(head, strings.TrimSpace(repo.Run("-C", root, "rev-parse", "HEAD")))
					data, err := os.ReadFile(filepath.Join(root, "file.txt"))
					require.NoError(t, err)
					assert.Equal("review target\n", string(data))
					assert.Contains(body, supplied)
					assert.NotContains(body, "CHECKOUT_POLICY_CANARY")
					if kind == "retry" && len(paths) == 1 {
						return "", errors.New("connection reset by peer")
					}
					if kind == "cancel" {
						require.NoError(t, db.CancelJob(jobID))
						require.True(t, server.workerPool.CancelJob(jobID))
						return "", ctx.Err()
					}
					return string(testutil.ReviewFixtureJSON("No issues found.")), nil
				},
			})
			job := enqueueViaHTTP(t, server, EnqueueRequest{
				RepoPath: repo.Path(), GitRef: ref, Agent: name, ReviewPrompt: supplied,
			})
			jobID = job.ID
			assert.Equal(jobType, job.JobType)
			repo.CommitFile("file.txt", "later checkout\n", "advance checkout")
			repo.Run("tag", "-f", "review-target", "HEAD")
			runs := 1
			if kind == "retry" || kind == "rerun" {
				runs = 2
			}
			for run := range runs {
				if kind == "rerun" && run == 1 {
					_, err := server.humaRerunJob(t.Context(), &RerunJobInput{Body: RerunJobRequest{JobID: job.ID}})
					require.NoError(t, err)
				}
				claimed, err := db.ClaimJob(testWorkerID)
				require.NoError(t, err)
				require.NotNil(t, claimed)
				require.Equal(t, job.ID, claimed.ID)
				server.workerPool.processJob(testWorkerID, claimed)
			}
			require.Len(t, paths, runs)
			for _, root := range paths {
				assert.NoDirExists(root)
			}
			stored, err := db.GetJobByID(job.ID)
			require.NoError(t, err)
			if kind == "cancel" {
				assert.Equal(storage.JobStatusCanceled, stored.Status)
			} else {
				assert.Equal(storage.JobStatusDone, stored.Status)
				review, err := db.GetReviewByJobID(job.ID)
				require.NoError(t, err)
				assert.Contains(review.Output, "No issues found.")
			}
			if kind == "retry" || kind == "rerun" {
				assert.Equal(prompts[0], prompts[1])
			}
			data, err := os.ReadFile(filepath.Join(repo.Path(), "file.txt"))
			require.NoError(t, err)
			assert.Equal("later checkout\n", string(data))
		})
	}
}

func TestEnqueueReviewPromptRejectsConflictingInputs(t *testing.T) {
	t.Parallel()
	server, _, _ := newTestServer(t)
	repo := testutil.NewGitRepo(t)
	head := repo.CommitFile("file.txt", "source\n", "initial")
	for _, tt := range []struct {
		name string
		edit func(*EnqueueRequest)
	}{
		{"blank", func(r *EnqueueRequest) { r.ReviewPrompt = " \n" }},
		{"task prompt", func(r *EnqueueRequest) { r.CustomPrompt = "another prompt" }},
		{"dirty", func(r *EnqueueRequest) { r.GitRef = "dirty"; r.DiffContent = "diff" }},
		{"agentic", func(r *EnqueueRequest) { r.Agentic = true }},
		{"panel", func(r *EnqueueRequest) { r.Panel = "team" }},
		{"job type", func(r *EnqueueRequest) { r.JobType = storage.JobTypeTask }},
		{"source", func(r *EnqueueRequest) { r.Source = storage.JobSourcePostCommit }},
		{"goal", func(r *EnqueueRequest) { r.ReviewType = "goal" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := EnqueueRequest{RepoPath: repo.Path(), GitRef: head, Agent: "test", ReviewPrompt: "Review the change."}
			tt.edit(&req)
			w := enqueueRaw(t, server, req)
			assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
		})
	}
}
