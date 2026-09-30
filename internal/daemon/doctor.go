package daemon

import (
	"context"
	"os"
	"strings"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
)

// humaDoctorAgents reports which agents the daemon process can run. The CLI's
// doctor command compares this with its own view to find agents that are on
// the user's PATH but not on the daemon's.
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
	return resp, nil
}
