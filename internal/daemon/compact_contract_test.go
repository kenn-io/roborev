package daemon

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/review"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
	"go.kenn.io/roborev/pkg/structuredreview"
)

type readOnlyCompactTestAgent struct{ agent.FakeAgent }

func (a *readOnlyCompactTestAgent) SupportsReadOnlyReview() bool                   { return true }
func (a *readOnlyCompactTestAgent) WithAgentic(bool) agent.Agent                   { return a }
func (a *readOnlyCompactTestAgent) WithModel(string) agent.Agent                   { return a }
func (a *readOnlyCompactTestAgent) WithReasoning(agent.ReasoningLevel) agent.Agent { return a }

func compactSourceFixture(t *testing.T, tc *workerTestContext) (*storage.ReviewJob, review.CompactManifest) {
	t.Helper()
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, "test")
	doc := structuredreview.Document{SchemaVersion: 3, Summary: "Original two findings", Verdict: "fail", Findings: []structuredreview.Finding{
		{Severity: "high", Kind: "defect", Confidence: 0.8, Problem: "Dismissed high original", Fix: "Preserve ownership"},
		{Severity: "low", Kind: "improvement", Confidence: 0.7, Problem: "Retained low original", Fix: "Clarify docs"},
	}}
	raw, err := json.Marshal(doc)
	require.NoError(t, err)
	require.NoError(t, tc.DB.CompleteJobResult(job.ID, "test", storage.ReviewCompletion{StructuredOutput: raw}))
	stored, err := tc.DB.GetReviewByJobID(job.ID)
	require.NoError(t, err)
	return job, review.CompactManifest{Version: 1, Sources: []review.CompactSource{{JobID: job.ID, ReviewUUID: *stored.UUID, GitRef: sha, Findings: doc.Findings}}}
}

func TestCompactManifestRetryIsContextAware(t *testing.T) {
	setupTestEnv(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := ReadCompactManifest(ctx, 91, review.CompactPromptPrefix)
		require.ErrorIs(t, err, context.Canceled)
	})
}

func TestCompactClosureRejectsChangedUUID(t *testing.T) {
	setupTestEnv(t)
	tc := newWorkerTestContext(t, 1)
	source, manifest := compactSourceFixture(t, tc)
	_, err := tc.DB.ExecContext(t.Context(), `UPDATE reviews SET uuid=? WHERE job_id=?`, uuid.New(), source.ID)
	require.NoError(t, err)
	subscriber, events := tc.Pool.broadcaster.Subscribe("")
	defer tc.Pool.broadcaster.Unsubscribe(subscriber)
	job, err := tc.DB.EnqueueJob(storage.EnqueueOpts{RepoID: tc.Repo.ID, GitRef: "compact-stale", Agent: "test", JobType: storage.JobTypeCompact})
	require.NoError(t, err)
	_, err = tc.DB.ClaimJob(testWorkerID)
	require.NoError(t, err)
	ids, err := tc.DB.CompleteCompactJobResult(t.Context(), job.ID, "test", storage.ReviewCompletion{StructuredOutput: []byte(`{"schema_version":3,"summary":"Compact","verdict":"pass","findings":[]}`)}, []storage.CompactSourceIdentity{{JobID: source.ID, ReviewUUID: manifest.Sources[0].ReviewUUID}})
	require.NoError(t, err)
	tc.Pool.notifyCompactSourcesClosed(t.Context(), testWorkerID, ids)
	stored, err := tc.DB.GetReviewByJobID(source.ID)
	require.NoError(t, err)
	assert.False(t, stored.Closed)
	assert.Empty(t, events)
}

