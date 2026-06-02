package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
)

// findOtherPanelRunUUID returns the single panel_run_uuid present in the DB that
// is not exclude, failing if there is not exactly one. Used to locate the new
// run a rerun creates.
func findOtherPanelRunUUID(t *testing.T, db *storage.DB, exclude string) string {
	t.Helper()
	rows, err := db.Query(
		"SELECT DISTINCT panel_run_uuid FROM review_jobs WHERE panel_run_uuid != '' AND panel_run_uuid != ?",
		exclude,
	)
	require.NoError(t, err)
	defer rows.Close()
	var uuids []string
	for rows.Next() {
		var u string
		require.NoError(t, rows.Scan(&u))
		uuids = append(uuids, u)
	}
	require.NoError(t, rows.Err())
	require.Len(t, uuids, 1, "expected exactly one new panel run")
	return uuids[0]
}

// markJobStatus forces a job's status, used to stage a terminal synthesis (a
// completed panel) before exercising rerun.
func markJobStatus(t *testing.T, db *storage.DB, jobID int64, status storage.JobStatus) {
	t.Helper()
	_, err := db.Exec("UPDATE review_jobs SET status = ? WHERE id = ?", status, jobID)
	require.NoError(t, err)
}

// rerunAndLoadNewRun marks the synthesis job done (rerun requires a terminal
// parent), reruns it, locates the single new panel run the rerun creates, and
// returns that run's UUID and members.
func rerunAndLoadNewRun(
	t *testing.T, server *Server, db *storage.DB, oldUUID string, synthID int64,
) (string, []storage.ReviewJob) {
	t.Helper()
	markJobStatus(t, db, synthID, storage.JobStatusDone)
	_, err := server.humaRerunJob(context.Background(), &RerunJobInput{
		Body: RerunJobRequest{JobID: synthID},
	})
	require.NoError(t, err)
	newUUID := findOtherPanelRunUUID(t, db, oldUUID)
	require.NotEqual(t, oldUUID, newUUID)
	newMembers, err := db.GetPanelMembers(newUUID)
	require.NoError(t, err)
	return newUUID, newMembers
}

// TestRerunSynthesisRejectsNonTerminal verifies a queued/blocked synthesis (an
// in-flight panel) cannot be rerun into a second active run.
func TestRerunSynthesisRejectsNonTerminal(t *testing.T) {
	server, db, _ := newTestServer(t)
	runUUID, _, synth := enqueueServerPanelRun(t, db, 2)

	// synth is queued + claim-blocked (members still pending), not terminal.
	_, err := server.humaRerunJob(context.Background(), &RerunJobInput{
		Body: RerunJobRequest{JobID: synth.ID},
	})
	require.Error(t, err, "rerunning a non-terminal synthesis must be rejected")

	var count int
	require.NoError(t, db.QueryRow(
		"SELECT COUNT(DISTINCT panel_run_uuid) FROM review_jobs WHERE panel_run_uuid != ''",
	).Scan(&count))
	assert.Equal(t, 1, count, "rejected rerun must not create a second panel run")
	assert.NotEmpty(t, runUUID)
}

func TestRerunPanelMemberRejectsDirectRerun(t *testing.T) {
	server, db, _ := newTestServer(t)
	runUUID, members, _ := enqueueServerPanelRun(t, db, 2)
	markJobStatus(t, db, members[0].ID, storage.JobStatusDone)

	_, err := server.humaRerunJob(context.Background(), &RerunJobInput{
		Body: RerunJobRequest{JobID: members[0].ID},
	})
	require.Error(t, err, "panel members must not rerun independently")

	got, err := db.GetJobByID(members[0].ID)
	require.NoError(t, err)
	assert.Equal(t, storage.JobStatusDone, got.Status, "member status should be unchanged")

	var count int
	require.NoError(t, db.QueryRow(
		"SELECT COUNT(DISTINCT panel_run_uuid) FROM review_jobs WHERE panel_run_uuid != ''",
	).Scan(&count))
	assert.Equal(t, 1, count, "direct member rerun must not create a new panel run")
	assert.NotEmpty(t, runUUID)
}

