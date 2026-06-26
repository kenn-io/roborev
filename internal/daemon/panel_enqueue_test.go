package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

// panelTOML is a .roborev.toml that defines three subagents (all using the
// always-available "test" agent with distinct review types) and a panel of all
// three, with default_panel/hook_review_panel pointing at it.
const panelTOML = `
[review]
default_panel = "trio"
hook_review_panel = "trio"

[review.subagents.bug]
agent = "test"
review_type = "default"

[review.subagents.security]
agent = "test"
review_type = "security"

[review.subagents.design]
agent = "test"
review_type = "design"

[review.panels.trio]
members = ["bug", "security", "design"]
synthesis_agent = "test"
`

// enqueuePanelViaHTTP posts an enqueue request that selects a panel and decodes
// the PanelEnqueueResponse, asserting a 201.
func enqueuePanelViaHTTP(t *testing.T, server *Server, body EnqueueRequest) PanelEnqueueResponse {
	t.Helper()

	req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/enqueue", body)
	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, req)

	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())

	var resp PanelEnqueueResponse
	testutil.DecodeJSON(t, w, &resp)
	require.NotNil(t, resp.ReviewJob, "panel response must embed the synthesis job")
	return resp
}

func autoDesignRowsForSHA(t *testing.T, db *storage.DB, repoID int64, sha string) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(`
		SELECT COUNT(*)
		  FROM review_jobs j
		  JOIN commits c ON c.id = j.commit_id
		 WHERE j.repo_id = ?
		   AND c.sha = ?
		   AND j.source = 'auto_design'
		   AND j.review_type = 'design'
	`, repoID, sha).Scan(&n))
	return n
}

// TestEnqueuePanelFanout verifies a 3-member panel fans out into exactly three
// member rows (ordered, distinct review types) plus one claim-blocked synthesis
// row, all sharing the run uuid, with the synthesis reasoning from the panel.
func TestEnqueuePanelFanout(t *testing.T) {
	assert := assert.New(t)
	server, db, _ := newTestServer(t)

	repo := testutil.NewGitRepo(t)
	repo.WriteFile(".roborev.toml", panelTOML)
	repo.CommitFile("a.txt", "a", "add a")

	resp := enqueuePanelViaHTTP(t, server, EnqueueRequest{
		RepoPath: repo.Path(),
		GitRef:   "HEAD",
		Agent:    "test",
	})

	assert.NotEmpty(resp.PanelRunUUID)
	assert.Positive(resp.ID, "handle is the synthesis job ID")

	members, err := db.GetPanelMembers(resp.PanelRunUUID)
	require.NoError(t, err)
	require.Len(t, members, 3)

	reviewTypes := make([]string, len(members))
	for i, m := range members {
		assert.Equal(i, m.PanelMemberIndex, "members ordered by index")
		assert.Equal(resp.PanelRunUUID, m.PanelRunUUID)
		assert.Equal(storage.PanelRoleMember, m.PanelRole)
		reviewTypes[i] = m.ReviewType
	}
	assert.ElementsMatch([]string{"default", "security", "design"}, reviewTypes)
	assert.Equal([]int64{members[0].ID, members[1].ID, members[2].ID}, resp.MemberJobIDs)

	synth, err := db.GetJobByID(resp.ID)
	require.NoError(t, err)
	assert.Equal(storage.JobTypeSynthesis, synth.JobType)
	assert.Equal(storage.PanelRoleSynthesis, synth.PanelRole)
	assert.Equal(resp.PanelRunUUID, synth.PanelRunUUID)
	assert.True(synth.ClaimBlocked, "synthesis must be claim-blocked")
	// Resolved fix reasoning (SynthesisSpec.Reasoning); the test config omits an
	// explicit reasoning so it falls back to the standard fix default.
	assert.Equal("standard", synth.Reasoning)

	// A claim-blocked synthesis must never be handed to a worker while its
	// members are still queued; ClaimJob returns a member, never the synthesis.
	claimed, err := db.ClaimJob("worker")
	require.NoError(t, err)
	assert.Equal(storage.PanelRoleMember, claimed.PanelRole,
		"ClaimJob must skip the claim-blocked synthesis row")
}