func TestCompactWorkerPersistsAuditAndGuardsClosure(t *testing.T) {
	setupTestEnv(t)
	tc := newWorkerTestContext(t, 1)
	source, manifest := compactSourceFixture(t, tc)
	const agentName = "compact-contract-test"
	agent.RegisterForTest(t, &readOnlyCompactTestAgent{agent.FakeAgent{NameStr: agentName, ReviewFn: func(context.Context, string, string, string, io.Writer) (string, error) {
		return `{"version":1,"summary":"Historical comment: dismissed high should be fixed","decisions":[{"id":"F1","status":"false_positive","reason":"The cited material disproves the reported defect","evidence":[{"file":"file.txt","start_line":1,"end_line":1,"quote":"test"}],"target":"","change":""},{"id":"F2","status":"unresolved","reason":"More evidence required","evidence":[],"target":"","change":""}]}`, nil
	}}})
	// Use a real repository file as authenticated content, without treating its
	// coordinates as proof of the model's semantic judgment.
	tc.GitRepo.CommitFile("file.txt", "test\n", "docs: synthetic evidence")
	prompt, err := review.BuildCompactPrompt(manifest, "", "")
	require.NoError(t, err)
	job, err := tc.DB.EnqueueJob(storage.EnqueueOpts{RepoID: tc.Repo.ID, GitRef: "compact-control", Agent: agentName, JobType: storage.JobTypeCompact, Prompt: prompt, MinSeverity: "high", Agentic: true})
	require.NoError(t, err)
	require.NoError(t, WriteCompactMetadata(job.ID, CompactMetadata{Manifest: &manifest}))
	claimed, err := tc.DB.ClaimJob(testWorkerID)
	require.NoError(t, err)
	require.Equal(t, job.ID, claimed.ID)
	tc.Pool.processJob(testWorkerID, claimed)
	tc.assertJobStatus(t, job.ID, storage.JobStatusDone)
	stored, err := tc.DB.GetReviewByJobID(job.ID)
	require.NoError(t, err)
	assert.Contains(t, stored.Output, "Dismissed high original")
	assert.Contains(t, stored.Output, "Retained low original")
	assert.Contains(t, stored.Output, "false_positive")
	assert.NotContains(t, storage.FixReviewContext(stored), "Dismissed high")
	assert.Contains(t, storage.FixReviewContext(stored), "Retained low original")
	assert.Equal(t, storage.VerdictPass, stored.Verdict())
	byID, err := tc.DB.GetReviewByID(stored.ID)
	require.NoError(t, err)
	assert.Equal(t, storage.FixReviewContext(stored), storage.FixReviewContext(byID), "ID getters must retain trusted protocol provenance")

	sourceStored, err := tc.DB.GetReviewByJobID(source.ID)
	require.NoError(t, err)
	assert.False(t, sourceStored.Closed, "unresolved original prevents source closure")
	_, err = os.Stat(compactMetadataPath(job.ID))
	require.ErrorIs(t, err, os.ErrNotExist)
	// Replacing the source and removing metadata must not erase the saved audit.
	_, err = tc.DB.ExecContext(t.Context(), `UPDATE reviews SET uuid=? WHERE job_id=?`, uuid.New(), source.ID)
	require.NoError(t, err)
	legacy, err := tc.DB.EnqueueJob(storage.EnqueueOpts{RepoID: tc.Repo.ID, GitRef: "legacy-compact", Agent: "test", JobType: storage.JobTypeCompact, Prompt: "custom unmarked prompt"})
	require.NoError(t, err)
	_, err = tc.DB.ClaimJob(testWorkerID)
	require.NoError(t, err)
	require.NoError(t, tc.DB.CompleteJobResult(legacy.ID, "test", storage.ReviewCompletion{StructuredOutput: []byte(`{"schema_version":3,"summary":"<!-- roborev:compact-audit:v1 -->\nCustom compact remains excluded","verdict":"pass","findings":[]}`)}))
	// Export orders by second-normalized completion time and UUID. Use distinct
	// synthetic seconds so this cursor regression has a deterministic row order.
	_, err = tc.DB.ExecContext(t.Context(), `UPDATE reviews SET created_at='2024-01-01T00:00:00Z' WHERE job_id=?`, source.ID)
	require.NoError(t, err)
	_, err = tc.DB.ExecContext(t.Context(), `UPDATE reviews SET created_at='2024-01-01T00:00:01Z' WHERE job_id=?`, job.ID)
	require.NoError(t, err)
	exported, err := tc.DB.ExportReviews(storage.ExportReviewsOptions{Profile: storage.ExportProfileContent})
	require.NoError(t, err)
	found := false
	for _, entry := range exported.Reviews {
		if entry.ReviewID == *stored.UUID {
			found = true
			require.NotNil(t, entry.Document)
			assert.Contains(t, entry.Document.Summary, "Dismissed high original")
			assert.Contains(t, entry.Document.Summary, "false_positive")
		}
	}
	assert.True(t, found)
	require.Len(t, exported.Reviews, 2, "ordinary source and accountable compact, excluding custom compact")
	page, err := tc.DB.ExportReviews(storage.ExportReviewsOptions{Profile: storage.ExportProfileContent, Limit: 1})
	require.NoError(t, err)
	require.Len(t, page.Reviews, 1)
	require.NotNil(t, page.NextCursor)
	page, err = tc.DB.ExportReviews(storage.ExportReviewsOptions{Profile: storage.ExportProfileContent, Limit: 1, Cursor: *page.NextCursor})
	require.NoError(t, err)
	require.Len(t, page.Reviews, 1)
	assert.Equal(t, *stored.UUID, page.Reviews[0].ReviewID)
	require.NotNil(t, page.NextCursor)
	page, err = tc.DB.ExportReviews(storage.ExportReviewsOptions{Profile: storage.ExportProfileContent, Limit: 1, Cursor: *page.NextCursor})
	require.NoError(t, err, "cursor emitted for accountable compact must be resumable")
	assert.Empty(t, page.Reviews)
}

