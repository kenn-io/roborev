package daemon

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"strings"
	"uuid"

	gitrepo "go.kenn.io/kit/git/repo"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/git"
	"go.kenn.io/roborev/internal/storage"
)

// SuppliedPanelExecution selects an agent without consulting repository policy.
type SuppliedPanelExecution struct {
	Agent     string `json:"agent" minLength:"1"`
	Model     string `json:"model,omitempty"`
	Provider  string `json:"provider,omitempty"`
	Reasoning string `json:"reasoning,omitempty"`
}

type SuppliedPanelMember struct {
	SuppliedPanelExecution
	Name           string                 `json:"name" minLength:"1"`
	ReviewPrompt   string                 `json:"review_prompt" minLength:"1"`
	ReviewSettings SuppliedReviewSettings `json:"review_settings"`
	ReviewType     string                 `json:"review_type,omitempty"`
	MinSeverity    string                 `json:"min_severity,omitempty"`
}

type SuppliedPanelSynthesis struct {
	SuppliedPanelExecution
	Instructions string                   `json:"instructions,omitempty"`
	Tools        *config.ReviewToolPolicy `json:"tools,omitempty"`
	BackupAgent  string                   `json:"backup_agent,omitempty"`
	BackupModel  string                   `json:"backup_model,omitempty"`
}

type EnqueueSuppliedPanelInput struct {
	Body struct {
		RepoPath      string                 `json:"repo_path" minLength:"1"`
		GitRef        string                 `json:"git_ref" minLength:"1" doc:"Commit or range resolved once for every reviewer and synthesis"`
		Members       []SuppliedPanelMember  `json:"members" minItems:"1"`
		Synthesis     SuppliedPanelSynthesis `json:"synthesis"`
		HoldSynthesis bool                   `json:"hold_synthesis,omitempty"`
	}
}

