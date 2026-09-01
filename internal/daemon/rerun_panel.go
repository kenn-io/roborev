package daemon

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
)

type experimentPanelMemberKey struct {
	name  string
	index int
}

func restoreExperimentPanelPlan(
	members []storage.EnqueueOpts,
	synthesis *storage.EnqueueOpts,
	assignment *storage.ExperimentAssignmentInput,
) error {
	if assignment == nil {
		return nil
	}
	var plan experimentReviewPlan
	if err := json.Unmarshal([]byte(assignment.EffectiveConfigJSON), &plan); err != nil {
		return fmt.Errorf("decode frozen experiment panel plan: %w", err)
	}
	planHash, err := config.FingerprintExperimentConfig(plan)
	if err != nil {
		return fmt.Errorf("fingerprint frozen experiment panel plan: %w", err)
	}
	if planHash != assignment.EffectiveConfigHash {
		return errors.New("frozen experiment panel plan does not match its attribution")
	}
	if len(plan.Members) != len(members) {
		return fmt.Errorf("frozen experiment panel plan has %d members, rerun has %d",
			len(plan.Members), len(members))
	}

	plansByMember := make(map[experimentPanelMemberKey]experimentJobPlan, len(plan.Members))
	for _, memberPlan := range plan.Members {
		key := experimentPanelMemberKey{
			name: memberPlan.PanelMemberName, index: memberPlan.PanelMemberIndex,
		}
		if _, duplicate := plansByMember[key]; duplicate {
			return fmt.Errorf("frozen experiment panel plan repeats member %q at index %d",
				key.name, key.index)
		}
		plansByMember[key] = memberPlan
	}
	for i := range members {
		key := experimentPanelMemberKey{
			name: members[i].PanelMemberName, index: members[i].PanelMemberIndex,
		}
		memberPlan, ok := plansByMember[key]
		if !ok {
			return fmt.Errorf("frozen experiment panel plan is missing member %q at index %d",
				key.name, key.index)
		}
		if memberPlan.PanelName != members[i].PanelName ||
			memberPlan.JobType != members[i].JobType {
			return fmt.Errorf("frozen experiment panel plan identity does not match member %q at index %d",
				key.name, key.index)
		}
		applyExperimentJobPlan(&members[i], memberPlan)
		delete(plansByMember, key)
	}
	if len(plansByMember) != 0 {
		return errors.New("frozen experiment panel plan contains unmatched members")
	}
	if plan.Synthesis.PanelName != synthesis.PanelName ||
		plan.Synthesis.JobType != synthesis.JobType {
		return errors.New("frozen experiment panel synthesis identity does not match rerun")
	}
	applyExperimentJobPlan(synthesis, plan.Synthesis)
	return nil
}

