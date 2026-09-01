package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strings"
	"time"
	"uuid"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/git"
	"go.kenn.io/roborev/internal/prompt"
	"go.kenn.io/roborev/internal/storage"
)

// targetDescriptor is the frozen, agent-independent part of an enqueue: every
// resolved EnqueueOpts field EXCEPT the per-agent ones (Agent/Model/Provider/
// Reasoning/ReviewType) and SessionID. Captured once so the single-agent job and
// (Task 4) every panel member review identical input. Keep this exhaustive — a
// missing field silently regresses one of the four enqueue paths.
type targetDescriptor struct {
	repoID            int64
	commitID          int64
	gitRef            string // frozen SHA / <sha>..<sha> / "dirty"; "" for prompt
	branch            string
	sessionSHA        string // SHA to key session reuse on ("" for prompt jobs)
	patchID           string
	diffContent       string
	dirtyFiles        []string
	minSeverity       string
	worktreePath      string
	jobType           string // req.JobType for prompt; "" lets EnqueueJob infer dirty/range/review
	source            string
	prompt            string
	promptPrebuilt    bool
	outputPrefix      string
	label             string // prompt jobs only (= gitRef display)
	agentic           bool
	requestedModel    string
	requestedProvider string
	commitSubject     string // single-commit only; set on the returned job
}

// baseOpts returns an EnqueueOpts with every non-agent, non-panel field set.
// SessionID stays empty (the caller resolves it). The caller overlays
// Agent/Model/Provider/Reasoning/ReviewType (+ panel fields in Task 4).
func (d targetDescriptor) baseOpts() storage.EnqueueOpts {
	return storage.EnqueueOpts{
		RepoID: d.repoID, CommitID: d.commitID, GitRef: d.gitRef, Branch: d.branch,
		PatchID: d.patchID, DiffContent: d.diffContent, DirtyFiles: d.dirtyFiles, MinSeverity: d.minSeverity,
		WorktreePath: d.worktreePath, JobType: d.jobType, Prompt: d.prompt,
		Source: d.source, PromptPrebuilt: d.promptPrebuilt, OutputPrefix: d.outputPrefix, Label: d.label,
		Agentic: d.agentic, RequestedModel: d.requestedModel, RequestedProvider: d.requestedProvider,
	}
}

// freezeInputs groups the agent-independent inputs needed to freeze an enqueue
// target. It keeps buildTargetDescriptor within the project's positional-param
// limit while threading the request, repo, and resolved paths through the
// per-branch descriptor builders.
type freezeInputs struct {
	repo              *storage.Repo
	req               EnqueueRequest
	gitRef            string
	checkoutRoot      string
	repoRoot          string
	metadata          git.EnqueueMetadataReader
	worktreePath      string
	normalizedMinSev  string
	requestedModel    string
	requestedProvider string
}

// targetKind classifies an enqueue target by how its diff is sourced.
type targetKind int

const (
	kindPrompt       targetKind = iota // stored custom prompt (task/insights/compact)
	kindDirty                          // uncommitted working-tree changes
	kindRange                          // commit range "<a>..<b>"
	kindSingleCommit                   // a single commit
)

// classifyTarget decides which freeze path an enqueue takes. A stored custom
// prompt wins over the git_ref; "dirty" and "<a>..<b>" are recognized by the ref.
func classifyTarget(customPrompt, gitRef string) targetKind {
	switch {
	case customPrompt != "":
		return kindPrompt
	case gitRef == "dirty":
		return kindDirty
	case strings.Contains(gitRef, ".."):
		return kindRange
	default:
		return kindSingleCommit
	}
}

