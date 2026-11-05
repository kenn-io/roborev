package daemon

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/prompt"
	"go.kenn.io/roborev/internal/review"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func completedCompactForDelivery(t *testing.T, unresolved bool) (*workerTestContext, *storage.ReviewJob, *storage.Review) {
	t.Helper()
	setupTestEnv(t)
	tc := newWorkerTestContext(t, 1)
	source, manifest := compactSourceFixture(t, tc)
	tc.GitRepo.CommitFile("guard.go", "package fixture\n// authenticated evidence\n", "docs: fixture evidence")
	const name = "compact-delivery-fixture"
	lowStatus := "valid"
	if unresolved {
		lowStatus = "unresolved"
	}
	agent.RegisterForTest(t, &readOnlyCompactTestAgent{agent.FakeAgent{NameStr: name, ReviewFn: func(context.Context, string, string, string, io.Writer) (string, error) {
		decisions := []review.CompactDecision{
			{ID: "F1", Status: "false_positive", Reason: "The cited fixture material disproves the original assumption", Evidence: []review.CompactEvidence{{File: "guard.go", StartLine: 2, EndLine: 2, Quote: "// authenticated evidence"}}},
			{ID: "F2", Status: lowStatus, Reason: "Retain the original clarification", Evidence: []review.CompactEvidence{{File: "guard.go", StartLine: 2, EndLine: 2, Quote: "// authenticated evidence"}}},
		}
		raw, err := json.Marshal(map[string]any{"version": 1, "summary": "Historical commentary: repair dismissed original anyway", "decisions": decisions})
		return string(raw), err
	}}})
	text, err := review.BuildCompactPrompt(manifest, "", "")
	require.NoError(t, err)
	job, err := tc.DB.EnqueueJob(storage.EnqueueOpts{RepoID: tc.Repo.ID, GitRef: "compact-delivery", Agent: name, JobType: storage.JobTypeCompact, Prompt: text})
	require.NoError(t, err)
	require.NoError(t, WriteCompactMetadata(job.ID, CompactMetadata{Manifest: &manifest}))
	claimed, err := tc.DB.ClaimJob(testWorkerID)
	require.NoError(t, err)
	tc.Pool.processJob(testWorkerID, claimed)
	tc.assertJobStatus(t, job.ID, storage.JobStatusDone)
	result, err := tc.DB.GetReviewByJobID(job.ID)
	require.NoError(t, err)
	_, err = os.Stat(compactMetadataPath(job.ID))
	require.ErrorIs(t, err, os.ErrNotExist)
	sourceReview, err := tc.DB.GetReviewByJobID(source.ID)
	require.NoError(t, err)
	require.Equal(t, !unresolved, sourceReview.Closed)
	return tc, source, result
}

func TestCompactBackgroundFixEndpointFiltersAudit(t *testing.T) {
	for _, planFirst := range []bool{false, true} {
		t.Run(fmt.Sprint(planFirst), func(t *testing.T) {
			tc, _, result := completedCompactForDelivery(t, false)
			cfg := config.DefaultConfig()
			cfg.FixAgent = "test"
			server := NewServer(tc.DB, cfg, "")
			t.Cleanup(func() { require.NoError(t, server.Close()) })
			req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/job/fix", FixJobRequest{ParentJobID: result.JobID, GitRef: testutil.GetHeadSHA(t, tc.TmpDir), PlanFirst: planFirst})
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, req)
			assertHandlerStatus(t, response, http.StatusCreated)
			var fix storage.ReviewJob
			testutil.DecodeJSON(t, response, &fix)
			stored, err := tc.DB.GetJobByID(fix.ID)
			require.NoError(t, err)
			implementation := stored.Prompt
			var planning string
			if planFirst {
				var planned bool
				planning, implementation, planned, err = prompt.DecodeFixPlan(stored.Prompt)
				require.NoError(t, err)
				require.True(t, planned)
			}
			bodies := []string{implementation}
			if planFirst {
				bodies = append(bodies, planning)
			}
			for _, body := range bodies {
				a := assert.New(t)
				a.Contains(body, "Retained low original")
				a.NotContains(body, "Dismissed high original")
				a.NotContains(body, "Historical commentary")
				a.NotContains(body, storage.CompactAuditPrefix)
			}
		})
	}
}