func TestEnqueuePanelRunBroadcastsAndLogsSynthesisJob(t *testing.T) {
	assert := assert.New(t)
	server, _, _ := newTestServer(t)

	repo := testutil.NewGitRepo(t)
	repo.WriteFile(".roborev.toml", panelTOML)
	sha := repo.CommitFile("a.txt", "a", "add a")

	subID, eventCh := server.broadcaster.Subscribe("")
	defer server.broadcaster.Unsubscribe(subID)

	resp := enqueuePanelViaHTTP(t, server, EnqueueRequest{
		RepoPath: repo.Path(),
		GitRef:   "HEAD",
		Agent:    "test",
	})

	select {
	case event := <-eventCh:
		assert.Equal("job.enqueued", event.Type)
		assert.Equal(resp.ID, event.JobID)
		assert.NotEmpty(event.Repo)
		assert.Equal(sha, event.SHA)
		assert.Equal("test", event.Agent)
	case <-time.After(time.Second):
		require.FailNow(t, "timed out waiting for panel job.enqueued event")
	}

	require.NotNil(t, server.activityLog)
	var enqueuedEntry *ActivityEntry
	wantJobID := strconv.FormatInt(resp.ID, 10)
	for _, entry := range server.activityLog.Recent() {
		if entry.Event == "job.enqueued" && entry.Details["job_id"] == wantJobID {
			enqueuedEntry = &entry
			break
		}
	}
	require.NotNil(t, enqueuedEntry, "panel enqueue should write an activity-log entry")
	assert.Equal("server", enqueuedEntry.Component)
	assert.Equal("test", enqueuedEntry.Details["agent"])
	assert.Equal("HEAD", enqueuedEntry.Details["ref"])
	assert.Equal("default", enqueuedEntry.Details["review_type"])
}

func TestEnqueuePanelPreservesAutoDesignFollowUp(t *testing.T) {
	assert := assert.New(t)
	ResetAutoDesignMetricsForTest()
	t.Cleanup(ResetAutoDesignMetricsForTest)
	server, db, _ := newTestServer(t)

	const autoDesignPanel = `
[review]
default_panel = "default_only"

[review.subagents.default]
agent = "test"
review_type = "default"

[review.panels.default_only]
members = ["default"]
synthesis_agent = "test"

[auto_design_review]
enabled = true
trigger_paths = ["migrations/**"]
`

	repo := testutil.NewGitRepo(t)
	repo.WriteFile(".roborev.toml", autoDesignPanel)
	repo.CommitFile("base.txt", "base", "base")
	sha := repo.CommitFile("migrations/001.sql", "create table t(id integer);\n", "feat: add migration")

	resp := enqueuePanelViaHTTP(t, server, EnqueueRequest{
		RepoPath: repo.Path(),
		GitRef:   sha,
		Agent:    "test",
	})
	members, err := db.GetPanelMembers(resp.PanelRunUUID)
	require.NoError(t, err)
	require.Len(t, members, 1)
	assert.Equal("default", members[0].ReviewType)

	storedRepo, err := db.GetOrCreateRepo(repo.Path())
	require.NoError(t, err)
	assert.Equal(1, autoDesignRowsForSHA(t, db, storedRepo.ID, sha),
		"default panel enqueue should preserve auto-design follow-up coverage")
	assert.EqualValues(1, AutoDesignMetricsSnapshot().TriggeredHeuristic)
}

