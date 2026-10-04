package daemon

import (
	"context"
	"log"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
)

// selectBudgetJobAgent preserves pinned execution contracts and automatic
// recovery. Fresh explicit-agent-only jobs opt into the global budget policy.
func (wp *WorkerPool) selectBudgetJobAgent(ctx context.Context, workerID string, job *storage.ReviewJob, cfg *config.Config) (agent.Agent, bool) {
	if wp.budgetRouter == nil || cfg == nil || !cfg.Budget.Enabled ||
		!hasBudgetPrice(job.Agent, cfg.Budget.AgentCosts) ||
		agent.CanonicalName(job.Agent) == "test" ||
		job.FrozenExperimentPlan != nil || job.BudgetRoutingLocked || job.RetryCount != 0 || job.SessionID != "" ||
		job.RequestedModel != "" || job.RequestedProvider != "" || job.PanelRole != "" ||
		job.IsCIReview() || job.JobType == storage.JobTypeClassify || job.IsSynthesisJob() {
		return nil, true
	}
	configRoot := job.RepoPath
	if job.IsGoalReviewJob() {
		configRoot = GoalReviewConfigRepoPath(job)
	}
	repoCfg, err := config.LoadRepoConfig(configRoot)
	if err != nil {
		if job.UsesStoredPrompt() {
			// Budget substitution must not require config that ordinary
			// stored-prompt execution can run without.
			return nil, true
		}
		wp.logJobStarted(workerID, job)
		wp.failOrRetryContext(ctx, workerID, job, job.Agent, "load budget routing config: "+err.Error())
		return nil, false
	}
	reviewType := ""
	if job.IsReviewJob() || job.JobType == storage.JobTypeCompact {
		reviewType = job.ReviewType
	}
	// A backup model is paired deliberately with that adapter. All other
	// substitutions keep the selected adapter's own default model/provider.
	resolution, resolveErr := agent.ResolveWorkflowConfigFromConfig(job.Agent, repoCfg, cfg, failoverWorkflow(job), job.Reasoning)
	modelForAgent := func(name string) string {
		if agent.CanonicalName(job.Agent) == name {
			return job.Model
		}
		if resolveErr != nil {
			return ""
		}
		if job.BackupAgent != "" {
			if resolution.AgentMatches(name, job.BackupAgent) {
				return job.BackupModel
			}
		} else if resolution.UsesBackupAgent(name) {
			return budgetBackupModel(resolution, name)
		}
		return ""
	}
	var eligible func(agent.Agent) bool
	if job.IsGoalReviewJob() {
		eligible = func(candidate agent.Agent) bool {
			if claude, ok := candidate.WithModel(modelForAgent(candidate.Name())).(*agent.ClaudeAgent); ok {
				return claude.ValidateGoalReview(ctx) == nil
			}
			return true
		}
	}
	selected, err := wp.budgetRouter.ResolveAgent(job.Agent, repoCfg, cfg, reviewType, eligible)
	if err != nil {
		return nil, true
	} // Existing resolution reports agent errors.
	if agent.CanonicalName(job.Agent) == selected.Name() {
		return selected, true
	}
	model := modelForAgent(selected.Name())
	fallbackAgent, fallbackModel := "", ""
	if backupAgent := wp.resolveBackupAgent(job); backupAgent != "" &&
		agent.CanonicalName(backupAgent) == selected.Name() {
		// The selected agent is the current failover target. Preserve the
		// originally resolved job agent as failover after budget routing.
		fallbackAgent = job.Agent
		fallbackModel = job.Model
	}
	updated, err := wp.db.SetJobBudgetAgent(
		job.ID, workerID, selected.Name(), model, fallbackAgent, fallbackModel,
	)
	if err != nil {
		log.Printf("[%s] Persist budget selection: %v", workerID, err)
		wp.logJobStarted(workerID, job)
		wp.failOrRetryContext(ctx, workerID, job, job.Agent, "persist budget selection: "+err.Error())
		return nil, false
	}
	if !updated {
		return nil, false
	}
	log.Printf("[%s] Budget routing selected %s instead of %s", workerID, selected.Name(), job.Agent)
	job.BudgetOriginalAgent = job.Agent
	job.BudgetOriginalBackupAgent = job.BackupAgent
	job.BudgetOriginalBackupModel = job.BackupModel
	job.Agent = selected.Name()
	job.Model = model
	if fallbackAgent != "" {
		job.BackupAgent = fallbackAgent
		job.BackupModel = fallbackModel
	}
	job.Provider = ""
	job.SessionID = ""
	job.BudgetRoutingLocked = true
	return selected, true
}

// budgetBackupModel applies a model only to the agent paired at its source
// layer. Native substitutions need the same protection as ACP substitutions.
func budgetBackupModel(w agent.WorkflowConfig, selected string) string {
	if config.ResolveWorkflowScopedBackupModelFromConfig(w.RepoConfig, nil, w.Workflow) != "" {
		return w.BackupModel()
	}
	if w.GlobalConfig == nil {
		return ""
	}
	paired := w.GlobalConfig.DefaultBackupAgent
	if config.ResolveWorkflowScopedBackupModelFromConfig(nil, w.GlobalConfig, w.Workflow) != "" {
		paired = config.ResolveBackupAgentForWorkflowFromConfig(nil, w.GlobalConfig, w.Workflow)
	}
	if paired == "" || !w.AgentMatches(selected, paired) {
		return ""
	}
	return w.BackupModel()
}
