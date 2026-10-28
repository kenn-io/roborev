package daemon

import (
	"fmt"
	"os"
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

func reviewAgentProblems(cfg *config.Config) []string {
	panels, err := ResolveDoctorPanels("", nil, nil, cfg)
	if err != nil {
		return []string{firstLine(err.Error())}
	}
	var problems []string
	for _, p := range panels {
		if p.Error != "" {
			problems = append(problems, fmt.Sprintf("panel %s: %s", p.Name, firstLine(p.Error)))
		} else if p.Synthesis.Error != "" {
			problems = append(problems, fmt.Sprintf("panel %s synthesis: %s", p.Name, firstLine(p.Synthesis.Error)))
		}
	}
	if len(panels) > 0 {
		return problems
	}
	// No panel applies, so reviews use the single review agent.
	resolution, err := agent.ResolveWorkflowConfigFromConfig("", nil, cfg, "review", "")
	if err == nil {
		_, err = agent.GetPreferredOrBackupWithConfigFromConfig(
			nil, resolution.PreferredAgent, cfg, resolution.BackupAgent,
		)
	}
	if err != nil {
		return []string{firstLine(err.Error())}
	}
	return nil
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}