func TestEnqueuePanelDoesNotDuplicateAutoDesignWhenPanelHasDesignMember(t *testing.T) {
	assert := assert.New(t)
	ResetAutoDesignMetricsForTest()
	t.Cleanup(ResetAutoDesignMetricsForTest)
	server, db, _ := newTestServer(t)

	repo := testutil.NewGitRepo(t)
	repo.WriteFile(".roborev.toml", panelTOML+"\n[auto_design_review]\nenabled = true\ntrigger_paths = [\"migrations/**\"]\n")
	repo.CommitFile("base.txt", "base", "base")
	sha := repo.CommitFile("migrations/001.sql", "create table t(id integer);\n", "feat: add migration")

	resp := enqueuePanelViaHTTP(t, server, EnqueueRequest{
		RepoPath: repo.Path(),
		GitRef:   sha,
		Agent:    "test",
	})
	members, err := db.GetPanelMembers(resp.PanelRunUUID)
	require.NoError(t, err)
	require.Len(t, members, 3)

	storedRepo, err := db.GetOrCreateRepo(repo.Path())
	require.NoError(t, err)
	assert.Equal(0, autoDesignRowsForSHA(t, db, storedRepo.ID, sha),
		"explicit design panel member should satisfy design coverage without an auto_design duplicate")
	assert.EqualValues(0, AutoDesignMetricsSnapshot().TriggeredHeuristic)
}

// TestEnqueuePanelFreezesSHA verifies a symbolic git_ref is frozen to one
// concrete SHA shared by every member and the synthesis row.
func TestEnqueuePanelFreezesSHA(t *testing.T) {
	assert := assert.New(t)
	server, db, _ := newTestServer(t)

	repo := testutil.NewGitRepo(t)
	repo.WriteFile(".roborev.toml", panelTOML)
	sha := repo.CommitFile("a.txt", "a", "add a")

	resp := enqueuePanelViaHTTP(t, server, EnqueueRequest{
		RepoPath: repo.Path(),
		GitRef:   "HEAD",
		Agent:    "test",
	})

	members, err := db.GetPanelMembers(resp.PanelRunUUID)
	require.NoError(t, err)
	require.Len(t, members, 3)
	for _, m := range members {
		assert.Equal(sha, m.GitRef, "members carry the frozen SHA")
	}

	synth, err := db.GetJobByID(resp.ID)
	require.NoError(t, err)
	assert.Equal(sha, synth.GitRef, "synthesis carries the frozen SHA")
}

// TestEnqueuePanelPreservesTarget verifies the frozen target (diff for dirty,
// commit/patch for single-commit) reaches every member.
func TestEnqueuePanelPreservesTarget(t *testing.T) {
	t.Run("dirty", func(t *testing.T) {
		assert := assert.New(t)
		server, db, _ := newTestServer(t)

		repo := testutil.NewGitRepo(t)
		repo.WriteFile(".roborev.toml", panelTOML)
		repo.CommitFile("a.txt", "a", "add a")

		diff := "diff --git a/x b/x\n+change\n"
		resp := enqueuePanelViaHTTP(t, server, EnqueueRequest{
			RepoPath:    repo.Path(),
			GitRef:      "dirty",
			Agent:       "test",
			DiffContent: diff,
		})

		// GetPanelMembers does not hydrate diff_content; ClaimJob does. Drain the
		// queued members and verify each carries the captured diff.
		members, err := db.GetPanelMembers(resp.PanelRunUUID)
		require.NoError(t, err)
		require.Len(t, members, 3)
		for range members {
			claimed, err := db.ClaimJob("worker")
			require.NoError(t, err)
			assert.Equal(storage.PanelRoleMember, claimed.PanelRole)
			require.NotNil(t, claimed.DiffContent)
			assert.Equal(diff, *claimed.DiffContent)
		}
	})

	t.Run("single-commit", func(t *testing.T) {
		assert := assert.New(t)
		server, db, _ := newTestServer(t)

		repo := testutil.NewGitRepo(t)
		repo.WriteFile(".roborev.toml", panelTOML)
		repo.CommitFile("a.txt", "a", "add a")

		resp := enqueuePanelViaHTTP(t, server, EnqueueRequest{
			RepoPath: repo.Path(),
			GitRef:   "HEAD",
			Agent:    "test",
		})

		members, err := db.GetPanelMembers(resp.PanelRunUUID)
		require.NoError(t, err)
		require.Len(t, members, 3)
		for _, m := range members {
			assert.NotZero(m.CommitIDValue(), "member references the commit row")
			assert.NotEmpty(m.PatchID, "member records the patch id")
		}
	})
}

