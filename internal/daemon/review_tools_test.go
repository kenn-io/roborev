package daemon

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"io"
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
	"go.kenn.io/roborev/internal/tokens"
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

type strictWorkerTestAgent struct {
	*structuredWorkerTestAgent
	mcpConfig string
	err       error
	onReview  func(mcpConfig string)
}

func (a *strictWorkerTestAgent) WithReasoning(agent.ReasoningLevel) agent.Agent { return a }
func (a *strictWorkerTestAgent) WithAgentic(bool) agent.Agent                   { return a }
func (a *strictWorkerTestAgent) WithModel(string) agent.Agent                   { return a }
func (a *strictWorkerTestAgent) StrictReviewMCPConfig() string                  { return a.mcpConfig }

func (a *strictWorkerTestAgent) WithStrictReview(_ config.ReviewToolPolicy, path string) agent.Agent {
	clone := *a
	clone.mcpConfig = path
	return &clone
}

func (a *strictWorkerTestAgent) ReviewWithSchema(context.Context, string, string, string, jsontext.Value, io.Writer) (jsontext.Value, error) {
	a.onReview(a.mcpConfig)
	return a.result, a.err
}

func TestProcessJobRunsStrictReviewInFreshSession(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
	}{{name: "success"}, {name: "agent failure", err: errors.New("agent crashed")}} {
		t.Run(tt.name, func(t *testing.T) {
			tc := newWorkerTestContext(t, 1)
			require.NoError(t, os.WriteFile(filepath.Join(tc.TmpDir, ".roborev.toml"), []byte(strictCustomTypeConfig), 0o644))
			require.NoError(t, os.WriteFile(filepath.Join(tc.TmpDir, "custom.md"), []byte("Check the citations.\n"), 0o644))
			sha := testutil.GetHeadSHA(t, tc.TmpDir)
			commit, err := tc.DB.GetOrCreateCommit(tc.Repo.ID, sha, "Author", "Subject", time.Now())
			require.NoError(t, err)
			job, err := tc.DB.EnqueueJob(storage.EnqueueOpts{
				RepoID: tc.Repo.ID, CommitID: commit.ID, GitRef: sha, Agent: "strict-tools-session", ReviewType: "custom",
			})
			require.NoError(t, err)
			claimed, err := tc.DB.ClaimJob(testWorkerID)
			require.NoError(t, err)
			_, err = tc.DB.Exec("UPDATE review_jobs SET session_id = 'old-session' WHERE id = ?", job.ID)
			require.NoError(t, err)
			claimed.SessionID = "old-session"

			var mcpConfig string
			agent.RegisterForTest(t, &strictWorkerTestAgent{
				structuredWorkerTestAgent: &structuredWorkerTestAgent{
					name:   "strict-tools-session",
					result: jsontext.Value(`{"schema_version":2,"summary":"No findings.","verdict":"pass","findings":[]}`),
				},
				err: tt.err,
				onReview: func(path string) {
					mcpConfig = path
					assert.FileExists(t, path)
					row, err := tc.DB.GetJobByID(job.ID)
					require.NoError(t, err)
					assert.Empty(t, row.SessionID, "a strict review must not resume a session")
					isolated, err := tc.DB.JobSessionIsolated(t.Context(), job.ID)
					require.NoError(t, err)
					assert.True(t, isolated)
				},
			})

			tc.Pool.processJob(testWorkerID, claimed)

			require.NotEmpty(t, mcpConfig, "the worker must prepare the strict agent")
			assert.NoFileExists(t, mcpConfig)
			if tt.err == nil {
				tc.assertJobStatus(t, job.ID, storage.JobStatusDone)
			}
		})
	}
}

func TestProcessJobRecordsStrictClaudeUsageWithoutSavedSession(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	require.NoError(t, os.WriteFile(filepath.Join(tc.TmpDir, ".roborev.toml"), []byte(strictCustomTypeConfig), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(tc.TmpDir, "custom.md"), []byte("Check the citations.\n"), 0o644))
	bin := t.TempDir()
	stream := filepath.Join(bin, "stream.jsonl")
	require.NoError(t, os.WriteFile(stream, []byte(
		`{"type":"system","subtype":"init","session_id":"unsaved","permissionMode":"dontAsk","tools":["Read","Glob","Grep","StructuredOutput"],"mcp_servers":[]}`+"\n"+
			`{"type":"result","session_id":"unsaved","result":"{\"schema_version\":2,\"summary\":\"No findings.\",\"verdict\":\"pass\",\"findings\":[]}","usage":{"input_tokens":100,"output_tokens":20},"total_cost_usd":0.25}`+"\n",
	), 0o600))
	claude := filepath.Join(bin, "claude")
	require.NoError(t, os.WriteFile(claude, []byte(`#!/bin/sh
case "$1" in
  *etxtbsy*) exit 0 ;;
  --help) echo '--restricted --tools --allowedTools --strict-mcp-config --mcp-config --permission-mode --permission-prompts --no-session-persistence --disable-slash-commands --json-schema'; exit 0 ;;
esac
cat '`+stream+`'
`), 0o700))
	cfg := config.DefaultConfig()
	cfg.ClaudeCodeCmd = claude
	tc.reconfigurePool(cfg)
	// Strict sessions are not saved, so a session lookup finds nothing.
	tc.Pool.tokenUsageFetcher = func(context.Context, string) (*tokens.Usage, error) {
		return nil, tokens.ErrUsageProviderUnavailable
	}
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	commit, err := tc.DB.GetOrCreateCommit(tc.Repo.ID, sha, "Author", "Subject", time.Now())
	require.NoError(t, err)
	job, err := tc.DB.EnqueueJob(storage.EnqueueOpts{
		RepoID: tc.Repo.ID, CommitID: commit.ID, GitRef: sha, Agent: "claude-code", ReviewType: "custom",
	})
	require.NoError(t, err)
	claimed, err := tc.DB.ClaimJob(testWorkerID)
	require.NoError(t, err)

	tc.Pool.processJob(testWorkerID, claimed)

	stored := tc.assertJobStatus(t, job.ID, storage.JobStatusDone)
	usage := tokens.ParseJSON(stored.TokenUsage)
	require.NotNil(t, usage, "strict Claude usage must be recorded")
	assert.Equal(t, int64(100), usage.InputTokens)
	assert.Equal(t, int64(20), usage.OutputTokens)
	assert.True(t, usage.HasCost)
	assert.InDelta(t, 0.25, usage.CostUSD, 1e-9)
}
