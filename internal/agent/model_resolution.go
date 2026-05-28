package agent

import (
	"strings"

	"go.kenn.io/roborev/internal/config"
)

// WorkflowConfig captures the workflow-specific agent resolution context
// shared by CLI, daemon, and batch review callers.
type WorkflowConfig struct {
	RepoPath       string
	GlobalConfig   *config.Config
	Workflow       string
	Reasoning      string
	PreferredAgent string
	BackupAgent    string
}

// ResolveWorkflowConfig resolves the preferred and backup agents for a
// workflow while retaining the workflow and reasoning context needed to
// resolve the final model after an agent has been selected.
//
// This helper intentionally does not validate repo config. Callers that
// must fail fast on malformed .roborev.toml should call
// config.ValidateRepoConfig before invoking it.
func ResolveWorkflowConfig(
	cliAgent, repoPath string,
	globalCfg *config.Config,
	workflow, reasoning string,
) (WorkflowConfig, error) {
	return WorkflowConfig{
		RepoPath:       repoPath,
		GlobalConfig:   globalCfg,
		Workflow:       workflow,
		Reasoning:      reasoning,
		PreferredAgent: config.ResolveAgentForWorkflow(cliAgent, repoPath, globalCfg, workflow, reasoning),
		BackupAgent:    config.ResolveBackupAgentForWorkflow(repoPath, globalCfg, workflow),
	}, nil
}

// AgentMatches reports whether two agent names refer to the same logical
// agent after alias and ACP-name normalization.
func (w WorkflowConfig) AgentMatches(left, right string) bool {
	return workflowModelComparableAgentName(left, w.RepoPath, w.GlobalConfig) ==
		workflowModelComparableAgentName(right, w.RepoPath, w.GlobalConfig)
}

// UsesBackupAgent reports whether the selected agent is the configured
// backup rather than the preferred primary for this workflow.
func (w WorkflowConfig) UsesBackupAgent(selectedAgent string) bool {
	return w.BackupAgent != "" &&
		w.AgentMatches(selectedAgent, w.BackupAgent) &&
		!w.AgentMatches(selectedAgent, w.PreferredAgent)
}

// BackupModel returns the workflow backup model override, if any.
func (w WorkflowConfig) BackupModel() string {
	return config.ResolveBackupModelForWorkflow(
		w.RepoPath, w.GlobalConfig, w.Workflow,
	)
}

// ModelForSelectedAgent resolves the model for the actual selected
// agent. Backup agents use the workflow backup model when no explicit
// CLI model was provided; otherwise the workflow/default precedence used
// by ResolveWorkflowModelForAgent is preserved.
func (w WorkflowConfig) ModelForSelectedAgent(
	selectedAgent, cliModel string,
) string {
	if w.UsesBackupAgent(selectedAgent) &&
		strings.TrimSpace(cliModel) == "" {
		return w.BackupModel()
	}
	model := ResolveWorkflowModelForAgent(
		selectedAgent, cliModel, w.RepoPath,
		w.GlobalConfig, w.Workflow, w.Reasoning,
	)
	// For ACP agents with no workflow model, fall back to configured ACP model
	if model == "" &&
		isConfiguredACPAgentName(selectedAgent, w.GlobalConfig, w.RepoPath) {
		acpCfg := config.ResolveACPAgentConfig(w.RepoPath, w.GlobalConfig)
		if acpCfg != nil && acpCfg.Model != "" {
			return acpCfg.Model
		}
	}
	return model
}

// ResolveWorkflowModelForAgent resolves a workflow model for the actual
// agent that will run. If that agent differs from the generic default
// agent and no explicit model was provided, generic default_model is
// skipped so the selected agent can keep its own built-in default unless
// a workflow-specific model override exists.
func ResolveWorkflowModelForAgent(
	selectedAgent, cliModel, repoPath string,
	globalCfg *config.Config,
	workflow, level string,
) string {
	if s := strings.TrimSpace(cliModel); s != "" {
		return config.ResolveModelForWorkflow(
			s, repoPath, globalCfg, workflow, level,
		)
	}

	selectedAgent = strings.TrimSpace(selectedAgent)
	if selectedAgent == "" {
		return config.ResolveModelForWorkflow(
			"", repoPath, globalCfg, workflow, level,
		)
	}

	defaultAgent := config.ResolveAgent("", repoPath, globalCfg)
	if workflowModelComparableAgentName(selectedAgent, repoPath, globalCfg) !=
		workflowModelComparableAgentName(defaultAgent, repoPath, globalCfg) {
		return config.ResolveWorkflowModel(
			repoPath, globalCfg, workflow, level,
		)
	}

	return config.ResolveModelForWorkflow(
		"", repoPath, globalCfg, workflow, level,
	)
}

func workflowModelComparableAgentName(name, repoPath string, cfg *config.Config) string {
	name = strings.TrimSpace(name)
	if isConfiguredACPAgentName(name, cfg, repoPath) {
		return defaultACPName
	}
	return CanonicalName(name)
}