// TestEnqueueProvenanceRouting verifies foreground uses default_panel, an
// automatic source uses hook_review_panel, and no defaults stays single-agent.
func TestEnqueueProvenanceRouting(t *testing.T) {
	const twoPanels = `
[review]
default_panel = "fg"
hook_review_panel = "hook"

[review.subagents.bug]
agent = "test"
review_type = "default"

[review.subagents.security]
agent = "test"
review_type = "security"

[review.panels.fg]
members = ["bug", "security"]
synthesis_agent = "test"

[review.panels.hook]
members = ["bug"]
synthesis_agent = "test"
`

	t.Run("foreground uses default_panel", func(t *testing.T) {
		assert := assert.New(t)
		server, db, _ := newTestServer(t)
		repo := testutil.NewGitRepo(t)
		repo.WriteFile(".roborev.toml", twoPanels)
		repo.CommitFile("a.txt", "a", "add a")

		resp := enqueuePanelViaHTTP(t, server, EnqueueRequest{
			RepoPath: repo.Path(),
			GitRef:   "HEAD",
			Agent:    "test",
		})
		assert.Equal("fg", resp.PanelName)
		members, err := db.GetPanelMembers(resp.PanelRunUUID)
		require.NoError(t, err)
		assert.Len(members, 2, "default_panel fg has two members")
	})

	t.Run("post_commit uses hook_review_panel", func(t *testing.T) {
		assert := assert.New(t)
		server, db, _ := newTestServer(t)
		repo := testutil.NewGitRepo(t)
		repo.WriteFile(".roborev.toml", twoPanels)
		repo.CommitFile("a.txt", "a", "add a")

		resp := enqueuePanelViaHTTP(t, server, EnqueueRequest{
			RepoPath: repo.Path(),
			GitRef:   "HEAD",
			Agent:    "test",
			Source:   "post_commit",
		})
		assert.Equal("hook", resp.PanelName)
		members, err := db.GetPanelMembers(resp.PanelRunUUID)
		require.NoError(t, err)
		assert.Len(members, 1, "hook_review_panel hook has one member")
	})

	t.Run("no defaults stays single-agent", func(t *testing.T) {
		assert := assert.New(t)
		server, _, _ := newTestServer(t)
		repo := testutil.NewGitRepo(t)
		repo.CommitFile("a.txt", "a", "add a")

		job := enqueueViaHTTP(t, server, EnqueueRequest{
			RepoPath: repo.Path(),
			GitRef:   "HEAD",
			Agent:    "test",
		})
		assert.Empty(job.PanelRunUUID, "no panel run without configured defaults")
		assert.Empty(job.PanelRole)
	})
}

// TestEnqueuePanelWhenDefaultAgentUnavailable verifies a panel still enqueues
// even when the requested single-review agent is unavailable, because panel
// selection precedes the single-agent availability gate.
func TestEnqueuePanelWhenDefaultAgentUnavailable(t *testing.T) {
	assert := assert.New(t)
	server, db, _ := newTestServer(t)

	repo := testutil.NewGitRepo(t)
	repo.WriteFile(".roborev.toml", panelTOML)
	repo.CommitFile("a.txt", "a", "add a")

	req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/enqueue", EnqueueRequest{
		RepoPath: repo.Path(),
		GitRef:   "HEAD",
		Agent:    "nonexistent-xyz",
	})
	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, req)

	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	assert.NotEqual(http.StatusServiceUnavailable, w.Code)

	var resp PanelEnqueueResponse
	testutil.DecodeJSON(t, w, &resp)
	require.NotNil(t, resp.ReviewJob)
	members, err := db.GetPanelMembers(resp.PanelRunUUID)
	require.NoError(t, err)
	assert.Len(members, 3, "panel members enqueued despite unavailable single agent")
	synth, err := db.GetJobByID(resp.ID)
	require.NoError(t, err)
	assert.Equal(storage.JobTypeSynthesis, synth.JobType)
}

