package daemon

import (
	"context"
	"os"
	"os/exec"
	"strings"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
)

// hookTools maps a [[hooks]] type to the CLI the daemon runs for it.
var hookTools = map[string]string{
	"kata":  "kata",
	"beads": "bd",
}

// humaDoctorAgents reports which agents and hook tools the daemon process can
// run. The CLI's doctor command compares this with its own view to find
// commands that are on the user's PATH but not on the daemon's.
func (s *Server) humaDoctorAgents(
	ctx context.Context, input *DoctorAgentsInput,
) (*DoctorAgentsOutput, error) {
	repo := strings.TrimSpace(input.Repo)
	cfg := s.configWatcher.Config()
	resp := &DoctorAgentsOutput{}
	var repoCfg *config.RepoConfig
	if repo != "" {
		cfg = cfg.ForRepo(repo)
		loaded, err := config.LoadRepoConfig(repo)
		if err != nil {
			resp.Body.RepoConfigError = err.Error()
		} else {
			repoCfg = loaded
		}
	}
	resp.Body.PathEnv = os.Getenv("PATH")
	resp.Body.Agents = agent.DiagnoseAll(repoCfg, cfg)
	for _, name := range input.Agent {
		if name = strings.TrimSpace(name); name != "" {
			resp.Body.Requested = append(resp.Body.Requested, agent.Diagnose(repoCfg, name, cfg))
		}
	}

	hooks := append([]config.HookConfig{}, cfg.Hooks...)
	if repoCfg != nil {
		hooks = append(hooks, repoCfg.Hooks...)
	}
	seen := map[string]bool{}
	for _, h := range hooks {
		tool, ok := hookTools[h.Type]
		if !ok || seen[tool] {
			continue
		}
		seen[tool] = true
		d := agent.Diagnosis{Name: tool, Command: tool}
		if path, err := exec.LookPath(tool); err != nil {
			d.Error = err.Error()
		} else {
			d.Available = true
			d.Path = path
		}
		resp.Body.HookTools = append(resp.Body.HookTools, d)
	}
	return resp, nil
}
