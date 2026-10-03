package goalreview

import (
	"fmt"
	"strings"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
)

type AgentOptions struct{ Agent, Model, Provider, Reasoning string }

func Select(repo *config.RepoConfig, spec, plan *string) (Selection, error) {
	if spec == nil && plan == nil {
		cfg := config.ResolveGoalReview(repo)
		spec, plan = cfg.SpecFile, cfg.PlanFile
	}
	selection := Selection{}
	for _, entry := range []struct {
		value  *string
		target *string
	}{{spec, &selection.SpecFile}, {plan, &selection.PlanFile}} {
		if entry.value != nil {
			if strings.TrimSpace(*entry.value) == "" {
				return Selection{}, fmt.Errorf("explicit Superpowers artifact path must not be blank")
			}
			*entry.target = *entry.value
		}
	}
	return selection, nil
}

func ResolveAgent(root string, cfg *config.Config, options AgentOptions) (agent.Agent, string, error) {
	reasoning, err := config.ResolveReviewReasoning(options.Reasoning, root, cfg)
	if err != nil {
		return nil, "", err
	}
	if err := config.ValidateRepoConfig(root); err != nil {
		return nil, "", err
	}
	resolution, err := agent.ResolveWorkflowConfig(options.Agent, root, cfg, "review", reasoning)
	if err != nil {
		return nil, "", err
	}
	a, err := agent.GetPreferredOrBackupWithConfig(root, resolution.PreferredAgent, cfg, resolution.BackupAgent)
	if err != nil {
		return nil, "", err
	}
	if err := ValidateAgent(a); err != nil {
		return nil, "", err
	}
	model := resolution.ModelForSelectedAgent(a.Name(), options.Model)
	a = a.WithModel(model).WithReasoning(agent.ParseReasoningLevel(reasoning))
	if options.Provider != "" {
		if pi, ok := a.(*agent.PiAgent); ok {
			a = pi.WithProvider(options.Provider)
		}
	}
	return a, reasoning, ValidateAgent(a)
}