func TestEnqueuePanelMemberUsesBackupModelWhenPreferredUnavailable(t *testing.T) {
	assert := assert.New(t)
	server, db, _ := newTestServer(t)

	const primaryAgent = "panel-unavailable-primary"
	agent.Register(&unavailableSynthesisCommandAgent{
		name:    primaryAgent,
		command: "roborev-missing-panel-primary",
	})
	t.Cleanup(func() { agent.Unregister(primaryAgent) })

	const panelWithBackup = `
review_backup_agent = "test"
review_backup_model = "backup-model"

[review]
default_panel = "solo"

[review.subagents.only]
agent = "panel-unavailable-primary"
review_type = "default"

[review.panels.solo]
members = ["only"]
synthesis_agent = "test"
`
	repo := testutil.NewGitRepo(t)
	repo.WriteFile(".roborev.toml", panelWithBackup)
	repo.CommitFile("a.txt", "a", "add a")

	resp := enqueuePanelViaHTTP(t, server, EnqueueRequest{
		RepoPath: repo.Path(),
		GitRef:   "HEAD",
		Agent:    "test",
	})

	members, err := db.GetPanelMembers(resp.PanelRunUUID)
	require.NoError(t, err)
	require.Len(t, members, 1)
	assert.Equal("test", members[0].Agent)
	assert.Equal("backup-model", members[0].Model)
}

// TestEnqueuePanelLookaheadMemberUsesAnalyzeConfig verifies that a lookahead
// panel member with no explicit agent resolves its agent/model from the generic
// per-type [analyze.lookahead] config. The lookahead review type carries no
// bespoke workflow fields, so this exercises the generic override path: if the
// member's review type did not key into [analyze.lookahead], the agent/model
// would fall back to the generic defaults instead.
func TestEnqueuePanelLookaheadMemberUsesAnalyzeConfig(t *testing.T) {
	assert := assert.New(t)
	server, db, _ := newTestServer(t)

	const panelWithAnalyzeLookahead = `
[analyze.lookahead]
agent = "test"
model = "lookahead-model"

[review]
default_panel = "solo"

[review.subagents.only]
review_type = "lookahead"

[review.panels.solo]
members = ["only"]
synthesis_agent = "test"
`
	repo := testutil.NewGitRepo(t)
	repo.WriteFile(".roborev.toml", panelWithAnalyzeLookahead)
	repo.CommitFile("a.txt", "a", "add a")

	resp := enqueuePanelViaHTTP(t, server, EnqueueRequest{
		RepoPath: repo.Path(),
		GitRef:   "HEAD",
		Agent:    "test",
	})

	members, err := db.GetPanelMembers(resp.PanelRunUUID)
	require.NoError(t, err)
	require.Len(t, members, 1)
	assert.Equal("test", members[0].Agent,
		"lookahead member should resolve its agent from [analyze.lookahead]")
	assert.Equal("lookahead-model", members[0].Model,
		"lookahead member should resolve its model from [analyze.lookahead]")
}

