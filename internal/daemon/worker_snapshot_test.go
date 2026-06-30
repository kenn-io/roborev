//go:build integration

package daemon

import (
	"context"
	"io"
	"os"
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

// These tests verify the end-to-end diff snapshot flow: the worker
// writes a snapshot file, passes the path to the agent via the prompt,
// and cleans up afterward. They use FakeAgent to capture exactly what
// the agent sees without making real AI calls.

func commitLargeChange(t *testing.T, repo *testutil.TestRepo) string {
	t.Helper()
	var content strings.Builder
	for range 20000 {
		content.WriteString("line ")
		content.WriteString(strings.Repeat("x", 20))
		content.WriteString(" ")
		content.WriteString(strings.Repeat("y", 20))
		content.WriteString("\n")
	}
	return repo.CommitFile("large.txt", content.String(), "large change")
}

func registerFakeAgent(t *testing.T, name string, fn func(ctx context.Context, repoPath, sha, prompt string, w io.Writer) (string, error)) {
	t.Helper()
	orig, err := agent.Get(name)
	require.NoError(t, err)
	agent.Register(&agent.FakeAgent{NameStr: name, ReviewFn: fn})
	t.Cleanup(func() { agent.Register(orig) })
}

func TestSnapshotFlow_SmallDiffInlinesWithoutFile(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)

	var receivedPrompt string
	registerFakeAgent(t, "test", func(_ context.Context, _, _, p string, _ io.Writer) (string, error) {
		receivedPrompt = p
		return "No issues found.", nil
	})

	job := tc.createAndClaimJob(t, sha, testWorkerID)
	tc.Pool.processJob(testWorkerID, job)

	tc.assertJobStatus(t, job.ID, storage.JobStatusDone)
	require.NotEmpty(t, receivedPrompt)

	// Small diff should be inlined
	assert.Contains(t, receivedPrompt, "```diff",
		"small diff should be inlined in prompt")
	assert.NotContains(t, receivedPrompt, "written to a file",
		"small diff should not reference a snapshot file")
	assert.NotContains(t, receivedPrompt, "Read the diff from:",
		"small diff should not have file read instructions")
}

func TestSnapshotFlow_LargeDiffWritesFileAndReferencesInPrompt(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	sha := commitLargeChange(t, tc.GitRepo)

	var receivedPrompt string
	registerFakeAgent(t, "test", func(_ context.Context, _, _, p string, _ io.Writer) (string, error) {
		receivedPrompt = p
		return "No issues found.", nil
	})

	commit, err := tc.DB.GetOrCreateCommit(
		tc.Repo.ID, sha, "Author", "large change", time.Now(),
	)
	require.NoError(t, err)
	job, err := tc.DB.EnqueueJob(storage.EnqueueOpts{
		RepoID: tc.Repo.ID, CommitID: commit.ID,
		GitRef: sha, Agent: "test",
	})
	require.NoError(t, err)
	claimed, err := tc.DB.ClaimJob(testWorkerID)
	require.NoError(t, err)

	tc.Pool.processJob(testWorkerID, claimed)

	tc.assertJobStatus(t, job.ID, storage.JobStatusDone)
	require.NotEmpty(t, receivedPrompt)

	// Prompt should reference a file, not inline the diff
	assert.NotContains(t, receivedPrompt, "```diff",
		"large diff should not be inlined")
	assert.Contains(t, receivedPrompt, "Read the diff from:",
		"large diff should reference snapshot file")
	assert.Contains(t, receivedPrompt, "roborev-snapshot-",
		"prompt should contain the snapshot filename")
}

func TestSnapshotFlow_SnapshotFileContentMatchesDiff(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	sha := commitLargeChange(t, tc.GitRepo)

	var snapshotPath string
	registerFakeAgent(t, "test", func(_ context.Context, _, _, p string, _ io.Writer) (string, error) {
		// Extract the file path from the prompt
		for line := range strings.SplitSeq(p, "\n") {
			if strings.Contains(line, "Read the diff from:") {
				// Line format: "Read the diff from: `/path/to/file`"
				start := strings.Index(line, "`")
				end := strings.LastIndex(line, "`")
				if start >= 0 && end > start {
					snapshotPath = line[start+1 : end]
				}
			}
		}
		return "No issues found.", nil
	})

	commit, err := tc.DB.GetOrCreateCommit(
		tc.Repo.ID, sha, "Author", "large", time.Now(),
	)
	require.NoError(t, err)
	job, err := tc.DB.EnqueueJob(storage.EnqueueOpts{
		RepoID: tc.Repo.ID, CommitID: commit.ID,
		GitRef: sha, Agent: "test",
	})
	require.NoError(t, err)
	claimed, err := tc.DB.ClaimJob(testWorkerID)
	require.NoError(t, err)

	tc.Pool.processJob(testWorkerID, claimed)
	tc.assertJobStatus(t, job.ID, storage.JobStatusDone)

	require.NotEmpty(t, snapshotPath, "agent should have received a snapshot file path")

	// The snapshot file should have existed during the review.
	// After processJob returns, cleanup runs and deletes it.
	// Verify the file was not hidden under .git, where sandboxed agents
	// may be unable to read it.
	gitDir, err := gitpkg.ResolveGitDir(tc.TmpDir)
	require.NoError(t, err)
	assert.False(t, strings.HasPrefix(snapshotPath, gitDir),
		"snapshot should not be in git dir: got %s, git dir %s",
		snapshotPath, gitDir)

	// Verify it was cleaned up
	_, err = os.Stat(snapshotPath)
	assert.True(t, os.IsNotExist(err),
		"snapshot file should be cleaned up after review")
}