func applyExperimentJobPlan(opts *storage.EnqueueOpts, plan experimentJobPlan) {
	opts.Agent = plan.Agent
	opts.Model = plan.Model
	opts.Provider = plan.Provider
	opts.Reasoning = plan.Reasoning
	opts.ReviewType = plan.ReviewType
	opts.MinSeverity = plan.MinSeverity
	opts.BackupAgent = plan.BackupAgent
	opts.BackupModel = plan.BackupModel
	opts.PanelMemberConfigJSON = plan.PanelMemberConfigJSON
}

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
func (s *Server) rerunPanelRun(job *storage.ReviewJob, requestID uuid.UUID) (*RerunJobOutput, error) {
	// Require the same terminal states as ReenqueueJob so a queued/running
	// synthesis cannot be rerun into a second active run alongside the original.
	if !isRerunnableStatus(job.Status) {
		return nil, huma.Error404NotFound("job not found or not rerunnable")
	}
	if panelRerunWorktreeIsInvalid(job) {
		return nil, huma.Error400BadRequest(
			"panel rerun worktree path is stale or invalid",
		)
	}
	if job.PanelRunUUID == nil {
		return nil, huma.Error400BadRequest("job is not part of a panel run")
	}
	members, err := s.db.GetPanelMembers(*job.PanelRunUUID)
	if err != nil {
		return nil, huma.Error500InternalServerError(
			fmt.Sprintf("load panel members: %v", err))
	}
	if len(members) == 0 {
		return nil, huma.Error400BadRequest("panel run has no members to rerun")
	}
	for i := range members {
		if !isRerunnableStatus(members[i].Status) {
			return nil, huma.Error409Conflict("panel member is not rerunnable")
		}
		// Successful and failed terminal jobs retain their historical worker ID.
		// A canceled job is different: its worker may still be unwinding and
		// using the shared worktree until ReleaseCanceledJobWorker clears the
		// ownership marker.
		if members[i].Status == storage.JobStatusCanceled && members[i].WorkerID != "" {
			return nil, huma.Error409Conflict("panel member is still stopping")
		}
		if panelRerunWorktreeIsInvalid(&members[i]) {
			return nil, huma.Error400BadRequest(
				"panel rerun worktree path is stale or invalid",
			)
		}
	}
	source, err := s.panelRerunSource(job)
	if err != nil {
		return nil, huma.Error500InternalServerError(
			fmt.Sprintf("resolve panel rerun source: %v", err))
	}

	runUUID := uuid.New()
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
	jobUUID := uuid.Nil()
	if job.UUID != nil {
		jobUUID = *job.UUID
	}
	synthOpts.Experiment, err = s.db.GetExperimentAssignmentInputForJobUUID(jobUUID)
	if err != nil {
		return nil, huma.Error500InternalServerError(
			fmt.Sprintf("load panel experiment assignment: %v", err))
	}
	if err := restoreExperimentPanelPlan(memberOpts, &synthOpts, synthOpts.Experiment); err != nil {
		return nil, huma.Error500InternalServerError(
			fmt.Sprintf("restore panel experiment plan: %v", err))
	}

	_, synthJob, replayed, err := s.db.EnqueuePanelRerun(memberOpts, synthOpts, requestID, job.ID)
	if err != nil {
		return nil, huma.Error500InternalServerError(
			fmt.Sprintf("enqueue rerun panel: %v", err))
	}
	if !replayed {
		s.broadcastRerunEnqueued(synthJob.ID, synthJob.UUID, job)
	}

	resp := &RerunJobOutput{}
	resp.Body.Success = true
	resp.Body.JobID = synthJob.ID
	resp.Body.RequestID = requestID
	resp.Body.RunUUID = synthJob.PanelRunUUID
	return resp, nil
}

func panelRerunWorktreeIsInvalid(job *storage.ReviewJob) bool {
	return job.WorktreePath != "" &&
		validatedWorktreePath(job.WorktreePath, job.RepoPath) == ""
}

func (s *Server) panelRerunSource(job *storage.ReviewJob) (string, error) {
	if job.Source != "" {
		return job.Source, nil
	}
	if job.PanelRunUUID == nil {
		return "", nil
	}
	if _, err := s.db.GetCIPanelByRunUUID(*job.PanelRunUUID); err != nil {
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
func panelRerunMemberOpts(m storage.ReviewJob, runUUID uuid.UUID, diff string, dirtyFiles []string, source string) storage.EnqueueOpts {
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
		PanelRunUUID:          &runUUID,
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
func panelRerunSynthesisOpts(job *storage.ReviewJob, runUUID uuid.UUID, diff string, dirtyFiles []string, source string) storage.EnqueueOpts {
	return storage.EnqueueOpts{
		RepoID:                job.RepoID,
		CommitID:              job.CommitIDValue(),
		GitRef:                job.GitRef,
		Branch:                job.Branch,
		CIBaseBranch:          job.CIBaseBranch,
		Agent:                 job.Agent,
		Model:                 job.Model,
		Provider:              job.Provider,
		RequestedModel:        job.RequestedModel,
		RequestedProvider:     job.RequestedProvider,
		Reasoning:             job.Reasoning,
		ReviewType:            job.ReviewType,
		PatchID:               job.PatchID,
		DiffContent:           diff,
		DirtyFiles:            dirtyFiles,
		OutputPrefix:          job.OutputPrefix,
		Source:                source,
		Agentic:               job.Agentic,
		JobType:               storage.JobTypeSynthesis,
		WorktreePath:          job.WorktreePath,
		MinSeverity:           job.MinSeverity,
		BackupAgent:           job.BackupAgent,
		BackupModel:           job.BackupModel,
		PanelRunUUID:          &runUUID,
		PanelRole:             storage.PanelRoleSynthesis,
		PanelName:             job.PanelName,
		PanelMemberConfigJSON: job.PanelMemberConfigJSON,
		ClaimBlocked:          true,
	}
}