func TestEnqueuePanelOmittedMemberAgentAutoDetectsAvailableAgent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("minimal PATH setup uses POSIX symlink")
	}
	assert := assert.New(t)
	server, db, _ := newTestServer(t)

	const primaryAgent = "panel-unavailable-default"
	agent.Register(&unavailableSynthesisCommandAgent{
		name:    primaryAgent,
		command: "roborev-missing-panel-default",
	})
	t.Cleanup(func() { agent.Unregister(primaryAgent) })
	agent.Register(&agent.FakeAgent{NameStr: "panel-auto-detected"})
	t.Cleanup(func() { agent.Unregister("panel-auto-detected") })

	gitPath, gitErr := exec.LookPath("git")
	require.NoError(t, gitErr)
	binDir := t.TempDir()
	require.NoError(t, os.Symlink(gitPath, filepath.Join(binDir, "git")))
	t.Setenv("PATH", binDir)

	const panelWithOmittedAgent = `
agent = "panel-unavailable-default"

[review]
default_panel = "solo"

[review.subagents.only]
review_type = "default"

[review.panels.solo]
members = ["only"]
synthesis_agent = "test"
`
	repo := testutil.NewGitRepo(t)
	repo.WriteFile(".roborev.toml", panelWithOmittedAgent)
	repo.CommitFile("a.txt", "a", "add a")

	resp := enqueuePanelViaHTTP(t, server, EnqueueRequest{
		RepoPath: repo.Path(),
		GitRef:   "HEAD",
	})

	members, err := db.GetPanelMembers(resp.PanelRunUUID)
	require.NoError(t, err)
	require.Len(t, members, 1)
	assert.Equal("panel-auto-detected", members[0].Agent)
}

// TestEnqueuePanelSynthesisBackupPersisted verifies a panel's
// synthesis_backup_agent/synthesis_backup_model are written to the synthesis
// job row so the worker can prefer them on synthesis failover.
func TestEnqueuePanelSynthesisBackupPersisted(t *testing.T) {
	assert := assert.New(t)
	server, db, _ := newTestServer(t)

	const backupPanel = `
[review]
default_panel = "trio"

[review.subagents.bug]
agent = "test"
review_type = "default"

[review.subagents.security]
agent = "test"
review_type = "security"

[review.panels.trio]
members = ["bug", "security"]
synthesis_agent = "test"
synthesis_backup_agent = "claude-code"
synthesis_backup_model = "opus"
`
	repo := testutil.NewGitRepo(t)
	repo.WriteFile(".roborev.toml", backupPanel)
	repo.CommitFile("a.txt", "a", "add a")

	resp := enqueuePanelViaHTTP(t, server, EnqueueRequest{
		RepoPath: repo.Path(),
		GitRef:   "HEAD",
		Agent:    "test",
	})

	synth, err := db.GetSynthesisJob(resp.PanelRunUUID)
	require.NoError(t, err)
	require.NotNil(t, synth)
	assert.Equal("claude-code", synth.BackupAgent, "synthesis backup agent persisted")
	assert.Equal("opus", synth.BackupModel, "synthesis backup model persisted")
}

// TestEnqueueSingleMemberPanelSynthesisBackupPersisted verifies the single-member
// override block that surfaces the member's agent/model on the parent row does
// NOT clear the synthesis backup agent/model.
func TestEnqueueSingleMemberPanelSynthesisBackupPersisted(t *testing.T) {
	assert := assert.New(t)
	server, db, _ := newTestServer(t)

	const soloBackup = `
[review]
default_panel = "solo"

[review.subagents.only]
agent = "test"
model = "member-model"
reasoning = "fast"
review_type = "default"

[review.panels.solo]
members = ["only"]
synthesis_agent = "synthesis-exec"
synthesis_model = "synth-model"
synthesis_backup_agent = "claude-code"
synthesis_backup_model = "opus"
`
	repo := testutil.NewGitRepo(t)
	repo.WriteFile(".roborev.toml", soloBackup)
	repo.CommitFile("a.txt", "a", "add a")

	resp := enqueuePanelViaHTTP(t, server, EnqueueRequest{
		RepoPath: repo.Path(),
		GitRef:   "HEAD",
		Agent:    "test",
	})

	synth, err := db.GetSynthesisJob(resp.PanelRunUUID)
	require.NoError(t, err)
	require.NotNil(t, synth)
	assert.Equal("synthesis-exec", synth.Agent, "single-member synthesis keeps execution agent")
	assert.Equal("synth-model", synth.Model, "single-member synthesis keeps execution model")
	assert.Equal("standard", synth.Reasoning, "single-member synthesis keeps execution reasoning")
	assert.Equal("claude-code", synth.BackupAgent, "single-member must not clear backup agent")
	assert.Equal("opus", synth.BackupModel, "single-member must not clear backup model")
}

