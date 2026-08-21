package storage

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnqueueJobStoresExperimentAtomically(t *testing.T) {
	db, repo := setupDBAndRepo(t, "experiment-single")
	assignment := &ExperimentAssignmentInput{
		ExperimentID:        "session-v1",
		DefinitionHash:      "definition-one",
		DefinitionJSON:      `{"ratio":0.5}`,
		Arm:                 "experiment",
		SubjectHash:         "subject-one",
		EffectiveConfigHash: "config-one",
		EffectiveConfigJSON: `{"agent":"codex"}`,
	}
	job, err := db.EnqueueJob(EnqueueOpts{
		RepoID: repo.ID, GitRef: "first", Agent: "codex",
		Experiment: assignment,
	})
	require.NoError(t, err)
	require.Len(t, job.Experiments, 1)
	assert.Equal(t, assignment.ExperimentID, job.Experiments[0].ID)
	assert.Equal(t, assignment.Arm, job.Experiments[0].Arm)

	projected, err := db.GetJobsWithReviewsByIDs([]int64{job.ID})
	require.NoError(t, err)
	require.Len(t, projected[job.ID].Job.Experiments, 1)
	assert.Equal(t, job.Experiments, projected[job.ID].Job.Experiments)

	claimed, err := db.ClaimJob("experiment-worker")
	require.NoError(t, err)
	require.Equal(t, job.ID, claimed.ID)
	require.NoError(t, db.CompleteJob(job.ID, "codex", "prompt", "No issues found."))
	review, err := db.GetReviewByJobID(job.ID)
	require.NoError(t, err)
	assert.Equal(t, job.Experiments, review.Job.Experiments)

	conflict := *assignment
	conflict.DefinitionHash = "definition-two"
	conflict.DefinitionJSON = `{"ratio":1}`
	_, err = db.EnqueueJob(EnqueueOpts{
		RepoID: repo.ID, GitRef: "second", Agent: "codex",
		Experiment: &conflict,
	})
	require.ErrorContains(t, err, "experiment definition conflict")

	var jobs, definitions, assignments int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM review_jobs`).Scan(&jobs))
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM experiment_definitions`).Scan(&definitions))
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM experiment_assignments`).Scan(&assignments))
	assert.Equal(t, 1, jobs)
	assert.Equal(t, 1, definitions)
	assert.Equal(t, 1, assignments)
}

func TestPanelExperimentProjectsToEveryJob(t *testing.T) {
	db, repo := setupDBAndRepo(t, "experiment-panel")
	const runUUID = "panel-run-one"
	assignment := &ExperimentAssignmentInput{
		ExperimentID:        "panel-v1",
		DefinitionHash:      "panel-definition",
		DefinitionJSON:      `{"ratio":1}`,
		Arm:                 "experiment",
		SubjectHash:         "panel-subject",
		EffectiveConfigHash: "panel-config",
		EffectiveConfigJSON: `{"members":[]}`,
	}
	members, synthesis, err := db.EnqueuePanelRun(
		[]EnqueueOpts{{
			RepoID: repo.ID, GitRef: "base..head", Agent: "codex",
			PanelRunUUID: runUUID, PanelName: "quality", PanelMemberName: "bugs",
		}},
		EnqueueOpts{
			RepoID: repo.ID, GitRef: "base..head", Agent: "codex",
			PanelRunUUID: runUUID, PanelName: "quality",
			Experiment: assignment,
		},
	)
	require.NoError(t, err)
	require.Len(t, members, 1)
	require.Len(t, members[0].Experiments, 1)
	require.Len(t, synthesis.Experiments, 1)
	assert.Equal(t, assignment.ExperimentID, members[0].Experiments[0].ID)
	assert.Equal(t, members[0].Experiments, synthesis.Experiments)

	projected, err := db.GetJobsWithReviewsByIDs([]int64{members[0].ID, synthesis.ID})
	require.NoError(t, err)
	require.Len(t, projected[members[0].ID].Job.Experiments, 1)
	require.Len(t, projected[synthesis.ID].Job.Experiments, 1)
	assert.Equal(t,
		projected[members[0].ID].Job.Experiments,
		projected[synthesis.ID].Job.Experiments,
	)

	stored, err := db.GetExperimentAssignmentInputForJobUUID(members[0].UUID)
	require.NoError(t, err)
	assert.Equal(t, assignment, stored)

	var assignmentCount int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM experiment_assignments`).Scan(&assignmentCount))
	assert.Equal(t, 1, assignmentCount)
}

