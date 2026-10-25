package daemon

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

const strictCustomTypeConfig = `[review.types.custom]
template = "custom.md"

[review.types.custom.tools]
mode = "read-only"
`

func TestHandleEnqueueRejectsStrictToolsWithUnsupportedAgent(t *testing.T) {
	const agentName = "strict-tools-enqueue"
	agent.RegisterForTest(t, &structuredWorkerTestAgent{name: agentName})
	repoDir := t.TempDir()
	testutil.InitTestGitRepo(t, repoDir)
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, ".roborev.toml"), []byte(strictCustomTypeConfig), 0o644))

	db, _ := testutil.OpenTestDBWithDir(t)
	server := NewServer(db, config.DefaultConfig(), "")
	t.Cleanup(func() { _ = server.Close() })
	req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/enqueue", EnqueueRequest{
		RepoPath: repoDir, GitRef: "HEAD", Agent: agentName, ReviewType: "custom",
	})
	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, req)

	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "does not support strict review tools")
	queued, _, _, _, _, _, _, _, _ := db.GetJobCounts()
	assert.Zero(t, queued)
}

func TestProcessJobRejectsStrictToolsBeforeInvokingAgent(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	const agentName = "strict-tools-worker"
	invoked := false
	agent.RegisterForTest(t, &structuredWorkerTestAgent{
		name:     agentName,
		onPrompt: func(string, string) { invoked = true },
	})
	require.NoError(t, os.WriteFile(filepath.Join(tc.TmpDir, ".roborev.toml"), []byte(strictCustomTypeConfig), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(tc.TmpDir, "custom.md"), []byte("Check the citations.\n"), 0o644))

	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	commit, err := tc.DB.GetOrCreateCommit(tc.Repo.ID, sha, "Author", "Subject", time.Now())
	require.NoError(t, err)
	job, err := tc.DB.EnqueueJob(storage.EnqueueOpts{
		RepoID: tc.Repo.ID, CommitID: commit.ID, GitRef: sha, Agent: agentName, ReviewType: "custom",
	})
	require.NoError(t, err)
	claimed, err := tc.DB.ClaimJob(testWorkerID)
	require.NoError(t, err)
	require.Equal(t, job.ID, claimed.ID)

	tc.Pool.processJob(testWorkerID, claimed)

	failed := tc.assertJobStatus(t, job.ID, storage.JobStatusFailed)
	assert.Contains(t, failed.Error, "does not support strict review tools")
	assert.False(t, invoked, "a strict review must not run unrestricted")
}

func TestPanelRerunKeepsFrozenToolPolicy(t *testing.T) {
	server, db, _ := newTestServer(t)
	oldRunUUID, members, synth := enqueueServerPanelRun(t, db, 1)
	_, err := db.Exec(
		"UPDATE review_jobs SET review_type = 'custom', panel_member_config_json = ? WHERE id = ?",
		`{"name":"member","tools":{"mode":"read-only","mcp":{}}}`, members[0].ID,
	)
	require.NoError(t, err)
	member, err := db.GetJobByID(members[0].ID)
	require.NoError(t, err)
	// The type no longer declares tools; the queued member keeps its policy.
	require.NoError(t, os.WriteFile(filepath.Join(member.RepoPath, ".roborev.toml"),
		[]byte("[review.types.custom]\ntemplate = \"custom.md\"\n"), 0o644))

	_, rerunMembers := rerunAndLoadNewRun(t, server, db, oldRunUUID, synth.ID)
	require.Len(t, rerunMembers, 1)
	cfg := config.DefaultConfig()
	tools, err := reviewJobTools(&rerunMembers[0], member.RepoPath, cfg)
	require.NoError(t, err)
	assert.Equal(t, &config.ReviewToolPolicy{Mode: "read-only", MCP: map[string][]string{}}, tools)
}