// TestEnqueueSingleMemberSynthesisKeepsExecutionAgent verifies a one-member
// panel keeps the configured synthesis agent on the parent row. Member display
// identity remains available from the member rows.
func TestEnqueueSingleMemberSynthesisKeepsExecutionAgent(t *testing.T) {
	assert := assert.New(t)
	server, db, _ := newTestServer(t)

	const single = `
[review]
default_panel = "solo"

[review.subagents.only]
agent = "test"
model = "member-model"
review_type = "default"

[review.panels.solo]
members = ["only"]
synthesis_agent = "synthesis-exec"
`
	repo := testutil.NewGitRepo(t)
	repo.WriteFile(".roborev.toml", single)
	repo.CommitFile("a.txt", "a", "add a")

	resp := enqueuePanelViaHTTP(t, server, EnqueueRequest{
		RepoPath: repo.Path(),
		GitRef:   "HEAD",
		Agent:    "test",
	})

	members, err := db.GetPanelMembers(resp.PanelRunUUID)
	require.NoError(t, err)
	require.Len(t, members, 1)

	synth, err := db.GetJobByID(resp.ID)
	require.NoError(t, err)
	assert.Equal("test", members[0].Agent, "member row carries the member's agent")
	assert.Equal("synthesis-exec", synth.Agent, "parent row carries the synthesis execution agent")
}

// TestEnqueuePanelUndefinedIsHardError verifies an undefined --panel is a 400
// that creates no jobs.
func TestEnqueuePanelUndefinedIsHardError(t *testing.T) {
	assert := assert.New(t)
	server, db, _ := newTestServer(t)

	repo := testutil.NewGitRepo(t)
	repo.CommitFile("a.txt", "a", "add a")

	req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/enqueue", EnqueueRequest{
		RepoPath: repo.Path(),
		GitRef:   "HEAD",
		Agent:    "test",
		Panel:    "nope",
	})
	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, req)

	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())

	jobs, err := db.ListJobs("", "", 100, 0)
	require.NoError(t, err)
	assert.Empty(jobs, "undefined panel must not create a job")
}

// TestEnqueuePanelNoneForcesSingle verifies Panel="none" forces a single-agent
// job even with a default_panel configured.
func TestEnqueuePanelNoneForcesSingle(t *testing.T) {
	assert := assert.New(t)
	server, db, _ := newTestServer(t)

	repo := testutil.NewGitRepo(t)
	repo.WriteFile(".roborev.toml", panelTOML)
	repo.CommitFile("a.txt", "a", "add a")

	job := enqueueViaHTTP(t, server, EnqueueRequest{
		RepoPath: repo.Path(),
		GitRef:   "HEAD",
		Agent:    "test",
		Panel:    "none",
	})

	assert.Empty(job.PanelRunUUID, "none forces a single-agent job")
	assert.Empty(job.PanelRole)

	stored, err := db.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Equal(storage.JobTypeReview, stored.JobType)
	assert.Empty(stored.PanelRunUUID)
}