type nativeCompactTestAgent struct {
	readOnlyCompactTestAgent
	schema jsontext.Value
}

func (a *nativeCompactTestAgent) WithAgentic(bool) agent.Agent                   { return a }
func (a *nativeCompactTestAgent) WithModel(string) agent.Agent                   { return a }
func (a *nativeCompactTestAgent) WithReasoning(agent.ReasoningLevel) agent.Agent { return a }
func (a *nativeCompactTestAgent) ReviewWithSchema(ctx context.Context, repo, ref, prompt string, schema jsontext.Value, out io.Writer) (jsontext.Value, error) {
	a.schema = schema
	raw, err := a.Review(ctx, repo, ref, prompt, out)
	return jsontext.Value(raw), err
}

func TestCompactProtocolSurvivesPromptFileHandoff(t *testing.T) {
	setupTestEnv(t)
	tc := newWorkerTestContext(t, 1)
	_, manifest := compactSourceFixture(t, tc)
	cfg := config.DefaultConfig()
	// Exercise the existing configured transport threshold, not a new limit.
	cfg.DefaultMaxPromptSize = 1024
	tc.reconfigurePool(cfg)
	const name = "compact-file-contract"
	a := &nativeCompactTestAgent{readOnlyCompactTestAgent: readOnlyCompactTestAgent{agent.FakeAgent{NameStr: name, ReviewFn: func(_ context.Context, _ string, _ string, prompt string, _ io.Writer) (string, error) {
		assert.Contains(t, prompt, "Read the complete task prompt from")
		return `{"version":1,"summary":"uncertain","decisions":[]}`, nil
	}}}}
	agent.RegisterForTest(t, a)
	text, err := review.BuildCompactPrompt(manifest, "", "")
	require.NoError(t, err)
	job, err := tc.DB.EnqueueJob(storage.EnqueueOpts{RepoID: tc.Repo.ID, GitRef: "compact-file", Agent: name, JobType: storage.JobTypeCompact, Prompt: text})
	require.NoError(t, err)
	require.NoError(t, WriteCompactMetadata(job.ID, CompactMetadata{Manifest: &manifest}))
	claimed, err := tc.DB.ClaimJob(testWorkerID)
	require.NoError(t, err)
	tc.Pool.processJob(testWorkerID, claimed)
	tc.assertJobStatus(t, job.ID, storage.JobStatusDone)
	assert.NotContains(t, string(a.schema), "schema_version")
	assert.Contains(t, string(a.schema), "false_positive")
	stored, err := tc.DB.GetReviewByJobID(job.ID)
	require.NoError(t, err)
	assert.Contains(t, stored.Output, "Dismissed high original")
	assert.Contains(t, stored.Output, "Retained low original")
}