func TestExportReviewIncludesExperimentAndResumeLineage(t *testing.T) {
	db, repo := setupDBAndRepo(t, "experiment-export")
	assignment := &ExperimentAssignmentInput{
		ExperimentID:        "session-v1",
		DefinitionHash:      "definition-hash",
		DefinitionJSON:      `{"ratio":1}`,
		Arm:                 "experiment",
		SubjectHash:         "subject-hash",
		EffectiveConfigHash: "config-hash",
		EffectiveConfigJSON: `{"agent":"codex"}`,
	}
	job, err := db.EnqueueJob(EnqueueOpts{
		RepoID: repo.ID, GitRef: "review-sha", Agent: "codex",
		ResumeSourceJobUUID: "source-job-uuid",
		Experiment:          assignment,
	})
	require.NoError(t, err)
	claimed, err := db.ClaimJob("export-worker")
	require.NoError(t, err)
	require.Equal(t, job.ID, claimed.ID)
	require.NoError(t, db.CompleteJob(job.ID, "codex", "prompt", "No issues found."))

	page, err := db.ExportReviews(ExportReviewsOptions{
		Profile: ExportProfileMetadata, Limit: 10,
	})
	require.NoError(t, err)
	require.Len(t, page.Reviews, 1)
	require.Len(t, page.Reviews[0].Experiments, 1)
	assert.Equal(t, job.Experiments, page.Reviews[0].Experiments)
	require.NotNil(t, page.Reviews[0].ResumeSourceJobUUID)
	assert.Equal(t, "source-job-uuid", *page.Reviews[0].ResumeSourceJobUUID)
}

func TestUpsertPulledExperimentAssignmentConflictLeavesOriginalRow(t *testing.T) {
	db, _ := setupDBAndRepo(t, "experiment-pull-conflict")
	assignedAt := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	for _, definition := range []SyncableExperimentDefinition{
		{
			ExperimentID: "session-v1", DefinitionHash: "definition-a",
			DefinitionJSON: `{"ratio":0.5}`, FirstSeenAt: assignedAt,
			SourceMachineID: "machine-a",
		},
		{
			ExperimentID: "model-v1", DefinitionHash: "definition-b",
			DefinitionJSON: `{"ratio":0.5}`, FirstSeenAt: assignedAt,
			SourceMachineID: "machine-a",
		},
	} {
		require.NoError(t, db.UpsertPulledExperimentDefinition(definition))
	}

	original := SyncableExperimentAssignment{
		ReviewUnitKind: ReviewUnitJob, ReviewUnitUUID: "job-unit-1",
		ExperimentID: "session-v1", Arm: "experiment",
		SubjectHash: "subject-a", EffectiveConfigHash: "config-a",
		EffectiveConfigJSON: `{"agent":"codex"}`,
		AssignedAt:          assignedAt, SourceMachineID: "machine-a",
	}
	require.NoError(t, db.UpsertPulledExperimentAssignment(original))
	require.NoError(t, db.UpsertPulledExperimentAssignment(original))

	conflicting := original
	conflicting.ExperimentID = "model-v1"
	conflicting.SubjectHash = "subject-b"
	conflicting.EffectiveConfigHash = "config-b"
	require.Error(t, db.UpsertPulledExperimentAssignment(conflicting))

	assignments, err := db.GetExperimentAssignments(ReviewUnitJob, original.ReviewUnitUUID)
	require.NoError(t, err)
	require.Len(t, assignments, 1)
	assert.Equal(t, original.ExperimentID, assignments[0].ID)
	assert.Equal(t, original.Arm, assignments[0].Arm)
	assert.Equal(t, original.SubjectHash, assignments[0].SubjectHash)
	assert.Equal(t, original.EffectiveConfigHash, assignments[0].EffectiveConfigHash)
}

func TestGetExperimentDefinitionsToSyncIncludesForeignDefinitionForLocalAssignment(t *testing.T) {
	db, repo := setupDBAndRepo(t, "experiment-definition-dependency")
	machineID, err := db.GetMachineID()
	require.NoError(t, err)
	definition := SyncableExperimentDefinition{
		ExperimentID: "session-v1", DefinitionHash: "definition-a",
		DefinitionJSON:  `{"ratio":0.5}`,
		FirstSeenAt:     time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC),
		SourceMachineID: "machine-a",
	}
	require.NoError(t, db.UpsertPulledExperimentDefinition(definition))

	_, err = db.EnqueueJob(EnqueueOpts{
		RepoID: repo.ID, GitRef: "review-sha", Agent: "codex",
		Experiment: &ExperimentAssignmentInput{
			ExperimentID: definition.ExperimentID, DefinitionHash: definition.DefinitionHash,
			DefinitionJSON: definition.DefinitionJSON, Arm: "experiment",
			SubjectHash: "subject-a", EffectiveConfigHash: "config-a",
			EffectiveConfigJSON: `{"agent":"codex"}`,
		},
	})
	require.NoError(t, err)
	require.NoError(t, db.ClearAllSyncedAt())

	definitions, err := db.GetExperimentDefinitionsToSync(machineID)
	require.NoError(t, err)
	require.Len(t, definitions, 1)
	assert.Equal(t, definition, definitions[0])
}