// TestPostCommitStaysSingleAgent verifies an automatic source with a
// default_panel but no hook_review_panel stays single-agent (no fan-out).
func TestPostCommitStaysSingleAgent(t *testing.T) {
	assert := assert.New(t)
	server, db, _ := newTestServer(t)

	const defaultOnly = `
[review]
default_panel = "trio"

[review.subagents.bug]
agent = "test"
review_type = "default"

[review.subagents.security]
agent = "test"
review_type = "security"

[review.subagents.design]
agent = "test"
review_type = "design"

[review.panels.trio]
members = ["bug", "security", "design"]
synthesis_agent = "test"
`
	repo := testutil.NewGitRepo(t)
	repo.WriteFile(".roborev.toml", defaultOnly)
	repo.CommitFile("a.txt", "a", "add a")

	job := enqueueViaHTTP(t, server, EnqueueRequest{
		RepoPath: repo.Path(),
		GitRef:   "HEAD",
		Agent:    "test",
		Source:   "post_commit",
	})

	assert.Empty(job.PanelRunUUID, "post_commit with no hook_review_panel stays single-agent")
	stored, err := db.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Equal(storage.JobTypeReview, stored.JobType)
}

// TestPostCommitBodyCarriesSource verifies the post-commit hook marshals
// source="post_commit" so routing sees the automatic provenance.
func TestPostCommitBodyCarriesSource(t *testing.T) {
	body, err := json.Marshal(EnqueueRequest{
		RepoPath: "/repo",
		GitRef:   "HEAD",
		Source:   "post_commit",
	})
	require.NoError(t, err)
	assert.Contains(t, string(body), `"source":"post_commit"`)
}

func TestPostCommitHookAutoDesignUsesHTTPSource(t *testing.T) {
	assert := assert.New(t)
	ResetAutoDesignMetricsForTest()
	t.Cleanup(ResetAutoDesignMetricsForTest)
	server, db, _ := newTestServer(t)

	const hookOnlyAutoDesign = `
[auto_design_review]
hook_enabled = true
trigger_paths = ["migrations/**"]
`
	repo := testutil.NewGitRepo(t)
	repo.WriteFile(".roborev.toml", hookOnlyAutoDesign)
	repo.CommitFile("base.txt", "base", "base")
	sha := repo.CommitFile("migrations/001.sql", "create table t(id integer);\n", "feat: add migration")

	job := enqueueViaHTTP(t, server, EnqueueRequest{
		RepoPath: repo.Path(),
		GitRef:   sha,
		Agent:    "test",
		Source:   "post_commit",
	})
	assert.Equal("post_commit", job.Source)

	stored, err := db.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Equal("post_commit", stored.Source)

	storedRepo, err := db.GetOrCreateRepo(repo.Path())
	require.NoError(t, err)
	assert.Equal(1, autoDesignRowsForSHA(t, db, storedRepo.ID, sha),
		"hook-only auto-design should see the post_commit source from HTTP enqueue")
	assert.EqualValues(1, AutoDesignMetricsSnapshot().TriggeredHeuristic)
}

// TestEnqueueStoredPromptSkipsPanel verifies a stored-prompt job (run/analyze/
// compact) is never fanned into a review panel, even with default_panel set:
// the panel synthesis worker assumes member code reviews, not prompt output.
func TestEnqueueStoredPromptSkipsPanel(t *testing.T) {
	assert := assert.New(t)
	server, db, _ := newTestServer(t)

	repo := testutil.NewGitRepo(t)
	repo.WriteFile(".roborev.toml", panelTOML)
	repo.CommitFile("a.txt", "a", "add a")

	job := enqueueViaHTTP(t, server, EnqueueRequest{
		RepoPath:     repo.Path(),
		GitRef:       "prompt",
		Agent:        "test",
		JobType:      storage.JobTypeTask,
		CustomPrompt: "Summarize the recent changes.",
	})

	assert.Empty(job.PanelRunUUID, "stored-prompt job must not fan into a panel")
	assert.Empty(job.PanelRole)

	stored, err := db.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Equal(storage.JobTypeTask, stored.JobType)
	assert.Empty(stored.PanelRunUUID)
}
