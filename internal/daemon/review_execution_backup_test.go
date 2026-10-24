package daemon

import (
	"context"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func TestReviewExecutionLocalPanelUsesCurrentDynamicACPBackup(t *testing.T) {
	for _, fixture := range []struct {
		name      string
		linked    bool
		frozenACP bool
	}{
		{name: "main-frozen-acp", frozenACP: true},
		{name: "linked-frozen-acp", linked: true, frozenACP: true},
		{name: "main-empty-acp"},
		{name: "linked-empty-acp", linked: true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			tc := newWorkerTestContext(t, 1)
			linked, configPath := "", tc.TmpDir
			if fixture.linked {
				linked = filepath.Join(t.TempDir(), "linked")
				tc.GitRepo.RunGit("worktree", "add", "--detach", linked)
				configPath = linked
			}
			command, err := os.Executable()
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(configPath, ".roborev.toml"),
				[]byte("review_backup_agent = 'acp.backup'\nreview_backup_model = 'linked-backup-model'\n[acp.backup]\ncommand = "+strconv.Quote(command)+"\n"), 0o600))
			snapshot := reviewExecutionSnapshot{Agent: "codex"}
			if fixture.frozenACP {
				snapshot.Agent = "acp.primary"
				snapshot.ACP = config.ACPAgentConfigs{"primary": {Command: filepath.Join(t.TempDir(), "unavailable-primary")}}
			}
			raw, err := json.Marshal(snapshot)
			require.NoError(t, err)
			job := &storage.ReviewJob{
				RepoPath: tc.TmpDir, WorktreePath: linked, Agent: snapshot.Agent,
				JobType: storage.JobTypeReview, PanelRole: storage.PanelRoleMember, PanelMemberConfigJSON: string(raw),
			}
			cfg := config.DefaultConfig()
			cfg.CodexCmd = filepath.Join(t.TempDir(), "unavailable-codex")
			tc.reconfigurePool(cfg)
			backup := tc.Pool.resolveBackupAgent(job)
			require.Equal(t, "acp.backup", backup)
			assert.Equal(t, "linked-backup-model", tc.Pool.resolveBackupModel(job))
			selected, err := resolveConfiguredJobAgent(job, cfg, backup)
			require.NoError(t, err)
			assert.Equal(t, "acp.backup", selected.Name())
			acp, ok := selected.(*agent.ACPAgent)
			require.True(t, ok)
			assert.Equal(t, command, acp.CommandName())
			// Explicit failover changes the row's agent but retains its member snapshot.
			failedOver := *job
			failedOver.Agent = backup
			selected, err = resolveConfiguredJobAgent(&failedOver, cfg)
			require.NoError(t, err)
			assert.Equal(t, "acp.backup", selected.Name())
			require.ErrorIs(t, agent.ValidateReviewToolSelection(&config.ReviewToolPolicy{Mode: "read-only"}, selected, false), agent.ErrReviewToolPolicy)
		})
	}
}

