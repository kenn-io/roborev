package agent

import (
	"strings"

	"go.kenn.io/roborev/internal/config"
)

// WorkflowConfig captures the workflow-specific agent resolution context
// shared by CLI, daemon, and batch review callers.
type WorkflowConfig struct {
	RepoPath       string
	RepoConfig     *config.RepoConfig
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
	repoCfg, _ := config.LoadRepoConfig(repoPath)
	resolution, err := ResolveWorkflowConfigFromConfig(
		cliAgent, repoCfg, globalCfg, workflow, reasoning,
	)
	if err != nil {
		return WorkflowConfig{}, err
	}
	resolution.RepoPath = repoPath
	return resolution, nil
}

// ResolveWorkflowConfigFromConfig resolves workflow agent/model context from
// already-loaded config, never reading the working tree.
func ResolveWorkflowConfigFromConfig(
	cliAgent string,
	repoCfg *config.RepoConfig,
	globalCfg *config.Config,
	workflow, reasoning string,
) (WorkflowConfig, error) {
	if repoCfg == nil {
		repoCfg = &config.RepoConfig{}
	}
	return WorkflowConfig{
		RepoConfig:     repoCfg,
		GlobalConfig:   globalCfg,
		Workflow:       workflow,
		Reasoning:      reasoning,
		PreferredAgent: config.ResolveAgentForWorkflowFromConfig(cliAgent, repoCfg, globalCfg, workflow, reasoning),
		BackupAgent:    config.ResolveBackupAgentForWorkflowFromConfig(repoCfg, globalCfg, workflow),
	}, nil
}