func TestMarkedCompactRejectsMissingCallerCheckout(t *testing.T) {
	for _, kind := range []string{"removed", "unrelated", "subdirectory"} {
		t.Run(kind, func(t *testing.T) {
			setupTestEnv(t)
			tc := newWorkerTestContext(t, 1)
			source, manifest := compactSourceFixture(t, tc)
			caller := filepath.Join(t.TempDir(), "caller")
			switch kind {
			case "removed":
				tc.GitRepo.Run("worktree", "add", "--detach", caller, "HEAD")
			case "unrelated":
				caller = testutil.NewTestRepoWithCommit(t).Root
			case "subdirectory":
				caller = filepath.Join(tc.TmpDir, "nested")
				require.NoError(t, os.MkdirAll(caller, 0o700))
			}
			text, err := review.BuildCompactPrompt(manifest, "", "")
			require.NoError(t, err)
			calls := 0
			const name = "compact-no-fallback"
			agent.RegisterForTest(t, &readOnlyCompactTestAgent{agent.FakeAgent{NameStr: name, ReviewFn: func(context.Context, string, string, string, io.Writer) (string, error) {
				calls++
				return `{"version":1,"summary":"","decisions":[]}`, nil
			}}})
			job, err := tc.DB.EnqueueJob(storage.EnqueueOpts{RepoID: tc.Repo.ID, GitRef: "compact-missing", Agent: name, JobType: storage.JobTypeCompact, Prompt: text, WorktreePath: caller})
			require.NoError(t, err)
			require.NoError(t, WriteCompactMetadata(job.ID, CompactMetadata{Manifest: &manifest}))
			if kind == "removed" {
				tc.GitRepo.Run("worktree", "remove", caller)
			}
			claimed, err := tc.DB.ClaimJob(testWorkerID)
			require.NoError(t, err)
			tc.Pool.processJob(testWorkerID, claimed)
			assert.Zero(t, calls, "must not inspect fallback main")
			stored, err := tc.DB.GetReviewByJobID(source.ID)
			require.NoError(t, err)
			assert.False(t, stored.Closed)
		})
	}
}

func TestMarkedCompactUsesCallerCheckout(t *testing.T) {
	setupTestEnv(t)
	tc := newWorkerTestContext(t, 1)
	tc.GitRepo.CommitFile("guard.go", "package fixture\n// initial guard\n", "feat: initial guard")
	source, manifest := compactSourceFixture(t, tc)
	caller := filepath.Join(t.TempDir(), "caller")
	tc.GitRepo.Run("worktree", "add", "--detach", caller, "HEAD")
	require.NoError(t, os.WriteFile(filepath.Join(caller, "guard.go"), []byte("package fixture\n// caller guard\n"), 0o600))
	tc.GitRepo.CommitFile("guard.go", "package fixture\n// different main guard\n", "chore: separate main")
	const name = "compact-caller-checkout"
	calls := 0
	agent.RegisterForTest(t, &readOnlyCompactTestAgent{agent.FakeAgent{NameStr: name, ReviewFn: func(_ context.Context, repo string, _ string, _ string, _ io.Writer) (string, error) {
		calls++
		assert.Equal(t, caller, repo)
		return `{"version":1,"summary":"Two original findings remain","decisions":[{"id":"F1","status":"valid","reason":"The caller code still exhibits the original behavior","evidence":[{"file":"guard.go","start_line":2,"end_line":2,"quote":"// caller guard"}],"target":"","change":""},{"id":"F2","status":"valid","reason":"The caller material still needs the original clarification","evidence":[{"file":"guard.go","start_line":2,"end_line":2,"quote":"// caller guard"}],"target":"","change":""}]}`, nil
	}}})
	text, err := review.BuildCompactPrompt(manifest, "", "")
	require.NoError(t, err)
	job, err := tc.DB.EnqueueJob(storage.EnqueueOpts{RepoID: tc.Repo.ID, GitRef: "compact-caller", Agent: name, JobType: storage.JobTypeCompact, Prompt: text, WorktreePath: caller})
	require.NoError(t, err)
	require.NoError(t, WriteCompactMetadata(job.ID, CompactMetadata{Manifest: &manifest}))
	claimed, err := tc.DB.ClaimJob(testWorkerID)
	require.NoError(t, err)
	tc.Pool.processJob(testWorkerID, claimed)
	tc.assertJobStatus(t, job.ID, storage.JobStatusDone)
	assert.Equal(t, 1, calls)
	original, err := tc.DB.GetReviewByJobID(source.ID)
	require.NoError(t, err)
	assert.True(t, original.Closed, "fully accounted originals are replaced by saved compact")
	result, err := tc.DB.GetReviewByJobID(job.ID)
	require.NoError(t, err)
	assert.Contains(t, result.Output, "### F1 — valid")
	assert.Contains(t, result.Output, "### F2 — valid")
}

