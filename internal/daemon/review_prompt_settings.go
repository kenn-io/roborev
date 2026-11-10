package daemon

import (
	"encoding/json/v2"
	"fmt"
	"strings"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/prompt"
	"go.kenn.io/roborev/internal/storage"
)

// SuppliedReviewSettings replaces repository review policy for review_prompt.
// Empty values are authoritative; the daemon still owns credentials and runtime controls.
type SuppliedReviewSettings struct {
	Tools           *config.ReviewToolPolicy `json:"tools,omitempty"`
	BinaryEvidence  bool                     `json:"binary_evidence,omitempty"`
	ExcludePatterns []string                 `json:"exclude_patterns,omitempty"`
	BackupAgent     string                   `json:"backup_agent,omitempty"`
	BackupModel     string                   `json:"backup_model,omitempty"`
}

// The existing member-settings column travels with jobs through sync and rerun.
// Execution keeps the original choice after a within-job backup-agent failover.
type suppliedReviewSnapshot struct {
	SuppliedReviewSettings
	Execution experimentJobPlan `json:"execution"`
}

func hasSuppliedReviewSettings(job *storage.ReviewJob) bool {
	return job.Source == storage.JobSourceReviewPrompt && job.PanelMemberConfigJSON != ""
}

func readSuppliedReviewSnapshot(job *storage.ReviewJob) (suppliedReviewSnapshot, error) {
	if job.IsSynthesisJob() {
		cfg, err := synthesisConfig(job)
		if err != nil {
			return suppliedReviewSnapshot{}, err
		}
		if cfg.Supplied == nil {
			return suppliedReviewSnapshot{}, fmt.Errorf("supplied synthesis settings are missing")
		}
		return *cfg.Supplied, nil
	}
	var snapshot suppliedReviewSnapshot
	err := json.Unmarshal([]byte(job.PanelMemberConfigJSON), &snapshot)
	return snapshot, err
}

func snapshotSuppliedReviewSettings(opts *storage.EnqueueOpts, settings SuppliedReviewSettings) error {
	plan, err := experimentPlanForJob(*opts)
	if err != nil {
		return err
	}
	snapshot, err := json.Marshal(suppliedReviewSnapshot{SuppliedReviewSettings: settings, Execution: plan})
	if err != nil {
		return err
	}
	opts.PanelMemberConfigJSON = string(snapshot)
	return nil
}

// A panel rerun starts with its original choices, even after a member or
// synthesis switched to a backup during the previous run.
func restoreSuppliedPanelExecution(opts *storage.EnqueueOpts, job *storage.ReviewJob) error {
	if !hasSuppliedReviewSettings(job) {
		return nil
	}
	snapshot, err := readSuppliedReviewSnapshot(job)
	if err != nil {
		return err
	}
	plan := snapshot.Execution
	opts.Agent, opts.Model, opts.Provider, opts.Reasoning = plan.Agent, plan.Model, plan.Provider, plan.Reasoning
	opts.BackupAgent, opts.BackupModel = plan.BackupAgent, plan.BackupModel
	opts.MinSeverity, opts.ReviewType = plan.MinSeverity, plan.ReviewType
	return nil
}

func validateSuppliedReviewSettings(req *EnqueueRequest) error {
	if req.ReviewSettings == nil {
		return nil
	}
	if strings.TrimSpace(req.ReviewPrompt) == "" {
		return fmt.Errorf("review_settings requires review_prompt")
	}
	if strings.Contains(req.ReviewPrompt, prompt.PriorRangeReviewsFilePathPlaceholder) {
		return fmt.Errorf("review_settings requires prior review context to be included in review_prompt")
	}
	settings := *req.ReviewSettings
	var err error
	settings.Tools, err = config.CanonicalReviewToolPolicy(settings.Tools)
	if err != nil {
		return err
	}
	settings.BackupAgent = strings.TrimSpace(settings.BackupAgent)
	settings.BackupModel = strings.TrimSpace(settings.BackupModel)
	if settings.BackupAgent == "" && settings.BackupModel != "" {
		return fmt.Errorf("backup_model requires backup_agent")
	}
	req.ReviewSettings = &settings
	return nil
}

func resolveSuppliedReviewExecution(in singleAgentInputs) (resolvedSingleAgent, error) {
	resolution, err := agent.ResolveWorkflowConfigFromConfig(in.req.Agent, nil, in.cfg, in.workflow, in.reasoning)
	if err != nil {
		return resolvedSingleAgent{}, err
	}
	settings := in.req.ReviewSettings
	resolution.BackupAgent = ""
	// No repository/default backup can fill an explicitly empty supplied backup.
	selected, err := agent.GetPreferredOrBackupWithConfigFromConfig(nil, resolution.PreferredAgent, in.cfg, settings.BackupAgent)
	if err != nil {
		return resolvedSingleAgent{}, err
	}
	if err := agent.ValidateStructuredReviewSelection(in.req.ReviewType, selected); err != nil {
		return resolvedSingleAgent{}, err
	}
	if err := agent.ValidateReviewToolSelection(settings.Tools, selected, false); err != nil {
		return resolvedSingleAgent{}, err
	}
	result := resolvedSingleAgent{Agent: agent.StorageNameFromConfig(selected.Name(), nil, in.cfg)}
	result.Model = resolution.ModelForSelectedAgent(result.Agent, in.requestedModel)
	if settings.BackupAgent != "" && !resolution.AgentMatches(result.Agent, settings.BackupAgent) {
		backup, err := agent.GetAvailableExactWithConfigFromConfig(nil, settings.BackupAgent, in.cfg)
		if err != nil {
			return resolvedSingleAgent{}, err
		}
		if err := agent.ValidateStructuredReviewSelection(in.req.ReviewType, backup); err != nil {
			return resolvedSingleAgent{}, err
		}
		if err := agent.ValidateReviewToolSelection(settings.Tools, backup, false); err != nil {
			return resolvedSingleAgent{}, err
		}
		result.BackupAgent = agent.StorageNameFromConfig(backup.Name(), nil, in.cfg)
		result.BackupModel = settings.BackupModel
	}
	if settings.BackupAgent != "" && resolution.AgentMatches(result.Agent, settings.BackupAgent) &&
		!resolution.AgentMatches(result.Agent, resolution.PreferredAgent) {
		result.Model = settings.BackupModel
	}
	return result, nil
}

func suppliedReviewRerunOpts(job *storage.ReviewJob, cfg *config.Config, selectedAgent string) (storage.ReenqueueOpts, error) {
	if strings.TrimSpace(selectedAgent) != "" {
		return storage.ReenqueueOpts{}, fmt.Errorf("supplied review settings cannot change agents on rerun; enqueue a new review")
	}
	snapshot, err := readSuppliedReviewSnapshot(job)
	if err != nil {
		return storage.ReenqueueOpts{}, fmt.Errorf("decode supplied review settings: %w", err)
	}
	plan := snapshot.Execution
	if _, err := agent.GetPreferredOrBackupWithConfigFromConfig(nil, plan.Agent, cfg, plan.BackupAgent); err != nil {
		return storage.ReenqueueOpts{}, err
	}
	return storage.ReenqueueOpts{
		Agent: plan.Agent, Model: plan.Model, Provider: plan.Provider, Reasoning: plan.Reasoning,
		ReviewType: plan.ReviewType, MinSeverity: plan.MinSeverity, BackupAgent: plan.BackupAgent, BackupModel: plan.BackupModel, RestorePlan: true,
	}, nil
}
