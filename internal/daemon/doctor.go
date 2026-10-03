package daemon

import (
	"context"
	"maps"
	"os"
	"os/exec"
	"slices"
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
	var rawRepo map[string]any
	if repo != "" {
		cfg = cfg.ForRepo(repo)
		loaded, raw, err := config.LoadRepoConfigWithRaw(repo)
		if err != nil {
			resp.Body.RepoConfigError = err.Error()
		} else {
			repoCfg, rawRepo = loaded, raw
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
	resp.Body.Panels = ResolveDoctorPanels(repoCfg, rawRepo, cfg)
	return resp, nil
}

// ResolveDoctorPanels resolves the panels selected for post-commit and manual
// reviews with the same member and synthesis resolution the daemon uses when
// it queues a panel review. It also resolves any different panel an enabled
// experiment selects, labelled with the experiment ID. Agent lookups use the
// calling process's PATH, so the daemon's answer is authoritative; the CLI
// calls this directly only when the daemon cannot be asked.
func ResolveDoctorPanels(repoCfg *config.RepoConfig, rawRepo map[string]any, cfg *config.Config) []DoctorPanel {
	type selection struct {
		experiment string
		repoCfg    *config.RepoConfig
	}
	selections := []selection{{repoCfg: repoCfg}}
	if experiments, err := config.EnabledExperimentConfigs(cfg, repoCfg, rawRepo); err == nil {
		for _, id := range slices.Sorted(maps.Keys(experiments)) {
			selections = append(selections, selection{experiment: id, repoCfg: experiments[id]})
		}
	}

	var panels []DoctorPanel
	checked := map[string]bool{} // "use/panel" pairs already reported
	for _, sel := range selections {
		merged := config.MergeReviewConfigFromConfig(sel.repoCfg, cfg)
		var batch []DoctorPanel
		add := func(name, use string) {
			if name == "" || checked[use+"/"+name] {
				return
			}
			checked[use+"/"+name] = true
			for i := range batch {
				if batch[i].Name == name {
					batch[i].UsedFor = append(batch[i].UsedFor, use)
					return
				}
			}
			batch = append(batch, DoctorPanel{Name: name, UsedFor: []string{use}, Experiment: sel.experiment})
		}
		add(merged.HookPanel, "post_commit")
		add(merged.DefaultPanel, "manual")
		for i := range batch {
			resolveDoctorPanel(&batch[i], sel.repoCfg, cfg)
		}
		panels = append(panels, batch...)
	}
	return panels
}

// resolveDoctorPanel fills in one panel's members and synthesis agent from
// already-loaded config, so an experiment's effective config is used as is.
func resolveDoctorPanel(p *DoctorPanel, repoCfg *config.RepoConfig, cfg *config.Config) {
	members, synth, err := config.ResolveCIPanel(p.Name, repoCfg, cfg)
	if err != nil {
		p.Error = err.Error()
		return
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