// buildTargetDescriptor performs the agent-independent freeze for one enqueue:
// insights-prompt building, classification, SHA resolution, diff-size and
// commit-message exclusion checks, and commit creation. It returns either a
// fully frozen targetDescriptor or a non-nil *RawJSONOutput that the caller must
// return verbatim (a skip 200 / 400 / 500 early return). It must NOT depend on
// the resolved agent or model — sessionSHA records the SHA so the caller can
// resolve SessionID after picking the agent.
func (s *Server) buildTargetDescriptor(
	ctx context.Context, in freezeInputs,
) (targetDescriptor, *RawJSONOutput) {
	if in.metadata == nil {
		in.metadata = git.OpenEnqueueMetadataReader(ctx, in.checkoutRoot)
	}
	insightsPrompt, early := s.resolveInsightsPrompt(ctx, in)
	if early != nil {
		return targetDescriptor{}, early
	}
	if insightsPrompt != "" {
		in.req.CustomPrompt = insightsPrompt
	}

	var desc targetDescriptor
	switch classifyTarget(in.req.CustomPrompt, in.gitRef) {
	case kindPrompt:
		desc = s.descriptorForPrompt(in)
	case kindDirty:
		desc, early = s.descriptorForDirty(ctx, in)
	case kindRange:
		desc, early = s.descriptorForRange(ctx, in)
	default:
		desc, early = s.descriptorForSingleCommit(ctx, in)
	}
	if early != nil {
		return targetDescriptor{}, early
	}
	desc.source = in.req.Source
	return desc, nil
}

// resolveInsightsPrompt builds the insights prompt for insights jobs and returns
// it so the caller can fold it into req.CustomPrompt. For every other job type it
// returns an empty prompt and no early return. A non-nil *RawJSONOutput is an
// early return (400 validation / 200 skip / 500 build error).
func (s *Server) resolveInsightsPrompt(
	ctx context.Context, in freezeInputs,
) (string, *RawJSONOutput) {
	if in.req.JobType != storage.JobTypeInsights {
		return "", nil
	}
	if in.req.Since == "" {
		out, _ := rawJSONOutput(http.StatusBadRequest,
			ErrorResponse{Error: "since is required for insights jobs"})
		return "", out
	}
	since, err := time.Parse(time.RFC3339, in.req.Since)
	if err != nil {
		out, _ := rawJSONOutput(http.StatusBadRequest,
			ErrorResponse{Error: "since must be RFC3339"})
		return "", out
	}

	insightsPrompt, reviewCount, err := s.buildInsightsPrompt(
		ctx, in.repoRoot, in.req.Branch, since,
	)
	if err != nil {
		if s.errorLog != nil {
			s.errorLog.LogError(
				"server", fmt.Sprintf("build insights prompt: %v", err), 0,
			)
		}
		out, _ := rawJSONOutput(http.StatusInternalServerError,
			ErrorResponse{Error: fmt.Sprintf("build insights prompt: %v", err)})
		return "", out
	}
	if reviewCount == 0 {
		out, _ := rawJSONOutput(http.StatusOK, EnqueueSkippedResponse{
			Skipped: true,
			Reason:  "No failing reviews found in the specified time window.",
		})
		return "", out
	}
	return insightsPrompt, nil
}

// descriptorForPrompt freezes a stored-prompt (task/insights/compact) target.
// gitRef/commitID/diffContent/patchID/sessionSHA stay empty; Label carries the
// git_ref display value.
func (s *Server) descriptorForPrompt(in freezeInputs) targetDescriptor {
	return targetDescriptor{
		repoID:            in.repo.ID,
		branch:            in.req.Branch,
		minSeverity:       in.normalizedMinSev,
		worktreePath:      in.worktreePath,
		jobType:           in.req.JobType,
		prompt:            in.req.CustomPrompt,
		outputPrefix:      in.req.OutputPrefix,
		label:             in.gitRef,
		agentic:           in.req.Agentic,
		requestedModel:    in.requestedModel,
		requestedProvider: in.requestedProvider,
	}
}