func TestCompactUnconfirmedCompletionNeverClosesSources(t *testing.T) {
	for _, failure := range []string{"canceled_noop", "save_failure", "missing_after_save", "source_close_failure"} {
		t.Run(failure, func(t *testing.T) {
			setupTestEnv(t)
			tc := newWorkerTestContext(t, 1)
			tc.GitRepo.CommitFile("guard.go", "package fixture\n// caller guard\n", "feat: guard fixture")
			source, manifest := compactSourceFixture(t, tc)
			var compactID int64
			const name = "compact-completion-failure"
			agent.RegisterForTest(t, &readOnlyCompactTestAgent{agent.FakeAgent{NameStr: name, ReviewFn: func(context.Context, string, string, string, io.Writer) (string, error) {
				switch failure {
				case "canceled_noop":
					require.NoError(t, tc.DB.CancelJob(compactID))
				case "save_failure":
					_, err := tc.DB.ExecContext(t.Context(), fmt.Sprintf(`CREATE TRIGGER compact_save_failure BEFORE INSERT ON reviews WHEN NEW.job_id=%d BEGIN SELECT RAISE(FAIL,'synthetic persistence failure'); END`, compactID))
					require.NoError(t, err)
				case "source_close_failure":
					_, err := tc.DB.ExecContext(t.Context(), fmt.Sprintf(`CREATE TRIGGER compact_close_failure BEFORE UPDATE OF closed ON reviews WHEN OLD.job_id=%d BEGIN SELECT RAISE(ABORT,'synthetic closure failure'); END`, source.ID))
					require.NoError(t, err)
				case "missing_after_save":
					_, err := tc.DB.ExecContext(t.Context(), fmt.Sprintf(`CREATE TRIGGER compact_missing AFTER INSERT ON reviews WHEN NEW.job_id=%d BEGIN DELETE FROM review_jobs WHERE id=NEW.job_id; END`, compactID))
					require.NoError(t, err)
				}
				return `{"version":1,"summary":"Both remain","decisions":[{"id":"F1","status":"valid","reason":"The current code preserves the original concern","evidence":[{"file":"guard.go","start_line":2,"end_line":2,"quote":"// caller guard"}],"target":"","change":""},{"id":"F2","status":"valid","reason":"The original clarification still applies","evidence":[{"file":"guard.go","start_line":2,"end_line":2,"quote":"// caller guard"}],"target":"","change":""}]}`, nil
			}}})
			text, err := review.BuildCompactPrompt(manifest, "", "")
			require.NoError(t, err)
			job, err := tc.DB.EnqueueJob(storage.EnqueueOpts{RepoID: tc.Repo.ID, GitRef: "compact-unconfirmed", Agent: name, JobType: storage.JobTypeCompact, Prompt: text})
			require.NoError(t, err)
			compactID = job.ID
			require.NoError(t, WriteCompactMetadata(job.ID, CompactMetadata{Manifest: &manifest}))
			claimed, err := tc.DB.ClaimJob(testWorkerID)
			require.NoError(t, err)
			tc.Pool.processJob(testWorkerID, claimed)
			original, err := tc.DB.GetReviewByJobID(source.ID)
			require.NoError(t, err)
			assert.False(t, original.Closed)
			if failure == "source_close_failure" {
				tc.assertJobStatus(t, job.ID, storage.JobStatusFailed)
				_, err = os.Stat(compactMetadataPath(job.ID))
				require.NoError(t, err)
				_, err = tc.DB.GetReviewByJobID(job.ID)
				require.ErrorIs(t, err, sql.ErrNoRows)
			}
		})
	}
}

func TestCompactMetadataAtomicOverwriteAndFailureCleanup(t *testing.T) {
	root := setupTestEnv(t)
	jobID := int64(321)
	require.NoError(t, WriteCompactMetadata(jobID, CompactMetadata{SourceJobIDs: []int64{1}}))
	require.NoError(t, WriteCompactMetadata(jobID, CompactMetadata{SourceJobIDs: []int64{2, 3}}))
	got, err := ReadCompactMetadata(jobID)
	require.NoError(t, err)
	assert.Equal(t, []int64{2, 3}, got.SourceJobIDs, "existing metadata must be replaced completely")
	path := compactMetadataPath(jobID)
	require.NoError(t, os.Remove(path))
	require.NoError(t, os.Mkdir(path, 0o700))
	sentinel := filepath.Join(path, "keep")
	require.NoError(t, os.WriteFile(sentinel, []byte("preserved"), 0o600))
	require.Error(t, WriteCompactMetadata(jobID, CompactMetadata{SourceJobIDs: []int64{4}}))
	data, err := os.ReadFile(sentinel)
	require.NoError(t, err)
	assert.Equal(t, "preserved", string(data), "failed publication must preserve the existing target")
	files, err := filepath.Glob(filepath.Join(root, ".compact-*.json"))
	require.NoError(t, err)
	assert.Empty(t, files, "success and failure must both clean up temporary metadata")
}