func (s *Server) humaEnqueueSuppliedPanel(ctx context.Context, input *EnqueueSuppliedPanelInput) (*RawJSONOutput, error) {
	req := input.Body
	if strings.TrimSpace(req.RepoPath) == "" || strings.TrimSpace(req.GitRef) == "" || req.GitRef == "dirty" || len(req.Members) == 0 {
		return rawJSONOutput(http.StatusBadRequest, ErrorResponse{Error: "repo_path, a committed git_ref, and at least one member are required"})
	}
	metadata := git.OpenEnqueueMetadataReader(ctx, req.RepoPath)
	checkoutRoot, err := metadata.Root()
	if err != nil {
		return rawJSONOutput(http.StatusBadRequest, ErrorResponse{Error: fmt.Sprintf("not a git repository: %v", err)})
	}
	repoRoot, err := gitrepo.MainRoot(ctx, checkoutRoot)
	if err != nil {
		return rawJSONOutput(http.StatusBadRequest, ErrorResponse{Error: err.Error()})
	}
	repo, err := s.db.GetOrCreateRepo(repoRoot, config.ResolveRepoIdentity(repoRoot, nil))
	if err != nil {
		return rawJSONOutput(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}
	// An explicit empty policy prevents branch/commit exclusions from the checkout.
	descriptor, early := s.buildTargetDescriptor(ctx, freezeInputs{
		repo: repo, gitRef: req.GitRef, checkoutRoot: checkoutRoot, repoRoot: repoRoot, metadata: metadata,
		req: EnqueueRequest{Source: storage.JobSourceReviewPrompt, ReviewSettings: &SuppliedReviewSettings{}},
	})
	if early != nil {
		return early, nil
	}
	descriptor.jobType = storage.JobTypeForEnqueue(descriptor.baseOpts())
	descriptor.branch = git.InferBranchForCommit(ctx, checkoutRoot, descriptor.sessionSHA)
	cfg := s.configWatcher.Config().ForRepo(checkoutRoot)
	run := uuid.New()
	members := make([]storage.EnqueueOpts, len(req.Members))
	names := make(map[string]bool, len(req.Members))
	for i, member := range req.Members {
		member.Name = strings.TrimSpace(member.Name)
		if member.Name == "" || names[member.Name] {
			return rawJSONOutput(http.StatusBadRequest, ErrorResponse{Error: "member names must be nonempty and unique"})
		}
		names[member.Name] = true
		memberReq := EnqueueRequest{
			Agent: member.Agent, Model: member.Model, Provider: member.Provider, Reasoning: member.Reasoning,
			ReviewPrompt: member.ReviewPrompt, ReviewSettings: &member.ReviewSettings,
			ReviewType: member.ReviewType, MinSeverity: member.MinSeverity,
		}
		members[i], err = suppliedPanelJobOpts(descriptor, memberReq, cfg)
		if err != nil {
			return rawJSONOutput(http.StatusBadRequest, ErrorResponse{Error: fmt.Sprintf("member %q: %v", member.Name, err)})
		}
		members[i].PanelRunUUID, members[i].PanelRole = &run, storage.PanelRoleMember
		members[i].PanelName, members[i].PanelMemberName, members[i].PanelMemberIndex = "supplied", member.Name, i
	}
	synth := req.Synthesis
	synthesis, err := suppliedPanelJobOpts(descriptor, EnqueueRequest{
		Agent: synth.Agent, Model: synth.Model, Provider: synth.Provider, Reasoning: synth.Reasoning,
		// This is only execution resolution. The synthesis worker builds its prompt
		// from completed member results and the frozen instructions below.
		ReviewPrompt: "synthesis", ReviewSettings: &SuppliedReviewSettings{
			Tools: synth.Tools, BackupAgent: synth.BackupAgent, BackupModel: synth.BackupModel,
		},
	}, cfg)
	if err != nil {
		return rawJSONOutput(http.StatusBadRequest, ErrorResponse{Error: fmt.Sprintf("synthesis: %v", err)})
	}
	var frozen suppliedReviewSnapshot
	if err := json.Unmarshal([]byte(synthesis.PanelMemberConfigJSON), &frozen); err != nil {
		return nil, err
	}
	synthesis.JobType, synthesis.Prompt, synthesis.PromptPrebuilt = storage.JobTypeSynthesis, "", false
	synthesis.PanelRunUUID, synthesis.PanelRole, synthesis.PanelName = &run, storage.PanelRoleSynthesis, "supplied"
	synthesis.PanelMemberConfigJSON, err = (synthesisJobConfig{
		Instructions: synth.Instructions, HoldSynthesis: req.HoldSynthesis, Supplied: &frozen,
	}).encode()
	if err != nil {
		return nil, err
	}
	jobs, parent, err := s.db.EnqueuePanelRun(members, synthesis)
	if err != nil {
		return rawJSONOutput(http.StatusInternalServerError, ErrorResponse{Error: fmt.Sprintf("enqueue panel: %v", err)})
	}
	ids := make([]int64, len(jobs))
	for i, job := range jobs {
		ids[i] = job.ID
	}
	parent.RepoPath, parent.RepoName = repo.RootPath, repo.Name
	s.logEnqueueSideEffects(parent, enqueueSideEffectInputs{repo: repo, gitRef: descriptor.gitRef, agentName: parent.Agent, reviewType: parent.ReviewType})
	return rawJSONOutput(http.StatusCreated, PanelEnqueueResponse{ReviewJob: parent, PanelRunUUID: run, MemberJobIDs: ids})
}

func suppliedPanelJobOpts(descriptor targetDescriptor, req EnqueueRequest, cfg *config.Config) (storage.EnqueueOpts, error) {
	if strings.TrimSpace(req.Agent) == "" {
		return storage.EnqueueOpts{}, fmt.Errorf("agent is required")
	}
	if err := validateSuppliedReviewSettings(&req); err != nil {
		return storage.EnqueueOpts{}, err
	}
	if req.ReviewType == "" {
		req.ReviewType = config.ReviewTypeDefault
	}
	if req.ReviewType == config.ReviewTypeGoal {
		return storage.EnqueueOpts{}, fmt.Errorf("panels require code reviews")
	}
	types, err := config.ValidateReviewTypesFromConfig([]string{req.ReviewType}, nil, cfg)
	if err != nil {
		return storage.EnqueueOpts{}, err
	}
	req.ReviewType = types[0]
	reasoning, err := config.ResolveReviewReasoningForTypeFromConfig(req.Reasoning, nil, cfg, req.ReviewType)
	if err != nil {
		return storage.EnqueueOpts{}, err
	}
	execution, err := resolveSuppliedReviewExecution(singleAgentInputs{
		req: req, cfg: cfg, workflow: config.WorkflowForReviewType(req.ReviewType), reasoning: reasoning, requestedModel: strings.TrimSpace(req.Model),
	})
	if err != nil {
		return storage.EnqueueOpts{}, err
	}
	opts := descriptor.baseOpts()
	opts.Agent, opts.Model, opts.Provider, opts.Reasoning = execution.Agent, execution.Model, strings.TrimSpace(req.Provider), reasoning
	opts.RequestedModel, opts.RequestedProvider = strings.TrimSpace(req.Model), opts.Provider
	opts.ReviewType, opts.Prompt, opts.PromptPrebuilt = req.ReviewType, req.ReviewPrompt, true
	opts.BackupAgent, opts.BackupModel = execution.BackupAgent, execution.BackupModel
	if strings.TrimSpace(req.MinSeverity) != "" {
		opts.MinSeverity, err = config.NormalizeMinSeverity(req.MinSeverity)
		if err != nil {
			return storage.EnqueueOpts{}, err
		}
	}
	if err := snapshotSuppliedReviewSettings(&opts, *req.ReviewSettings); err != nil {
		return storage.EnqueueOpts{}, err
	}
	return opts, nil
}