// AgentMatches reports whether two agent names refer to the same logical
// agent after alias and ACP-name normalization.
func (w WorkflowConfig) AgentMatches(left, right string) bool {
	if w.RepoConfig != nil {
		return workflowModelComparableAgentNameFromConfig(left, w.RepoConfig, w.GlobalConfig) ==
			workflowModelComparableAgentNameFromConfig(right, w.RepoConfig, w.GlobalConfig)
	}
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
	if w.RepoConfig != nil {
		return config.ResolveBackupModelForWorkflowFromConfig(
			w.RepoConfig, w.GlobalConfig, w.Workflow,
		)
	}
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
	var model string
	if w.RepoConfig != nil {
		model = ResolveWorkflowModelForAgentFromConfig(
			selectedAgent, cliModel, w.RepoConfig,
			w.GlobalConfig, w.Workflow, w.Reasoning,
		)
	} else {
		model = ResolveWorkflowModelForAgent(
			selectedAgent, cliModel, w.RepoPath,
			w.GlobalConfig, w.Workflow, w.Reasoning,
		)
	}
	// For ACP agents with no workflow model, fall back to configured ACP model
	if model == "" &&
		w.isConfiguredACPAgentName(selectedAgent) {
		acpCfg := w.resolveACPAgentConfig()
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
	repoCfg, _ := config.LoadRepoConfig(repoPath)
	return ResolveWorkflowModelForAgentFromConfig(
		selectedAgent, cliModel, repoCfg, globalCfg, workflow, level,
	)
}

// ResolveWorkflowModelForAgentFromConfig is the config-taking core of
// ResolveWorkflowModelForAgent, never reading the working tree.
func ResolveWorkflowModelForAgentFromConfig(
	selectedAgent, cliModel string,
	repoCfg *config.RepoConfig,
	globalCfg *config.Config,
	workflow, level string,
) string {
	if s := strings.TrimSpace(cliModel); s != "" {
		return config.ResolveModelForWorkflowFromConfig(
			s, repoCfg, globalCfg, workflow, level,
		)
	}

	selectedAgent = strings.TrimSpace(selectedAgent)
	if selectedAgent == "" {
		return config.ResolveModelForWorkflowFromConfig(
			"", repoCfg, globalCfg, workflow, level,
		)
	}

	defaultAgent := config.ResolveAgentFromConfig("", repoCfg, globalCfg)
	if workflowModelComparableAgentNameFromConfig(selectedAgent, repoCfg, globalCfg) !=
		workflowModelComparableAgentNameFromConfig(defaultAgent, repoCfg, globalCfg) {
		// Workflow models follow their paired workflow agent. When the
		// selected agent is the configured ACP agent but the workflow's
		// configured agent for this workflow+level is NOT that ACP agent, the
		// workflow model (e.g. a global review_model meant for the default
		// reviewer) is paired with a different agent. Returning "" lets the
		// ACP-config-model fallback in ModelForSelectedAgent supply
		// [acp].model rather than handing a foreign model to the ACP agent,
		// which its model validation would reject. Scope guard: this only
		// affects ACP-selected agents; non-ACP agents keep legacy behavior.
		if acpSelectedWithUnpairedWorkflowAgent(
			selectedAgent, repoCfg, globalCfg, workflow, level,
		) {
			return ""
		}
		return config.ResolveWorkflowModelFromConfig(
			repoCfg, globalCfg, workflow, level,
		)
	}

	// Selected agent IS the default agent. The generic model (repo model /
	// global default_model) pairs with the default agent and still applies,
	// but a workflow model pairs with the workflow-configured agent: when
	// the selected ACP agent is the default yet the workflow agent resolves
	// to a different agent, skip the workflow model and resolve only the
	// generic chain. If that is empty, the ACP-config-model fallback in
	// ModelForSelectedAgent supplies [acp].model.
	if acpSelectedWithUnpairedWorkflowAgent(
		selectedAgent, repoCfg, globalCfg, workflow, level,
	) {
		return config.ResolveModelFromConfig("", repoCfg, globalCfg)
	}

	return config.ResolveModelForWorkflowFromConfig(
		"", repoCfg, globalCfg, workflow, level,
	)
}

// acpSelectedWithUnpairedWorkflowAgent reports whether the selected agent is
// the configured ACP agent while the workflow-configured agent for this
// workflow+level resolves to a different agent. In that case workflow models
// are paired with that other agent and must not be handed to the ACP agent.
func acpSelectedWithUnpairedWorkflowAgent(
	selectedAgent string,
	repoCfg *config.RepoConfig,
	globalCfg *config.Config,
	workflow, level string,
) bool {
	if !isConfiguredACPAgentNameFromConfig(selectedAgent, globalCfg, repoCfg) {
		return false
	}
	workflowAgent := config.ResolveAgentForWorkflowFromConfig(
		"", repoCfg, globalCfg, workflow, level,
	)
	return workflowModelComparableAgentNameFromConfig(workflowAgent, repoCfg, globalCfg) !=
		workflowModelComparableAgentNameFromConfig(selectedAgent, repoCfg, globalCfg)
}

func workflowModelComparableAgentName(name string, repoPath string, cfg *config.Config) string {
	name = strings.TrimSpace(name)
	if isConfiguredACPAgentName(name, cfg, repoPath) {
		return defaultACPName
	}
	return CanonicalName(name)
}

func workflowModelComparableAgentNameFromConfig(name string, repoCfg *config.RepoConfig, cfg *config.Config) string {
	name = strings.TrimSpace(name)
	if isConfiguredACPAgentNameFromConfig(name, cfg, repoCfg) {
		return defaultACPName
	}
	return CanonicalName(name)
}

func (w WorkflowConfig) isConfiguredACPAgentName(name string) bool {
	if w.RepoConfig != nil {
		return isConfiguredACPAgentNameFromConfig(name, w.GlobalConfig, w.RepoConfig)
	}
	return isConfiguredACPAgentName(name, w.GlobalConfig, w.RepoPath)
}

func (w WorkflowConfig) resolveACPAgentConfig() *config.ACPAgentConfig {
	if w.RepoConfig != nil {
		return config.ResolveACPAgentConfigFromConfig(w.RepoConfig, w.GlobalConfig)
	}
	return config.ResolveACPAgentConfig(w.RepoPath, w.GlobalConfig)
}
