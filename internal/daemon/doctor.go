package daemon

import (
	"context"
	"os"
	"os/exec"
	"slices"
	"strings"
	"uuid"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
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
	panels, err := ResolveDoctorPanels(repo, repoCfg, rawRepo, cfg)
	if err != nil {
		resp.Body.PanelsError = err.Error()
	}
	resp.Body.Panels = panels
	return resp, nil
}

// doctorPanelSources maps each kind of review to the job source that selects
// its panel: post-commit reviews use hook_review_panel, manual reviews use
// default_panel.
var doctorPanelSources = []struct{ use, source string }{
	{"post_commit", storage.JobSourcePostCommit},
	{"manual", ""},
}

// ResolveDoctorPanels reports what queueing a post-commit or manual review
// would run, for every config a review can use: the default arm and, when a
// review experiment is enabled, its experimental arm. Panels are selected and
// planned by the same functions that queue a review, and the synthesis agent
// is resolved the way the worker resolves it. Identical outcomes are merged;
// an experimental arm is listed separately only when its outcome differs.
// Agent lookups use the calling process's PATH, so the daemon's answer is
// authoritative; the CLI calls this directly only when the daemon cannot be
// asked.
func ResolveDoctorPanels(
	repo string, repoCfg *config.RepoConfig, rawRepo map[string]any, cfg *config.Config,
) ([]DoctorPanel, error) {
	arms, err := config.WorkflowExperimentArms(config.ExperimentWorkflowReview, cfg, repoCfg, rawRepo)
	if err != nil {
		return nil, err
	}
	var panels []DoctorPanel
	for _, arm := range arms {
		merged := config.MergeReviewConfigFromConfig(arm.RepoConfig, cfg)
		for _, src := range doctorPanelSources {
			name := config.SelectPanelName("", src.source, merged)
			if name == "" {
				continue
			}
			p := planDoctorPanel(repo, name, arm.RepoConfig, cfg)
			if arm.Arm == config.ExperimentArmExperimental {
				p.Experiment = arm.ExperimentID
			}
			panels = mergeDoctorPanel(panels, p, src.use)
		}
	}
	return panels, nil
}

// planDoctorPanel plans one panel with planPanelRun and resolves its
// synthesis agent with the worker's resolution.
func planDoctorPanel(repo, name string, repoCfg *config.RepoConfig, cfg *config.Config) DoctorPanel {
	p := DoctorPanel{Name: name}
	plan, err := planPanelRun(targetDescriptor{}, name, uuid.UUID{}, repoCfg, cfg)
	if err != nil {
		p.Error = err.Error()
		return p
	}
	for _, o := range plan.memberOpts {
		p.Members = append(p.Members, DoctorPanelMember{Name: o.PanelMemberName, Agent: o.Agent})
	}
	job := &storage.ReviewJob{RepoPath: repo, Agent: plan.synthOpts.Agent, BackupAgent: plan.synthOpts.BackupAgent}
	p.Synthesis = agent.Diagnosis{Name: plan.synthOpts.Agent}
	if a, err := resolveConfiguredJobAgent(job, cfg, job.BackupAgent); err != nil {
		p.Synthesis.Error = err.Error()
	} else {
		p.Synthesis.Available = true
		if ca, ok := a.(agent.CommandAgent); ok {
			p.Synthesis.Command = ca.CommandName()
		}
	}
	return p
}

// mergeDoctorPanel adds p for the given kind of review, merging it into an
// existing entry with the same outcome. The default arm is planned first, so
// an experimental arm that changes nothing merges into it.
func mergeDoctorPanel(panels []DoctorPanel, p DoctorPanel, use string) []DoctorPanel {
	for i := range panels {
		if sameDoctorPanelOutcome(panels[i], p) {
			if !slices.Contains(panels[i].UsedFor, use) {
				panels[i].UsedFor = append(panels[i].UsedFor, use)
			}
			return panels
		}
	}
	p.UsedFor = []string{use}
	return append(panels, p)
}

func sameDoctorPanelOutcome(a, b DoctorPanel) bool {
	return a.Name == b.Name && a.Error == b.Error &&
		slices.Equal(a.Members, b.Members) && a.Synthesis == b.Synthesis
}