func TestRerunSynthesisCreatesNewRun(t *testing.T) {
	assert := assert.New(t)
	server, db, _ := newTestServer(t)
	repo, err := db.GetOrCreateRepo(t.TempDir())
	require.NoError(t, err)
	commit, err := db.GetOrCreateCommit(repo.ID, "abc123", "A", "S", time.Now())
	require.NoError(t, err)

	// Distinct resolved fields per member so the clone assertions below would
	// fail if panelRerunMemberOpts dropped any of agent/model/provider/
	// reasoning/review_type/config.
	oldUUID := uuid.NewString()
	mkMember := func(name string, idx int, agent, model, provider, reasoning, reviewType string) storage.EnqueueOpts {
		return storage.EnqueueOpts{
			RepoID:                repo.ID,
			CommitID:              commit.ID,
			GitRef:                "abc123",
			Agent:                 agent,
			Model:                 model,
			Provider:              provider,
			Reasoning:             reasoning,
			ReviewType:            reviewType,
			JobType:               storage.JobTypeReview,
			PanelRunUUID:          oldUUID,
			PanelRole:             storage.PanelRoleMember,
			PanelName:             "panel",
			PanelMemberName:       name,
			PanelMemberIndex:      idx,
			PanelMemberConfigJSON: `{"name":"` + name + `","agent":"` + agent + `"}`,
		}
	}
	srcMembers := []storage.EnqueueOpts{
		mkMember("m0", 0, "agent-a", "model-a", "prov-a", "thorough", "security"),
		mkMember("m1", 1, "agent-b", "model-b", "prov-b", "fast", ""),
	}
	srcSynth := storage.EnqueueOpts{
		RepoID: repo.ID, CommitID: commit.ID, GitRef: "abc123",
		Agent: "synth-agent", PanelRunUUID: oldUUID,
		PanelRole: storage.PanelRoleSynthesis, PanelName: "panel",
	}
	_, oldSynth, err := db.EnqueuePanelRun(srcMembers, srcSynth)
	require.NoError(t, err)

	newUUID, newMembers := rerunAndLoadNewRun(t, server, db, oldUUID, oldSynth.ID)

	// Old run is untouched; use its hydrated rows as the copy baseline so the
	// comparison is apples-to-apples (same query path, post-insert normalized).
	oldSynthAfter, err := db.GetSynthesisJob(oldUUID)
	require.NoError(t, err)
	assert.Equal(oldSynth.ID, oldSynthAfter.ID, "old synthesis row preserved")
	oldMembers, err := db.GetPanelMembers(oldUUID)
	require.NoError(t, err)
	require.Len(t, newMembers, len(oldMembers))

	for i := range newMembers {
		old, got := oldMembers[i], newMembers[i]
		assert.NotEqual(old.ID, got.ID, "rerun member is a fresh row")
		assert.Equal(old.PanelMemberName, got.PanelMemberName, "member name copied")
		assert.Equal(old.PanelMemberIndex, got.PanelMemberIndex, "member index copied")
		assert.Equal(old.Agent, got.Agent, "agent copied")
		assert.Equal(old.Model, got.Model, "model copied")
		assert.Equal(old.Provider, got.Provider, "provider copied")
		assert.Equal(old.Reasoning, got.Reasoning, "reasoning copied")
		assert.Equal(old.ReviewType, got.ReviewType, "review_type copied")
		assert.Equal(old.PanelMemberConfigJSON, got.PanelMemberConfigJSON, "member config copied")
		assert.Equal(storage.JobStatusQueued, got.Status, "rerun members start queued")
	}

	newSynth, err := db.GetSynthesisJob(newUUID)
	require.NoError(t, err)
	assert.True(newSynth.IsSynthesisJob())
	assert.True(newSynth.ClaimBlocked, "new synthesis re-blocked until members finish")
}

func TestRerunPanelPreservesStoredPrompt(t *testing.T) {
	assert := assert.New(t)
	server, db, _ := newTestServer(t)
	repo, err := db.GetOrCreateRepo(t.TempDir())
	require.NoError(t, err)

	// A default_panel applied to a stored-prompt command (run/analyze/compact)
	// fans the prompt out onto each member. The worker hard-fails a stored-prompt
	// job whose prompt is empty, so the rerun must carry the prompt across.
	const prompt = "Custom task: analyze the migration plan."
	runUUID := uuid.NewString()
	mkMember := func(name string, idx int) storage.EnqueueOpts {
		return storage.EnqueueOpts{
			RepoID:           repo.ID,
			GitRef:           "task",
			JobType:          storage.JobTypeTask,
			Prompt:           prompt,
			Agent:            "test",
			PanelRunUUID:     runUUID,
			PanelRole:        storage.PanelRoleMember,
			PanelName:        "p",
			PanelMemberName:  name,
			PanelMemberIndex: idx,
		}
	}
	members := []storage.EnqueueOpts{mkMember("m0", 0), mkMember("m1", 1)}
	synth := storage.EnqueueOpts{
		RepoID: repo.ID, GitRef: "task", JobType: storage.JobTypeTask,
		Prompt: prompt, Agent: "test", PanelRunUUID: runUUID,
		PanelRole: storage.PanelRoleSynthesis, PanelName: "p",
	}
	_, synthJob, err := db.EnqueuePanelRun(members, synth)
	require.NoError(t, err)

	_, newMembers := rerunAndLoadNewRun(t, server, db, runUUID, synthJob.ID)
	require.Len(t, newMembers, 2)
	for _, m := range newMembers {
		assert.Equal(prompt, m.Prompt, "stored prompt copied to rerun member")
		assert.Equal(storage.JobTypeTask, m.JobType, "stored-prompt job type preserved")
	}
}