// descriptorForDirty freezes an uncommitted-changes target. The diff-size and
// required-diff checks stay here as early returns. sessionSHA is HEAD so session
// reuse keys on the working-tree base commit.
func (s *Server) descriptorForDirty(
	ctx context.Context, in freezeInputs,
) (targetDescriptor, *RawJSONOutput) {
	if in.req.DiffContent == "" && !prompt.HasDependencyMetadataFiles(in.req.DirtyFiles) {
		out, _ := rawJSONOutput(http.StatusBadRequest,
			ErrorResponse{Error: "diff_content required for dirty review"})
		return targetDescriptor{}, out
	}
	const maxDiffSize = 200 * 1024
	if len(in.req.DiffContent) > maxDiffSize {
		out, _ := rawJSONOutput(http.StatusBadRequest,
			ErrorResponse{Error: fmt.Sprintf(
				"diff_content too large (%d bytes, max %d)",
				len(in.req.DiffContent), maxDiffSize,
			)})
		return targetDescriptor{}, out
	}

	targetSHA, _ := in.metadata.Resolve("HEAD")
	var commitID int64
	if targetSHA != "" {
		if info, err := in.metadata.CommitInfo(targetSHA); err == nil {
			if commit, err := s.db.GetOrCreateCommit(
				in.repo.ID, targetSHA, info.Author, info.Subject, info.Timestamp,
			); err == nil {
				commitID = commit.ID
			}
		}
	}
	return targetDescriptor{
		repoID:            in.repo.ID,
		commitID:          commitID,
		gitRef:            in.gitRef,
		branch:            in.req.Branch,
		sessionSHA:        targetSHA,
		diffContent:       in.req.DiffContent,
		dirtyFiles:        slices.Clone(in.req.DirtyFiles),
		jobType:           storage.JobTypeDirty,
		minSeverity:       in.normalizedMinSev,
		worktreePath:      in.worktreePath,
		requestedModel:    in.requestedModel,
		requestedProvider: in.requestedProvider,
	}, nil
}

// descriptorForRange freezes a commit-range target. It resolves the endpoints to
// SHAs (supporting the "<first>^.." empty-tree fallback), applies the
// all-commits-excluded skip, and freezes git_ref to "<startSHA>..<endSHA>".
func (s *Server) descriptorForRange(
	ctx context.Context, in freezeInputs,
) (targetDescriptor, *RawJSONOutput) {
	parts := strings.SplitN(in.gitRef, "..", 2)
	startSHA, err := in.metadata.Resolve(parts[0])
	if err != nil {
		if before, ok := strings.CutSuffix(parts[0], "^"); ok {
			if _, resolveErr := in.metadata.Resolve(before + "^{commit}"); resolveErr == nil {
				startSHA = git.EmptyTreeSHA
				err = nil
			}
		}
		if err != nil {
			out, _ := rawJSONOutput(http.StatusBadRequest,
				ErrorResponse{Error: fmt.Sprintf("invalid start commit: %v", err)})
			return targetDescriptor{}, out
		}
	}
	endSHA, err := in.metadata.Resolve(parts[1])
	if err != nil {
		out, _ := rawJSONOutput(http.StatusBadRequest,
			ErrorResponse{Error: fmt.Sprintf("invalid end commit: %v", err)})
		return targetDescriptor{}, out
	}

	fullRef := startSHA + ".." + endSHA
	if skip := s.rangeExclusionSkip(in.repoRoot, in.metadata, fullRef); skip != nil {
		return targetDescriptor{}, skip
	}

	return targetDescriptor{
		repoID:            in.repo.ID,
		gitRef:            fullRef,
		branch:            in.req.Branch,
		sessionSHA:        endSHA,
		minSeverity:       in.normalizedMinSev,
		worktreePath:      in.worktreePath,
		requestedModel:    in.requestedModel,
		requestedProvider: in.requestedProvider,
	}, nil
}

// rangeExclusionSkip returns a skip 200 when every commit in the range matches an
// excluded message pattern, mirroring the single-commit exclusion. It returns nil
// when the range cannot be fully read or no commits match, so the review proceeds.
func (s *Server) rangeExclusionSkip(
	repoRoot string, metadata git.EnqueueMetadataReader, fullRef string,
) *RawJSONOutput {
	rangeCommits, rcErr := metadata.RangeCommits(fullRef)
	if rcErr != nil || len(rangeCommits) == 0 {
		return nil
	}
	messages := make([]string, 0, len(rangeCommits))
	for _, rc := range rangeCommits {
		ci, ciErr := metadata.CommitInfo(rc)
		if ciErr != nil {
			return nil
		}
		messages = append(messages, ci.Subject+"\n"+ci.Body)
	}
	if !config.AllCommitMessagesExcluded(repoRoot, messages) {
		return nil
	}
	out, _ := rawJSONOutput(http.StatusOK, EnqueueSkippedResponse{
		Skipped: true,
		Reason:  "all commits in range match excluded patterns",
	})
	return out
}

