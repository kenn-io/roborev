package daemon

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	gitpkg "go.kenn.io/roborev/internal/git"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func TestFindCompatibleReusableSessionSkipsOtherCLIs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake Gemini command uses a POSIX shell")
	}
	server, db, tmpDir := newTestServer(t)
	repoDir := filepath.Join(tmpDir, "testrepo")
	testRepo := testutil.InitTestGitRepo(t, repoDir)
	testRepo.CheckoutNewBranch("feature/session")

	reuseSessions := true
	cfg := server.configWatcher.Config()
	cfg.ReuseReviewSession = &reuseSessions
	geminiCommand := filepath.Join(tmpDir, "gemini")
	require.NoError(t, os.WriteFile(geminiCommand, []byte("#!/bin/sh\nexit 0\n"), 0o755))
	cfg.GeminiCmd = geminiCommand

	repoRoot, err := gitpkg.GetMainRepoRoot(repoDir)
	require.NoError(t, err)
	repo, err := db.GetOrCreateRepo(repoRoot, config.ResolveRepoIdentity(repoRoot, nil))
	require.NoError(t, err)
	sha := testRepo.HeadSHA()
	commit, err := db.GetOrCreateCommit(repo.ID, sha, "Author", "Subject", time.Now())
	require.NoError(t, err)

	seedCandidate := func(sessionID, sessionCLI string, finishedAt time.Time) {
		t.Helper()
		job, err := db.EnqueueJob(storage.EnqueueOpts{
			RepoID: repo.ID, CommitID: commit.ID, GitRef: sha, Branch: "feature/session",
			Agent: "gemini", Model: "gemini-3.1-pro-preview", Reasoning: "thorough",
			ReviewType: config.ReviewTypeDefault,
		})
		require.NoError(t, err)
		claimed, err := db.ClaimJob("session-worker")
		require.NoError(t, err)
		require.Equal(t, job.ID, claimed.ID)
		require.NoError(t, testutil.CompleteReviewFixture(db, job.ID, "gemini", "prompt", "No issues found."))
		_, err = db.ExecContext(t.Context(), `
			UPDATE review_jobs
			SET session_id = ?, session_cli = ?, finished_at = ?
			WHERE id = ?`,
			sessionID, sessionCLI, finishedAt.Format(time.RFC3339Nano), job.ID,
		)
		require.NoError(t, err)
	}
	now := time.Now()
	seedCandidate("older-gemini-session", "gemini", now.Add(-2*time.Minute))
	seedCandidate("newer-agy-session", "agy", now.Add(-time.Minute))

	opts := storage.EnqueueOpts{
		RepoID: repo.ID, GitRef: sha, Branch: "feature/session",
		Agent: "gemini", Model: "gemini-3.1-pro-preview", Reasoning: "thorough",
		ReviewType: config.ReviewTypeDefault,
	}
	sessionID, sessionCLI, _ := findCompatibleReusableSession(
		t.Context(), db, repoRoot, sha, opts, nil, nil, cfg, nil, 0,
	)
	assert.Equal(t, "older-gemini-session", sessionID)
	assert.Equal(t, "gemini", sessionCLI)
}