func TestReviewExecutionLocalPanelRetainsFrozenBackupAfterConfigChange(t *testing.T) {
	for _, model := range []string{"frozen-backup-model", ""} {
		t.Run("model="+model, func(t *testing.T) {
			assert := assert.New(t)
			server, db, _ := newTestServer(t)
			server.configWatcher.Config().Review = config.ReviewConfig{
				Subagents: map[string]config.SubagentSpec{"reader": {Agent: "test", ReviewType: "default"}},
				Panels:    map[string]config.PanelSpec{"panel": {Members: []string{"reader"}, SynthesisAgent: "test"}},
			}
			repo := testutil.NewGitRepo(t)
			command, err := os.Executable()
			require.NoError(t, err)
			repo.WriteFile(".roborev.toml", "review_backup_agent = 'acp.backup'\nreview_backup_model = "+strconv.Quote(model)+"\n[acp.backup]\ncommand = "+strconv.Quote(command)+"\n")
			repo.CommitFile("review.go", "package review\n", "add review")
			panel := enqueuePanelViaHTTP(t, server, EnqueueRequest{RepoPath: repo.Path(), GitRef: "HEAD", Panel: "panel"})
			require.Len(t, panel.MemberJobIDs, 1)
			claimed, err := db.ClaimJob(testWorkerID)
			require.NoError(t, err)
			require.NotNil(t, claimed)
			require.Equal(t, panel.MemberJobIDs[0], claimed.ID)
			require.Empty(t, claimed.BackupAgent)
			snapshot, err := decodeReviewExecutionSnapshot(claimed.PanelMemberConfigJSON)
			require.NoError(t, err)
			require.Equal(t, "acp.backup", snapshot.BackupAgent)
			require.Equal(t, model, snapshot.BackupModel)
			repo.WriteFile(".roborev.toml", "review_backup_agent = 'acp.replacement'\nreview_backup_model = 'changed-model'\n[acp.replacement]\ncommand = "+strconv.Quote(command)+"\n[acp.backup]\ncommand = 'unavailable-live-backup'\n")

			assert.Equal("acp.backup", server.workerPool.resolveBackupAgent(claimed))
			assert.Equal(model, server.workerPool.resolveBackupModel(claimed))
			server.workerPool.failoverOrFailNonRetryableAgentContext(context.Background(), testWorkerID, claimed, claimed.Agent, "synthetic primary failure")
			failedOver, err := db.GetJobByID(claimed.ID)
			require.NoError(t, err)
			assert.Equal(storage.JobStatusQueued, failedOver.Status)
			assert.Equal("acp.backup", failedOver.Agent)
			assert.Equal(model, failedOver.Model)
			assert.Equal(claimed.PanelMemberConfigJSON, failedOver.PanelMemberConfigJSON)
			selected, err := resolveConfiguredJobAgent(failedOver, server.configWatcher.Config())
			require.NoError(t, err)
			acp, ok := selected.(*agent.ACPAgent)
			require.True(t, ok)
			assert.Equal(command, acp.CommandName())
		})
	}
}

func TestReviewExecutionFrozenACPDoesNotUseLiveBackup(t *testing.T) {
	command, err := os.Executable()
	require.NoError(t, err)
	for _, kind := range []string{"ci", "experiment", "attributed", "explicit"} {
		t.Run(kind, func(t *testing.T) {
			repo := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(repo, ".roborev.toml"), []byte("review_backup_agent = 'acp.backup'\n[acp.backup]\ncommand = "+strconv.Quote(command)+"\n"), 0o600))
			snapshot := reviewExecutionSnapshot{Agent: "acp.primary", ACP: config.ACPAgentConfigs{"primary": {Command: filepath.Join(t.TempDir(), "unavailable-primary")}}}
			job := &storage.ReviewJob{RepoPath: repo, Agent: "acp.primary", JobType: storage.JobTypeReview, PanelRole: storage.PanelRoleMember}
			switch kind {
			case "ci":
				job.Source = storage.JobSourceCI
			case "experiment":
				job.FrozenExperimentPlan = &storage.ExperimentAssignmentInput{}
			case "attributed":
				job.Experiments = []storage.ExperimentAssignment{{}}
			case "explicit":
				job.BackupAgent = "acp.backup"
				snapshot.BackupAgent = "acp.backup"
			}
			raw, err := json.Marshal(snapshot)
			require.NoError(t, err)
			job.PanelMemberConfigJSON = string(raw)
			_, err = resolveConfiguredJobAgent(job, config.DefaultConfig(), "acp.backup")
			require.Error(t, err)
		})
	}
}

func TestReviewExecutionFrozenACPEntriesWinOverLive(t *testing.T) {
	command, err := os.Executable()
	require.NoError(t, err)
	repo := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(repo, ".roborev.toml"), []byte("[acp.primary]\ncommand = 'missing-live-primary'\n[acp.backup]\ncommand = 'missing-live-backup'\n"), 0o600))
	for _, name := range []string{"primary", "backup"} {
		t.Run(name, func(t *testing.T) {
			snapshot := reviewExecutionSnapshot{
				Agent: "acp.primary", BackupAgent: "acp.backup",
				ACP: config.ACPAgentConfigs{"primary": {Command: command}, "backup": {Command: command}},
			}
			raw, err := json.Marshal(snapshot)
			require.NoError(t, err)
			job := &storage.ReviewJob{RepoPath: repo, Agent: "acp." + name, JobType: storage.JobTypeReview, PanelRole: storage.PanelRoleMember, PanelMemberConfigJSON: string(raw)}
			selected, err := resolveConfiguredJobAgent(job, config.DefaultConfig())
			require.NoError(t, err)
			acp, ok := selected.(*agent.ACPAgent)
			require.True(t, ok)
			assert.Equal(t, command, acp.CommandName())
		})
	}
}