// descriptorForSingleCommit freezes a single-commit target: it resolves the SHA,
// applies the commit-message exclusion skip, creates/loads the commit row, and
// records the patch id and commit subject.
func (s *Server) descriptorForSingleCommit(
	ctx context.Context, in freezeInputs,
) (targetDescriptor, *RawJSONOutput) {
	sha, err := in.metadata.Resolve(in.gitRef)
	if err != nil {
		out, _ := rawJSONOutput(http.StatusBadRequest,
			ErrorResponse{Error: fmt.Sprintf("invalid commit: %v", err)})
		return targetDescriptor{}, out
	}

	info, err := in.metadata.CommitInfo(sha)
	if err != nil {
		out, _ := rawJSONOutput(http.StatusBadRequest,
			ErrorResponse{Error: fmt.Sprintf("get commit info: %v", err)})
		return targetDescriptor{}, out
	}

	fullMessage := info.Subject + "\n" + info.Body
	if config.IsCommitMessageExcluded(in.repoRoot, fullMessage) {
		out, _ := rawJSONOutput(http.StatusOK, EnqueueSkippedResponse{
			Skipped: true,
			Reason:  "commit message matches an excluded pattern",
		})
		return targetDescriptor{}, out
	}

	commit, err := s.db.GetOrCreateCommit(
		in.repo.ID, sha, info.Author, info.Subject, info.Timestamp,
	)
	if err != nil {
		out, _ := rawJSONOutput(http.StatusInternalServerError,
			ErrorResponse{Error: fmt.Sprintf("get commit: %v", err)})
		return targetDescriptor{}, out
	}

	return targetDescriptor{
		repoID:            in.repo.ID,
		commitID:          commit.ID,
		gitRef:            sha,
		branch:            in.req.Branch,
		sessionSHA:        sha,
		patchID:           git.GetPatchID(in.checkoutRoot, sha),
		minSeverity:       in.normalizedMinSev,
		worktreePath:      in.worktreePath,
		requestedModel:    in.requestedModel,
		requestedProvider: in.requestedProvider,
		commitSubject:     commit.Subject,
	}, nil
}

// selectPanelForTarget resolves the panel a frozen target should fan out into,
// or "" for a single-agent enqueue. Panels review code targets (single commit,
// range, dirty); a stored-prompt job (run/analyze/compact) must never fan into a
// review panel, whose synthesis worker assumes member code reviews. Gating on
// the frozen target keeps a configured default_panel/hook_panel from capturing
// prompt-native jobs.
func selectPanelForTarget(
	descriptor targetDescriptor, req EnqueueRequest, merged config.ReviewConfig,
) string {
	if descriptor.prompt != "" {
		return ""
	}
	return config.SelectPanelName(req.Panel, req.Source, merged)
}

// panelRunInputs groups the agent-independent inputs needed to fan a frozen
// enqueue target out into a panel run. It keeps enqueuePanelRun within the
// positional-param limit while threading the descriptor, selected panel name,
// resolution path, global config, and repo through to the storage layer.
type panelRunInputs struct {
	descriptor     targetDescriptor
	req            EnqueueRequest
	panelName      string
	gitRef         string
	resolutionPath string
	cfg            *config.Config
	repoCfg        *config.RepoConfig
	rawRepoCfg     map[string]any
	experiment     *config.ExperimentAssignment
	repo           *storage.Repo
}

type experimentJobPlan struct {
	Agent                 string
	Model                 string
	Provider              string
	Reasoning             string
	ReviewType            string
	MinSeverity           string
	BackupAgent           string
	BackupModel           string
	PanelName             string
	PanelMemberName       string
	PanelMemberIndex      int
	PanelMemberConfigJSON string
	JobType               string
}

type experimentReviewPlan struct {
	Members   []experimentJobPlan
	Synthesis experimentJobPlan
}

func experimentPlanForJob(opts storage.EnqueueOpts) experimentJobPlan {
	return experimentJobPlan{
		Agent:                 opts.Agent,
		Model:                 opts.Model,
		Provider:              opts.Provider,
		Reasoning:             opts.Reasoning,
		ReviewType:            opts.ReviewType,
		MinSeverity:           opts.MinSeverity,
		BackupAgent:           opts.BackupAgent,
		BackupModel:           opts.BackupModel,
		PanelName:             opts.PanelName,
		PanelMemberName:       opts.PanelMemberName,
		PanelMemberIndex:      opts.PanelMemberIndex,
		PanelMemberConfigJSON: opts.PanelMemberConfigJSON,
		JobType:               storage.JobTypeForEnqueue(opts),
	}
}