func TestRerunPanelClearsPrebuiltReviewPrompt(t *testing.T) {
	assert := assert.New(t)
	server, db, _ := newTestServer(t)
	repo, err := db.GetOrCreateRepo(t.TempDir())
	require.NoError(t, err)

	const prompt = "prebuilt CI prompt with PR context"
	runUUID := uuid.NewString()
	members := []storage.EnqueueOpts{
		{
			RepoID: repo.ID, GitRef: "base..head", JobType: storage.JobTypeRange,
			Prompt: prompt, PromptPrebuilt: true, Agent: "test",
			PanelRunUUID: runUUID, PanelRole: storage.PanelRoleMember,
			PanelName: "ci", PanelMemberName: "bug", PanelMemberIndex: 0,
		},
	}
	synth := storage.EnqueueOpts{
		RepoID: repo.ID, GitRef: "base..head", Agent: "test",
		PanelRunUUID: runUUID, PanelRole: storage.PanelRoleSynthesis, PanelName: "ci",
	}
	_, synthJob, err := db.EnqueuePanelRun(members, synth)
	require.NoError(t, err)

	_, newMembers := rerunAndLoadNewRun(t, server, db, runUUID, synthJob.ID)
	require.Len(t, newMembers, 1)
	assert.Empty(newMembers[0].Prompt, "review prompt should be rebuilt on rerun")
	assert.False(newMembers[0].PromptPrebuilt, "prebuilt prompt flag should be cleared")
}

func TestRerunPanelPreservesSynthesisBackup(t *testing.T) {
	assert := assert.New(t)
	server, db, _ := newTestServer(t)
	repo, err := db.GetOrCreateRepo(t.TempDir())
	require.NoError(t, err)

	runUUID := uuid.NewString()
	members := []storage.EnqueueOpts{
		{
			RepoID: repo.ID, GitRef: "abc123", Agent: "test",
			PanelRunUUID: runUUID, PanelRole: storage.PanelRoleMember,
			PanelName: "p", PanelMemberName: "m0", PanelMemberIndex: 0,
		},
	}
	synth := storage.EnqueueOpts{
		RepoID: repo.ID, GitRef: "abc123", Agent: "primary",
		BackupAgent: "backup", BackupModel: "backup-model",
		PanelRunUUID: runUUID, PanelRole: storage.PanelRoleSynthesis, PanelName: "p",
	}
	_, synthJob, err := db.EnqueuePanelRun(members, synth)
	require.NoError(t, err)

	newUUID, _ := rerunAndLoadNewRun(t, server, db, runUUID, synthJob.ID)
	newSynth, err := db.GetSynthesisJob(newUUID)
	require.NoError(t, err)
	assert.Equal("backup", newSynth.BackupAgent)
	assert.Equal("backup-model", newSynth.BackupModel)
}

// TestRerunPanelPreservesOutputPrefix verifies a prefixed panel (analyze/compact
// stamp OutputPrefix) keeps its context header across a rerun. The prefix must be
// hydrated by GetPanelMembers/GetSynthesisJob and copied by the rerun opts, or
// CompleteJob would prepend an empty header on the new run.
func TestRerunPanelPreservesOutputPrefix(t *testing.T) {
	assert := assert.New(t)
	server, db, _ := newTestServer(t)
	repo, err := db.GetOrCreateRepo(t.TempDir())
	require.NoError(t, err)

	const memberPrefix = "Member context header\n\n"
	const synthPrefix = "Synthesis context header\n\n"
	runUUID := uuid.NewString()
	mkMember := func(name string, idx int) storage.EnqueueOpts {
		return storage.EnqueueOpts{
			RepoID: repo.ID, GitRef: "task", JobType: storage.JobTypeTask,
			Prompt: "p", OutputPrefix: memberPrefix, Agent: "test",
			PanelRunUUID: runUUID, PanelRole: storage.PanelRoleMember,
			PanelName: "p", PanelMemberName: name, PanelMemberIndex: idx,
		}
	}
	members := []storage.EnqueueOpts{mkMember("m0", 0), mkMember("m1", 1)}
	synth := storage.EnqueueOpts{
		RepoID: repo.ID, GitRef: "task", JobType: storage.JobTypeTask,
		Prompt: "p", OutputPrefix: synthPrefix, Agent: "test",
		PanelRunUUID: runUUID, PanelRole: storage.PanelRoleSynthesis, PanelName: "p",
	}
	_, synthJob, err := db.EnqueuePanelRun(members, synth)
	require.NoError(t, err)

	newUUID, newMembers := rerunAndLoadNewRun(t, server, db, runUUID, synthJob.ID)
	require.Len(t, newMembers, 2)
	for _, m := range newMembers {
		assert.Equal(memberPrefix, m.OutputPrefix, "member output_prefix copied to rerun")
	}
	newSynth, err := db.GetSynthesisJob(newUUID)
	require.NoError(t, err)
	assert.Equal(synthPrefix, newSynth.OutputPrefix, "synthesis output_prefix copied to rerun")
}

