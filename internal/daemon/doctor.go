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
	resp.Body.Panels = ResolveDoctorPanels(repo, repoCfg, cfg)
	return resp, nil
}

// ResolveDoctorPanels resolves the panels selected for post-commit and manual
// reviews with the same member and synthesis resolution the daemon uses when
// it queues a panel review. Agent lookups use the calling process's PATH, so
// the daemon's answer is authoritative; the CLI calls this directly only when
// the daemon cannot be asked.
func ResolveDoctorPanels(repo string, repoCfg *config.RepoConfig, cfg *config.Config) []DoctorPanel {
	merged := config.MergeReviewConfigFromConfig(repoCfg, cfg)
	var panels []DoctorPanel
	add := func(name, use string) {
		if name == "" {
			return
		}
		for i := range panels {
			if panels[i].Name == name {
				panels[i].UsedFor = append(panels[i].UsedFor, use)
				return
			}
		}
		panels = append(panels, DoctorPanel{Name: name, UsedFor: []string{use}})
	}
	add(merged.HookPanel, "post_commit")
	add(merged.DefaultPanel, "manual")

	for i := range panels {
		p := &panels[i]
		members, synth, err := config.ResolvePanel(p.Name, repo, cfg)
		if err != nil {
			p.Error = err.Error()
			continue
		}
		for _, m := range members {
			dm := DoctorPanelMember{Name: m.Name}
			selected, _, _, _, err := resolvePanelMemberExecution(m, targetDescriptor{}, repoCfg, cfg)
			if err != nil {
				dm.Error = err.Error()
			} else {
				dm.Agent = selected
			}
			p.Members = append(p.Members, dm)
		}
		p.Synthesis = agent.Diagnosis{Name: synth.Agent}
		if a, err := agent.GetPreferredOrBackupWithConfigFromConfig(repoCfg, synth.Agent, cfg, synth.BackupAgent); err != nil {
			p.Synthesis.Error = err.Error()
		} else {
			p.Synthesis.Available = true
			if ca, ok := a.(agent.CommandAgent); ok {
				p.Synthesis.Command = ca.CommandName()
			}
		}
	}
	return panels
}