func TestSnapshotFlow_SnapshotFileReadableDuringReview(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	sha := commitLargeChange(t, tc.GitRepo)

	var fileContent string
	var fileReadErr error
	registerFakeAgent(t, "test", func(_ context.Context, _, _, p string, _ io.Writer) (string, error) {
		// Extract and read the snapshot file during the review
		for line := range strings.SplitSeq(p, "\n") {
			if strings.Contains(line, "Read the diff from:") {
				start := strings.Index(line, "`")
				end := strings.LastIndex(line, "`")
				if start >= 0 && end > start {
					path := line[start+1 : end]
					data, err := os.ReadFile(path)
					fileContent = string(data)
					fileReadErr = err
				}
			}
		}
		return "No issues found.", nil
	})

	commit, err := tc.DB.GetOrCreateCommit(
		tc.Repo.ID, sha, "Author", "large", time.Now(),
	)
	require.NoError(t, err)
	job, err := tc.DB.EnqueueJob(storage.EnqueueOpts{
		RepoID: tc.Repo.ID, CommitID: commit.ID,
		GitRef: sha, Agent: "test",
	})
	require.NoError(t, err)
	claimed, err := tc.DB.ClaimJob(testWorkerID)
	require.NoError(t, err)

	tc.Pool.processJob(testWorkerID, claimed)
	tc.assertJobStatus(t, job.ID, storage.JobStatusDone)

	require.NoError(t, fileReadErr,
		"agent should be able to read the snapshot file during review")
	assert.NotEmpty(t, fileContent,
		"snapshot file should contain diff content")

	// Verify it contains actual diff content
	expectedDiff, err := gitpkg.GetDiff(tc.TmpDir, sha)
	require.NoError(t, err)
	assert.Equal(t, expectedDiff, fileContent,
		"snapshot file should match git diff output")
}

func TestSnapshotFlow_ExcludePatternsAppliedToSnapshot(t *testing.T) {
	tc := newWorkerTestContext(t, 1)

	sha := tc.GitRepo.CommitFiles(map[string]string{
		"code.go":       strings.Repeat("package main\n", 15000),
		"generated.dat": strings.Repeat("gen\n", 5000),
	}, "mixed change")

	// Configure exclude patterns via global config
	cfg := tc.Pool.cfgGetter.Config()
	cfg.ExcludePatterns = []string{"generated.dat"}
	tc.Pool.cfgGetter = NewStaticConfig(cfg)

	var fileContent string
	registerFakeAgent(t, "test", func(_ context.Context, _, _, p string, _ io.Writer) (string, error) {
		for line := range strings.SplitSeq(p, "\n") {
			if strings.Contains(line, "Read the diff from:") {
				start := strings.Index(line, "`")
				end := strings.LastIndex(line, "`")
				if start >= 0 && end > start {
					data, _ := os.ReadFile(line[start+1 : end])
					fileContent = string(data)
				}
			}
		}
		return "No issues found.", nil
	})

	commit, err := tc.DB.GetOrCreateCommit(
		tc.Repo.ID, sha, "Author", "mixed", time.Now(),
	)
	require.NoError(t, err)
	job, err := tc.DB.EnqueueJob(storage.EnqueueOpts{
		RepoID: tc.Repo.ID, CommitID: commit.ID,
		GitRef: sha, Agent: "test",
	})
	require.NoError(t, err)
	claimed, err := tc.DB.ClaimJob(testWorkerID)
	require.NoError(t, err)

	tc.Pool.processJob(testWorkerID, claimed)
	tc.assertJobStatus(t, job.ID, storage.JobStatusDone)

	require.NotEmpty(t, fileContent, "snapshot should have been written")
	assert.Contains(t, fileContent, "code.go",
		"snapshot should contain non-excluded file")
	assert.NotContains(t, fileContent, "generated.dat",
		"snapshot should exclude configured patterns")
}