func TestRerunPanelPreservesTarget(t *testing.T) {
	t.Run("dirty", func(t *testing.T) {
		assert := assert.New(t)
		server, db, _ := newTestServer(t)
		repo, err := db.GetOrCreateRepo(t.TempDir())
		require.NoError(t, err)

		const diff = "diff --git a/x b/x\n+dirty change\n"
		runUUID := uuid.NewString()
		mkMember := func(name string, idx int) storage.EnqueueOpts {
			return storage.EnqueueOpts{
				RepoID: repo.ID, GitRef: "dirty", JobType: storage.JobTypeDirty,
				DiffContent: diff, Agent: "test", PanelRunUUID: runUUID,
				PanelRole: storage.PanelRoleMember, PanelName: "p",
				PanelMemberName: name, PanelMemberIndex: idx,
			}
		}
		members := []storage.EnqueueOpts{mkMember("m0", 0), mkMember("m1", 1)}
		synth := storage.EnqueueOpts{
			RepoID: repo.ID, GitRef: "dirty", JobType: storage.JobTypeDirty,
			DiffContent: diff, Agent: "test", PanelRunUUID: runUUID,
			PanelRole: storage.PanelRoleSynthesis, PanelName: "p",
		}
		_, synthJob, err := db.EnqueuePanelRun(members, synth)
		require.NoError(t, err)

		newUUID, newMembers := rerunAndLoadNewRun(t, server, db, runUUID, synthJob.ID)
		require.Len(t, newMembers, 2)
		for _, m := range newMembers {
			gotDiff, err := db.GetJobDiffContent(m.ID)
			require.NoError(t, err)
			assert.Equal(diff, gotDiff, "dirty diff copied to rerun member")
			assert.Equal(storage.JobTypeDirty, m.JobType)
		}
		newSynth, err := db.GetSynthesisJob(newUUID)
		require.NoError(t, err)
		gotSynthDiff, err := db.GetJobDiffContent(newSynth.ID)
		require.NoError(t, err)
		assert.Equal(diff, gotSynthDiff, "dirty diff copied to rerun synthesis")
	})

	t.Run("single_commit", func(t *testing.T) {
		assert := assert.New(t)
		server, db, _ := newTestServer(t)
		repo, err := db.GetOrCreateRepo(t.TempDir())
		require.NoError(t, err)
		commit, err := db.GetOrCreateCommit(repo.ID, "abc123", "A", "S", time.Now())
		require.NoError(t, err)

		const patchID = "patch-abc"
		runUUID := uuid.NewString()
		mkMember := func(name string, idx int) storage.EnqueueOpts {
			return storage.EnqueueOpts{
				RepoID: repo.ID, CommitID: commit.ID, GitRef: "abc123",
				PatchID: patchID, JobType: storage.JobTypeReview, Agent: "test",
				PanelRunUUID: runUUID, PanelRole: storage.PanelRoleMember,
				PanelName: "p", PanelMemberName: name, PanelMemberIndex: idx,
			}
		}
		members := []storage.EnqueueOpts{mkMember("m0", 0), mkMember("m1", 1)}
		synth := storage.EnqueueOpts{
			RepoID: repo.ID, CommitID: commit.ID, GitRef: "abc123", PatchID: patchID,
			Agent: "test", PanelRunUUID: runUUID, PanelRole: storage.PanelRoleSynthesis,
			PanelName: "p",
		}
		_, synthJob, err := db.EnqueuePanelRun(members, synth)
		require.NoError(t, err)

		_, newMembers := rerunAndLoadNewRun(t, server, db, runUUID, synthJob.ID)
		require.Len(t, newMembers, 2)
		for _, m := range newMembers {
			assert.Equal(commit.ID, m.CommitIDValue(), "commit id copied to rerun member")
			assert.Equal(patchID, m.PatchID, "patch id copied to rerun member")
			assert.Equal("abc123", m.GitRef)
			gotDiff, err := db.GetJobDiffContent(m.ID)
			require.NoError(t, err)
			assert.Empty(gotDiff, "single-commit members carry no diff")
		}
	})
}
