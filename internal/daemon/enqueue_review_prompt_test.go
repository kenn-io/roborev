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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	gitpkg "go.kenn.io/roborev/internal/git"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func TestEnqueueReviewPromptExecutesFrozenRevision(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"commit", "range", "retry", "rerun", "imported rerun", "cancel"} {
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
			if kind == "retry" || kind == "rerun" || kind == "imported rerun" {
				runs = 2
			}
			for run := range runs {
				if kind == "imported rerun" && run == 1 {
					machineID, err := db.GetMachineID()
					require.NoError(t, err)
					jobs, err := db.GetJobsToSync(machineID, 10)
					require.NoError(t, err)
					require.Len(t, jobs, 1)
					exported := jobs[0]
					server, db, _ = newTestServer(t)
					localRepo, err := db.GetOrCreateRepo(repo.Path())
					require.NoError(t, err)
					commit, err := db.GetOrCreateCommit(localRepo.ID, exported.CommitSHA,
						exported.CommitAuthor, exported.CommitSubject, exported.CommitTimestamp)
					require.NoError(t, err)
					// Sync carries the source and prompt, but no prompt_prebuilt field.
					require.NoError(t, db.UpsertPulledJob(storage.PulledJob{
						UUID: exported.UUID, GitRef: exported.GitRef, Agent: exported.Agent,
						Source: exported.Source, Prompt: exported.Prompt, JobType: exported.JobType,
						Status: exported.Status, EnqueuedAt: exported.EnqueuedAt,
						FinishedAt: exported.FinishedAt, UpdatedAt: exported.UpdatedAt,
						SourceMachineID: exported.SourceMachineID,
					}, localRepo.ID, &commit.ID))
					require.NoError(t, db.QueryRowContext(t.Context(),
						`SELECT id FROM review_jobs WHERE uuid = ?`, exported.UUID).Scan(&job.ID))
				}
				if (kind == "rerun" || kind == "imported rerun") && run == 1 {
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
			if kind == "retry" || kind == "rerun" || kind == "imported rerun" {
				assert.Equal(prompts[0], prompts[1])
			}
			data, err := os.ReadFile(filepath.Join(repo.Path(), "file.txt"))
			require.NoError(t, err)
			assert.Equal("later checkout\n", string(data))
		})
	}
}

func TestEnqueueReviewPromptSurvivesCommitRemapping(t *testing.T) {
	t.Parallel()
	for _, method := range []string{"api", "git ref"} {
		t.Run(method, func(t *testing.T) {
			assert := assert.New(t)
			server, db, _ := newTestServer(t)
			repo := testutil.NewGitRepo(t)
			repo.CommitFile("base.txt", "original base\n", "base")
			repo.Run("checkout", "-b", "feature")
			head := repo.CommitFile("feature.txt", "feature\n", "feature")
			const supplied = "Review the feature against the original base."
			called := false
			name := "remap-supplied-review-" + method
			agent.RegisterForTest(t, &agent.FakeAgent{
				NameStr: name,
				ReviewFn: func(_ context.Context, root, ref, body string, _ io.Writer) (string, error) {
					called = true
					assert.Equal(head, ref)
					assert.Equal(head, strings.TrimSpace(repo.Run("-C", root, "rev-parse", "HEAD")))
					base, err := os.ReadFile(filepath.Join(root, "base.txt"))
					require.NoError(t, err)
					assert.Equal("original base\n", string(base))
					assert.Contains(body, supplied)
					return string(testutil.ReviewFixtureJSON("No issues found.")), nil
				},
			})
			frozen := enqueueViaHTTP(t, server, EnqueueRequest{
				RepoPath: repo.Path(), GitRef: head, Agent: name, ReviewPrompt: supplied,
			})
			ordinary := enqueueViaHTTP(t, server, EnqueueRequest{
				RepoPath: repo.Path(), GitRef: head, Agent: "test",
			})
			repo.Run("checkout", "main")
			repo.CommitFile("base.txt", "changed base\n", "advance base")
			repo.Run("checkout", "feature")
			repo.Run("rebase", "main")
			rebased := repo.HeadSHA()
			require.NotEqual(t, head, rebased)
			patchID := gitpkg.GetPatchID(repo.Path(), rebased)
			require.NotEmpty(t, patchID)
			require.Equal(t, gitpkg.GetPatchID(repo.Path(), head), patchID)
			if method == "api" {
				result, err := server.humaRemap(t.Context(), &RemapInput{Body: RemapRequest{
					RepoPath: repo.Path(), Mappings: []RemapMapping{{
						OldSHA: head, NewSHA: rebased, PatchID: patchID,
						Author: "Author", Subject: "feature", Timestamp: time.Now().Format(time.RFC3339),
					}},
				}})
				require.NoError(t, err)
				assert.Equal(1, result.Body.Remapped)
			} else {
				commit, err := db.GetOrCreateCommit(frozen.RepoID, rebased, "Author", "feature", time.Now())
				require.NoError(t, err)
				count, err := db.RemapJobGitRef(frozen.RepoID, head, rebased, patchID, commit.ID)
				require.NoError(t, err)
				assert.Equal(1, count)
			}
			moved, err := db.GetJobByID(ordinary.ID)
			require.NoError(t, err)
			assert.Equal(rebased, moved.GitRef)
			claimed, err := db.ClaimJob(testWorkerID)
			require.NoError(t, err)
			require.NotNil(t, claimed)
			require.Equal(t, frozen.ID, claimed.ID)
			assert.Equal(frozen.CommitID, claimed.CommitID)
			server.workerPool.processJob(testWorkerID, claimed)
			assert.True(called)
			stored, err := db.GetJobByID(frozen.ID)
			require.NoError(t, err)
			assert.Equal(storage.JobStatusDone, stored.Status)
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
