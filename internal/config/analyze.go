package config

import (
	"strings"
)

// AnalyzeConfig holds per-analysis-type overrides such as:
//
//	[analyze.refactor]
//	agent = "claude-code"
//	model = "sonnet"
//	reasoning = "fast"
type AnalyzeConfig struct {
	Agent     string `toml:"agent" comment:"Agent override for this analysis type."`
	Model     string `toml:"model" comment:"Model override for this analysis type."`
	Reasoning string `toml:"reasoning" comment:"Reasoning level for this analysis type: fast, standard, medium, thorough, or maximum."`
}

// ResolveAnalyzeConfig resolves the agent/model/reasoning that should be sent
// on an analyze enqueue request. It keeps the usual config layering:
// CLI > repo analyze.<type> > repo workflow/generic > global analyze.<type> >
// global workflow/generic > default.
func ResolveAnalyzeConfig(
	cliAgent, cliModel, cliReasoning string,
	repoPath string,
	globalCfg *Config,
	analysisType string,
	fallbackWorkflow string,
	fallbackLevel string,
) (AnalyzeConfig, error) {
	repoCfg, _ := LoadRepoConfig(repoPath)
	return ResolveAnalyzeConfigFromConfig(
		cliAgent, cliModel, cliReasoning,
		repoCfg, globalCfg, analysisType, fallbackWorkflow, fallbackLevel,
	)
}

// ResolveAnalyzeConfigFromConfig is the config-taking core of
// ResolveAnalyzeConfig, never reading the working tree.
func ResolveAnalyzeConfigFromConfig(
	cliAgent, cliModel, cliReasoning string,
	repoCfg *RepoConfig,
	globalCfg *Config,
	analysisType string,
	fallbackWorkflow string,
	fallbackLevel string,
) (AnalyzeConfig, error) {
	reasoning, err := resolveAnalyzeReasoning(
		cliReasoning, repoCfg, globalCfg, analysisType, fallbackLevel,
	)
	if err != nil {
		return AnalyzeConfig{}, err
	}

	return AnalyzeConfig{
		Agent: resolveAnalyzeAgent(
			cliAgent, repoCfg, globalCfg, analysisType,
			fallbackWorkflow, reasoning,
		),
		Model: resolveAnalyzeModel(
			cliModel, repoCfg, globalCfg, analysisType,
			fallbackWorkflow, reasoning,
		),
		Reasoning: reasoning,
	}, nil
}

func resolveAnalyzeAgent(
	cli string,
	repoCfg *RepoConfig,
	globalCfg *Config,
	analysisType string,
	fallbackWorkflow string,
	level string,
) string {
	if s := strings.TrimSpace(cli); s != "" {
		return s
	}
	if s := repoAnalyzeField(repoCfg, analysisType, true); s != "" {
		return s
	}
	if s := repoWorkflowField(repoCfg, fallbackWorkflow, level, true); s != "" {
		return s
	}
	if s := repoWorkflowField(repoCfg, fallbackWorkflow, "", true); s != "" {
		return s
	}
	if repoCfg != nil && strings.TrimSpace(repoCfg.Agent) != "" {
		return strings.TrimSpace(repoCfg.Agent)
	}
	if s := globalAnalyzeField(globalCfg, analysisType, true); s != "" {
		return s
	}
	if s := globalWorkflowField(globalCfg, fallbackWorkflow, level, true); s != "" {
		return s
	}
	if s := globalWorkflowField(globalCfg, fallbackWorkflow, "", true); s != "" {
		return s
	}
	if globalCfg != nil && strings.TrimSpace(globalCfg.DefaultAgent) != "" {
		return strings.TrimSpace(globalCfg.DefaultAgent)
	}
	return "codex"
}

func resolveAnalyzeModel(
	cli string,
	repoCfg *RepoConfig,
	globalCfg *Config,
	analysisType string,
	fallbackWorkflow string,
	level string,
) string {
	if s := strings.TrimSpace(cli); s != "" {
		return s
	}
	if s := repoAnalyzeField(repoCfg, analysisType, false); s != "" {
		return s
	}
	if s := repoWorkflowField(repoCfg, fallbackWorkflow, level, false); s != "" {
		return s
	}
	if s := repoWorkflowField(repoCfg, fallbackWorkflow, "", false); s != "" {
		return s
	}
	if repoCfg != nil && strings.TrimSpace(repoCfg.Model) != "" {
		return strings.TrimSpace(repoCfg.Model)
	}
	if s := globalAnalyzeField(globalCfg, analysisType, false); s != "" {
		return s
	}
	if s := globalWorkflowField(globalCfg, fallbackWorkflow, level, false); s != "" {
		return s
	}
	if s := globalWorkflowField(globalCfg, fallbackWorkflow, "", false); s != "" {
		return s
	}
	if globalCfg != nil && strings.TrimSpace(globalCfg.DefaultModel) != "" {
		return strings.TrimSpace(globalCfg.DefaultModel)
	}
	return ""
}

func resolveAnalyzeReasoning(
	cli string,
	repoCfg *RepoConfig,
	globalCfg *Config,
	analysisType string,
	fallbackLevel string,
) (string, error) {
	if s := strings.TrimSpace(cli); s != "" {
		return NormalizeReasoning(s)
	}
	if s := repoAnalyzeReasoning(repoCfg, analysisType); s != "" {
		return NormalizeReasoning(s)
	}
	if repoCfg != nil && strings.TrimSpace(repoCfg.ReviewReasoning) != "" {
		return NormalizeReasoning(repoCfg.ReviewReasoning)
	}
	if s := globalAnalyzeReasoning(globalCfg, analysisType); s != "" {
		return NormalizeReasoning(s)
	}
	if globalCfg != nil && strings.TrimSpace(globalCfg.ReviewReasoning) != "" {
		return NormalizeReasoning(globalCfg.ReviewReasoning)
	}
	if s := strings.TrimSpace(fallbackLevel); s != "" {
		return NormalizeReasoning(s)
	}
	return "thorough", nil
}

func repoAnalyzeField(repoCfg *RepoConfig, analysisType string, isAgent bool) string {
	if repoCfg == nil {
		return ""
	}
	return analyzeField(repoCfg.Analyze, analysisType, isAgent)
}

func globalAnalyzeField(globalCfg *Config, analysisType string, isAgent bool) string {
	if globalCfg == nil {
		return ""
	}
	return analyzeField(globalCfg.Analyze, analysisType, isAgent)
}

func analyzeField(configs map[string]AnalyzeConfig, analysisType string, isAgent bool) string {
	cfg, ok := configs[strings.TrimSpace(analysisType)]
	if !ok {
		return ""
	}
	if isAgent {
		return strings.TrimSpace(cfg.Agent)
	}
	return strings.TrimSpace(cfg.Model)
}

func repoAnalyzeReasoning(repoCfg *RepoConfig, analysisType string) string {
	if repoCfg == nil {
		return ""
	}
	return analyzeReasoning(repoCfg.Analyze, analysisType)
}

func globalAnalyzeReasoning(globalCfg *Config, analysisType string) string {
	if globalCfg == nil {
		return ""
	}
	return analyzeReasoning(globalCfg.Analyze, analysisType)
}

func analyzeReasoning(configs map[string]AnalyzeConfig, analysisType string) string {
	cfg, ok := configs[strings.TrimSpace(analysisType)]
	if !ok {
		return ""
	}
	return strings.TrimSpace(cfg.Reasoning)
}
