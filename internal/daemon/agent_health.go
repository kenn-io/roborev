package daemon

import (
	"fmt"
	"os"
	"slices"
	"strings"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
)

// reviewAgentHealth reports whether this daemon can run the review agents
// that the global config selects for post-commit and manual reviews. Enqueue
// rejects a review whose agents cannot run, and a post-commit hook has no
// terminal to report that on, so the daemon reports it here instead. Repo
// configs can select other agents; `roborev doctor` checks those per repo.
func reviewAgentHealth(cfg *config.Config) storage.ComponentHealth {
	problems := reviewAgentProblems(cfg)
	if len(problems) == 0 {
		return storage.ComponentHealth{Name: "agents", Healthy: true}
	}
	return storage.ComponentHealth{
		Name:    "agents",
		Healthy: false,
		Message: fmt.Sprintf("%s (daemon PATH=%s)", strings.Join(problems, "; "), os.Getenv("PATH")),
	}
}

// reviewAgentProblems checks every kind of review the global config can start:
// post-commit and manual reviews, in the default arm and in the experimental
// arm of an enabled review experiment. Each one runs the panel it selects, or
// else the single review agent, the way enqueue resolves them.
func reviewAgentProblems(cfg *config.Config) []string {
	arms, err := config.WorkflowExperimentArms(config.ExperimentWorkflowReview, cfg, nil, nil)
	if err != nil {
		return []string{firstLine(err.Error())}
	}
	var problems []string
	add := func(problem string) {
		if !slices.Contains(problems, problem) {
			problems = append(problems, problem)
		}
	}
	for _, arm := range arms {
		for _, src := range doctorPanelSources {
			name := arm.PanelName(src.source)
			if name == "" {
				if err := singleReviewAgentError(arm.RepoConfig, cfg); err != nil {
					add(firstLine(err.Error()))
				}
				continue
			}
			p := planDoctorPanel("", name, arm.RepoConfig, cfg)
			if p.Error != "" {
				add(fmt.Sprintf("panel %s: %s", p.Name, firstLine(p.Error)))
			} else if p.Synthesis.Error != "" {
				add(fmt.Sprintf("panel %s synthesis: %s", p.Name, firstLine(p.Synthesis.Error)))
			}
		}
	}
	return problems
}

// singleReviewAgentError reports why the single review agent cannot run. It
// resolves the reasoning level first because reasoning-specific agent
// settings such as review_agent_thorough select the agent.
func singleReviewAgentError(repoCfg *config.RepoConfig, cfg *config.Config) error {
	reasoning, err := config.ResolveReviewReasoningFromConfig("", repoCfg, cfg)
	if err != nil {
		return err
	}
	resolution, err := agent.ResolveWorkflowConfigFromConfig("", repoCfg, cfg, "review", reasoning)
	if err != nil {
		return err
	}
	_, err = agent.GetPreferredOrBackupWithConfigFromConfig(
		repoCfg, resolution.PreferredAgent, cfg, resolution.BackupAgent,
	)
	return err
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}