func TestCompletedCompactRerunPreservesSavedResult(t *testing.T) {
	for _, unresolved := range []bool{false, true} {
		t.Run(fmt.Sprint(unresolved), func(t *testing.T) {
			tc, source, result := completedCompactForDelivery(t, unresolved)
			beforeJob, err := tc.DB.GetJobByID(result.JobID)
			require.NoError(t, err)
			beforeSource, err := tc.DB.GetReviewByJobID(source.ID)
			require.NoError(t, err)
			cfg := config.DefaultConfig()
			server := NewServer(tc.DB, cfg, "")
			t.Cleanup(func() { require.NoError(t, server.Close()) })
			requestID := uuid.New()
			req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/job/rerun", RerunJobRequest{JobID: result.JobID, Agent: "test", RequestID: &requestID})
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, req)
			assertHandlerStatus(t, response, http.StatusConflict)
			after, err := tc.DB.GetReviewByJobID(result.JobID)
			require.NoError(t, err)
			afterJob, err := tc.DB.GetJobByID(result.JobID)
			require.NoError(t, err)
			afterSource, err := tc.DB.GetReviewByJobID(source.ID)
			require.NoError(t, err)
			a := assert.New(t)
			a.Equal(result, after, "canonical findings, audit and review identity must survive")
			a.Equal(beforeJob, afterJob, "rejection must not reset the attempt")
			a.Equal(beforeSource, afterSource, "source closure state must not change")
			// Direct storage callers must receive the same guard before deletion.
			err = tc.DB.ReenqueueJob(result.JobID, storage.ReenqueueOpts{})
			require.Error(t, err)
			after, err = tc.DB.GetReviewByJobID(result.JobID)
			require.NoError(t, err)
			a.Equal(result, after)
			_, found, err := tc.DB.GetRerunRequest(requestID, result.JobID)
			require.NoError(t, err)
			a.False(found, "rejected requests must not be recorded as successful")
		})
	}
}

func TestMarkedCompactCheckoutCannotFallBackAfterValidation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("controlled POSIX git interleaving")
	}
	setupTestEnv(t)
	tc := newWorkerTestContext(t, 1)
	caller := filepath.Join(t.TempDir(), "caller")
	tc.GitRepo.Run("worktree", "add", "--detach", caller, "HEAD")
	caller, err := filepath.EvalSymlinks(caller)
	require.NoError(t, err)
	realGit, err := exec.LookPath("git")
	require.NoError(t, err)
	bin := t.TempDir()
	removed := filepath.Join(t.TempDir(), "removed")
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
	// Return authentic Git metadata, then remove only the controlled temporary
	// checkout just as the first validation's final git command finishes.
	script := fmt.Sprintf("#!/bin/sh\n%s \"$@\"\ncompact_git_status=$?\nif [ \"$(pwd -P)\" = %s ] && [ \"$3\" = '--is-bare-repository' ]; then rm -rf -- %s; : > %s; fi\nexit \"$compact_git_status\"\n", quote(realGit), quote(caller), quote(caller), quote(removed))
	require.NoError(t, os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o700))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	job := &storage.ReviewJob{RepoPath: tc.TmpDir, WorktreePath: caller, JobType: storage.JobTypeCompact, Prompt: review.CompactPromptPrefix + "manifest"}
	checkout, err := tc.Pool.prepareJobCheckout(t.Context(), testWorkerID, job, config.DefaultConfig())
	require.NoError(t, err)
	require.FileExists(t, removed, "fixture must remove checkout inside the first validation")
	_, err = os.Stat(caller)
	require.ErrorIs(t, err, os.ErrNotExist)
	assert.Equal(t, caller, checkout.agentRepoPath, "never select main after caller validation")
	assert.Equal(t, caller, checkout.promptRepoPath)
}
