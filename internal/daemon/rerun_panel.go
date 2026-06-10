package daemon

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"go.kenn.io/roborev/internal/storage"
)

// isRerunnableStatus reports whether a job in this status may be rerun. It
// mirrors ReenqueueJob's terminal-state guard so panel synthesis reruns reject
// queued/running jobs the same way single-job reruns do.
func isRerunnableStatus(status storage.JobStatus) bool {
	switch status {
	case storage.JobStatusDone, storage.JobStatusFailed,
		storage.JobStatusCanceled, storage.JobStatusSkipped:
		return true
	default:
		return false
	}
}

// rerunPanelRun rebuilds a panel run as a brand-new run. Rerunning the synthesis
// parent clones every member's frozen target + resolved agent/panel config and
// the synthesis row into fresh queued jobs under a new panel_run_uuid, leaving
// the original run intact as history. EnqueuePanelRun re-blocks the new
// synthesis until the new members finish.
func (s *Server) rerunPanelRun(job *storage.ReviewJob) (*RerunJobOutput, error) {
	// Require the same terminal states as ReenqueueJob so a queued/running
	// synthesis cannot be rerun into a second active run alongside the original.
	if !isRerunnableStatus(job.Status) {
		return nil, huma.Error404NotFound("job not found or not rerunnable")
	}
	members, err := s.db.GetPanelMembers(job.PanelRunUUID)
	if err != nil {
		return nil, huma.Error500InternalServerError(
			fmt.Sprintf("load panel members: %v", err))
	}
	if len(members) == 0 {
		return nil, huma.Error400BadRequest("panel run has no members to rerun")
	}
	source, err := s.panelRerunSource(job)
	if err != nil {
		return nil, huma.Error500InternalServerError(
			fmt.Sprintf("resolve panel rerun source: %v", err))
	}

	runUUID := uuid.NewString()
	memberOpts := make([]storage.EnqueueOpts, len(members))
	for i := range members {
		diff, diffErr := s.db.GetJobDiffContent(members[i].ID)
		if diffErr != nil {
			return nil, huma.Error500InternalServerError(
				fmt.Sprintf("load member diff: %v", diffErr))
		}
		dirtyFiles, filesErr := s.db.GetJobDirtyFiles(members[i].ID)
		if filesErr != nil {
			return nil, huma.Error500InternalServerError(
				fmt.Sprintf("load member dirty files: %v", filesErr))
		}
		memberOpts[i] = panelRerunMemberOpts(members[i], runUUID, diff, dirtyFiles, source)
	}

	synthDiff, err := s.db.GetJobDiffContent(job.ID)
	if err != nil {
		return nil, huma.Error500InternalServerError(
			fmt.Sprintf("load synthesis diff: %v", err))
	}
	synthDirtyFiles, err := s.db.GetJobDirtyFiles(job.ID)
	if err != nil {
		return nil, huma.Error500InternalServerError(
			fmt.Sprintf("load synthesis dirty files: %v", err))
	}
	synthOpts := panelRerunSynthesisOpts(job, runUUID, synthDiff, synthDirtyFiles, source)

	if _, _, err := s.db.EnqueuePanelRun(memberOpts, synthOpts); err != nil {
		return nil, huma.Error500InternalServerError(
			fmt.Sprintf("enqueue rerun panel: %v", err))
	}

	resp := &RerunJobOutput{}
	resp.Body.Success = true
	return resp, nil
}

func (s *Server) panelRerunSource(job *storage.ReviewJob) (string, error) {
	if job.Source != "" {
		return job.Source, nil
	}
	if _, err := s.db.GetCIPanelByRunUUID(job.PanelRunUUID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	return storage.JobSourceCI, nil
}

// panelRerunMemberOpts clones one member job into fresh EnqueueOpts for a new
// run. It copies the full frozen target (commit/diff/patch/severity/worktree),
// the resolved agent/model/provider/reasoning/review_type, the stored Prompt
// only for prompt-native job types, and the member's panel identity
// (name/index/config), reassigning only the run UUID. Review/range/dirty prompts
// are rebuilt by the worker so reruns do not reuse stale prebuilt prompts. diff
// is the member's stored dirty diff (empty for commit/range targets).
func panelRerunMemberOpts(m storage.ReviewJob, runUUID, diff string, dirtyFiles []string, source string) storage.EnqueueOpts {
	prompt := ""
	if m.UsesStoredPrompt() {
		prompt = m.Prompt
	}
	return storage.EnqueueOpts{
		RepoID:                m.RepoID,
		CommitID:              m.CommitIDValue(),
		GitRef:                m.GitRef,
		Branch:                m.Branch,
		CIBaseBranch:          m.CIBaseBranch,
		Agent:                 m.Agent,
		Model:                 m.Model,
		Provider:              m.Provider,
		RequestedModel:        m.RequestedModel,
		RequestedProvider:     m.RequestedProvider,
		Reasoning:             m.Reasoning,
		ReviewType:            m.ReviewType,
		PatchID:               m.PatchID,
		DiffContent:           diff,
		DirtyFiles:            dirtyFiles,
		Prompt:                prompt,
		PromptPrebuilt:        false,
		Source:                source,
		OutputPrefix:          m.OutputPrefix,
		Agentic:               m.Agentic,
		JobType:               m.JobType,
		WorktreePath:          m.WorktreePath,
		MinSeverity:           m.MinSeverity,
		BackupAgent:           m.BackupAgent,
		BackupModel:           m.BackupModel,
		PanelRunUUID:          runUUID,
		PanelRole:             storage.PanelRoleMember,
		PanelName:             m.PanelName,
		PanelMemberName:       m.PanelMemberName,
		PanelMemberIndex:      m.PanelMemberIndex,
		PanelMemberConfigJSON: m.PanelMemberConfigJSON,
	}
}

// panelRerunSynthesisOpts clones the synthesis parent into fresh EnqueueOpts for
// a new run. EnqueuePanelRun re-enforces JobType=synthesis, role=synthesis, and
// ClaimBlocked, but they are set here too so the opts are self-describing.
func panelRerunSynthesisOpts(job *storage.ReviewJob, runUUID, diff string, dirtyFiles []string, source string) storage.EnqueueOpts {
	return storage.EnqueueOpts{
		RepoID:            job.RepoID,
		CommitID:          job.CommitIDValue(),
		GitRef:            job.GitRef,
		Branch:            job.Branch,
		CIBaseBranch:      job.CIBaseBranch,
		Agent:             job.Agent,
		Model:             job.Model,
		Provider:          job.Provider,
		RequestedModel:    job.RequestedModel,
		RequestedProvider: job.RequestedProvider,
		Reasoning:         job.Reasoning,
		ReviewType:        job.ReviewType,
		PatchID:           job.PatchID,
		DiffContent:       diff,
		DirtyFiles:        dirtyFiles,
		OutputPrefix:      job.OutputPrefix,
		Source:            source,
		Agentic:           job.Agentic,
		JobType:           storage.JobTypeSynthesis,
		WorktreePath:      job.WorktreePath,
		MinSeverity:       job.MinSeverity,
		BackupAgent:       job.BackupAgent,
		BackupModel:       job.BackupModel,
		PanelRunUUID:      runUUID,
		PanelRole:         storage.PanelRoleSynthesis,
		PanelName:         job.PanelName,
		ClaimBlocked:      true,
	}
}