func experimentPlanForPanel(
	members []storage.EnqueueOpts,
	synthesis storage.EnqueueOpts,
) experimentReviewPlan {
	plan := experimentReviewPlan{
		Members:   make([]experimentJobPlan, len(members)),
		Synthesis: experimentPlanForJob(synthesis),
	}
	for i := range members {
		plan.Members[i] = experimentPlanForJob(members[i])
	}
	return plan
}

func applyFrozenExperimentSettings(
	job *storage.ReviewJob,
	assignment *storage.ExperimentAssignmentInput,
) error {
	if assignment == nil {
		return nil
	}
	var plan experimentJobPlan
	if job.PanelRunUUID == nil {
		if err := json.Unmarshal([]byte(assignment.EffectiveConfigJSON), &plan); err != nil {
			return fmt.Errorf("decode frozen experiment plan: %w", err)
		}
		planHash, err := config.FingerprintExperimentConfig(plan)
		if err != nil {
			return fmt.Errorf("fingerprint frozen experiment plan: %w", err)
		}
		if planHash != assignment.EffectiveConfigHash {
			return errors.New("frozen experiment plan does not match its attribution")
		}
	} else {
		var panel experimentReviewPlan
		if err := json.Unmarshal([]byte(assignment.EffectiveConfigJSON), &panel); err != nil {
			return fmt.Errorf("decode frozen experiment panel plan: %w", err)
		}
		planHash, err := config.FingerprintExperimentConfig(panel)
		if err != nil {
			return fmt.Errorf("fingerprint frozen experiment panel plan: %w", err)
		}
		if planHash != assignment.EffectiveConfigHash {
			return errors.New("frozen experiment panel plan does not match its attribution")
		}
		if job.PanelRole == storage.PanelRoleSynthesis {
			plan = panel.Synthesis
		} else {
			found := false
			for _, member := range panel.Members {
				if member.PanelMemberName == job.PanelMemberName &&
					member.PanelMemberIndex == job.PanelMemberIndex {
					plan = member
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("frozen experiment panel plan is missing member %q at index %d",
					job.PanelMemberName, job.PanelMemberIndex)
			}
		}
	}

	job.MinSeverity = plan.MinSeverity
	job.BackupAgent = plan.BackupAgent
	job.BackupModel = plan.BackupModel
	return nil
}

func storageAssignmentForExperiment(
	assignment *config.ExperimentAssignment,
	plan any,
) (*storage.ExperimentAssignmentInput, error) {
	if assignment == nil {
		return nil, nil
	}
	effectiveJSON, effectiveHash, err := config.EncodeExperimentConfig(plan)
	if err != nil {
		return nil, err
	}
	return &storage.ExperimentAssignmentInput{
		ExperimentID:        assignment.ID,
		DefinitionHash:      assignment.DefinitionHash,
		DefinitionJSON:      assignment.DefinitionJSON,
		Arm:                 string(assignment.Arm),
		SubjectHash:         assignment.SubjectHash,
		EffectiveConfigHash: effectiveHash,
		EffectiveConfigJSON: effectiveJSON,
	}, nil
}

// enqueuePanelRun resolves the selected panel and fans the frozen target out
// into N member jobs plus one claim-blocked synthesis job in a single
// transaction. It returns the PanelEnqueueResponse (201) or an early *RawJSONOutput
// (400 for an undefined panel, 500 for an insert failure). Synthesis agent
// availability is deferred to worker time (failover). Member execution fields
// are resolved up front so a selected backup agent receives its own model
// instead of the preferred agent's model.
func (s *Server) enqueuePanelRun(ctx context.Context, in panelRunInputs) (*RawJSONOutput, error) {
	members, synth, err := config.ResolveCIPanel(in.panelName, in.repoCfg, in.cfg)
	if err != nil {
		return rawJSONOutput(http.StatusBadRequest, ErrorResponse{Error: err.Error()})
	}

	runUUID := uuid.New()
	memberOpts, err := panelMemberOpts(
		in.descriptor, in.panelName, runUUID, members,
		in.repoCfg, in.cfg,
	)
	if err != nil {
		return rawJSONOutput(http.StatusBadRequest, ErrorResponse{Error: err.Error()})
	}
	synthOpts := panelSynthesisOpts(
		in.descriptor, in.panelName, runUUID, synth, in.repoCfg, in.cfg,
	)
	if in.experiment != nil {
		assignment, assignErr := storageAssignmentForExperiment(
			in.experiment, experimentPlanForPanel(memberOpts, synthOpts),
		)
		if assignErr != nil {
			return rawJSONOutput(http.StatusInternalServerError,
				ErrorResponse{Error: fmt.Sprintf("fingerprint experiment plan: %v", assignErr)})
		}
		synthOpts.Experiment = assignment
	} else {
		// Local panel members historically resolve failover at execution time.
		// Freeze backup choices only when they are part of experiment attribution.
		for i := range memberOpts {
			memberOpts[i].BackupAgent = ""
			memberOpts[i].BackupModel = ""
		}
	}
	for i := range memberOpts {
		memberOpts[i].SessionID, memberOpts[i].ResumeSourceJobUUID = findCompatibleReusableSession(
			ctx, s.db, in.resolutionPath, in.descriptor.sessionSHA,
			memberOpts[i], in.repoCfg, in.rawRepoCfg, in.cfg, synthOpts.Experiment, 0,
		)
	}

	var memberJobs []*storage.ReviewJob
	var synthJob *storage.ReviewJob
	var duplicate bool
	if in.req.Source == storage.JobSourcePostCommit {
		memberJobs, synthJob, duplicate, err = s.db.EnqueuePostCommitPanelRun(
			memberOpts, synthOpts,
		)
	} else {
		memberJobs, synthJob, err = s.db.EnqueuePanelRun(memberOpts, synthOpts)
	}
	if err != nil {
		return rawJSONOutput(http.StatusInternalServerError,
			ErrorResponse{Error: fmt.Sprintf("enqueue panel run: %v", err)})
	}
	if duplicate {
		return postCommitDuplicateResponse()
	}

	synthJob.RepoPath = in.repo.RootPath
	synthJob.RepoName = in.repo.Name
	memberIDs := make([]int64, len(memberJobs))
	for i, mj := range memberJobs {
		memberIDs[i] = mj.ID
	}
	s.maybeDispatchPanelAutoDesign(ctx, in, members, memberJobs)
	s.logEnqueueSideEffects(synthJob, enqueueSideEffectInputs{
		repo:       in.repo,
		gitRef:     in.gitRef,
		agentName:  synthJob.Agent,
		reviewType: in.req.ReviewType,
	})
	return rawJSONOutput(http.StatusCreated, PanelEnqueueResponse{
		ReviewJob:    synthJob,
		PanelRunUUID: runUUID,
		MemberJobIDs: memberIDs,
	})
}

func (s *Server) maybeDispatchPanelAutoDesign(
	ctx context.Context,
	in panelRunInputs,
	members []config.ResolvedMember,
	memberJobs []*storage.ReviewJob,
) {
	if !config.IsDefaultReviewType(in.req.ReviewType) ||
		panelHasDesignMember(members) ||
		len(memberJobs) == 0 ||
		memberJobs[0].JobType != storage.JobTypeReview {
		return
	}
	parent := *memberJobs[0]
	parent.RepoPath = in.repo.RootPath
	parent.RepoName = in.repo.Name
	if err := s.maybeDispatchAutoDesign(ctx, &parent); err != nil {
		log.Printf("auto-design dispatch failed: %v", err)
	}
}

func panelHasDesignMember(members []config.ResolvedMember) bool {
	for _, m := range members {
		if strings.EqualFold(strings.TrimSpace(m.ReviewType), "design") {
			return true
		}
	}
	return false
}

// panelMemberOpts overlays each resolved member's agent/model/provider/reasoning
// /review_type and panel fields onto the frozen base opts.
func panelMemberOpts(
	descriptor targetDescriptor, panelName string, runUUID uuid.UUID, members []config.ResolvedMember,
	repoCfg *config.RepoConfig, cfg *config.Config,
) ([]storage.EnqueueOpts, error) {
	out := make([]storage.EnqueueOpts, len(members))
	for i, m := range members {
		o := descriptor.baseOpts()
		cfgJSON, _ := json.Marshal(m)
		var err error
		o.Agent, o.Model, o.BackupAgent, o.BackupModel, err = resolvePanelMemberExecution(
			m, descriptor, repoCfg, cfg,
		)
		if err != nil {
			return nil, fmt.Errorf("panel member %q: %w", m.Name, err)
		}
		o.Provider = m.Provider
		o.Reasoning, o.ReviewType = m.Reasoning, m.ReviewType
		o.PanelRunUUID, o.PanelRole = &runUUID, storage.PanelRoleMember
		o.PanelName, o.PanelMemberName, o.PanelMemberIndex = panelName, m.Name, m.Index
		o.PanelMemberConfigJSON = string(cfgJSON)
		out[i] = o
	}
	return out, nil
}

func resolvePanelMemberExecution(
	m config.ResolvedMember, descriptor targetDescriptor, repoCfg *config.RepoConfig, cfg *config.Config,
) (string, string, string, string, error) {
	model := m.Model
	resolution, err := agent.ResolveWorkflowConfigFromConfig(
		m.Agent, repoCfg, cfg, workflowForPanelReviewType(m.ReviewType), m.Reasoning,
	)
	if err != nil {
		return "", "", "", "", err
	}
	strictWorkflowAgent := m.AgentExplicit ||
		config.HasWorkflowAgentOverrideFromConfig(
			resolution.RepoConfig, cfg, resolution.Workflow, resolution.Reasoning,
		) ||
		strings.TrimSpace(resolution.BackupAgent) != ""
	var selected agent.Agent
	if strictWorkflowAgent {
		selected, err = agent.GetPreferredOrBackupWithConfigFromConfig(
			repoCfg, resolution.PreferredAgent, cfg, resolution.BackupAgent,
		)
	} else {
		selected, err = agent.GetAvailableWithConfigFromConfig(
			repoCfg, resolution.PreferredAgent, cfg, resolution.BackupAgent,
		)
	}
	if err != nil {
		return "", "", "", "", err
	}
	if err := agent.ValidateStructuredReviewSelection(m.ReviewType, selected); err != nil {
		return "", "", "", "", err
	}
	selectedName := agent.StorageNameFromConfig(selected.Name(), repoCfg, cfg)
	if err := agent.ValidateStructuredReviewBackup(
		m.ReviewType, resolution, selectedName,
	); err != nil {
		return "", "", "", "", err
	}
	if !m.ModelExplicit || !resolution.AgentMatches(selectedName, m.Agent) {
		model = resolution.ModelForSelectedAgent(selectedName, "")
	}
	backupAgent, backupModel := backupExecutionForSelectedAgent(
		resolution, selectedName, repoCfg, cfg,
	)
	return selectedName, model, backupAgent, backupModel, nil
}

func workflowForPanelReviewType(reviewType string) string {
	return config.WorkflowForReviewType(reviewType)
}

// panelSynthesisOpts overlays the synthesis spec and panel fields onto the
// frozen base opts. The synthesis BackupAgent/BackupModel are persisted so the
// worker can prefer them on synthesis failover.
// EnqueuePanelRun enforces JobTypeSynthesis/PanelRoleSynthesis/ClaimBlocked, but
// they are set here too so the opts are self-describing.
func panelSynthesisOpts(
	descriptor targetDescriptor, panelName string, runUUID uuid.UUID,
	synth config.SynthesisSpec,
	repoCfg *config.RepoConfig, cfg *config.Config,
) storage.EnqueueOpts {
	o := descriptor.baseOpts()
	o.JobType = storage.JobTypeSynthesis
	o.Agent = agent.StorageNameFromConfig(synth.Agent, repoCfg, cfg)
	o.Model, o.Reasoning = synth.Model, synth.Reasoning
	o.BackupAgent = agent.StorageNameFromConfig(synth.BackupAgent, repoCfg, cfg)
	o.BackupModel = synth.BackupModel
	o.PanelRunUUID, o.PanelRole = &runUUID, storage.PanelRoleSynthesis
	o.PanelName, o.ClaimBlocked = panelName, true
	return o
}
