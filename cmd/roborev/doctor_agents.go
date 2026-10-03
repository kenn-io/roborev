package main

import (
	"fmt"
	"strings"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/daemon"
)

// agentView is one agent's availability from where reviews actually run: the
// daemon when it is reachable, otherwise this shell.
type agentView struct {
	diag   agent.Diagnosis
	daemon bool
}

func (env *doctorEnv) viewAgent(name string) agentView {
	name = strings.TrimSpace(name)
	if env.daemonAgents != nil {
		for _, d := range env.daemonAgents.Requested {
			if d.Name == name {
				return agentView{diag: d, daemon: true}
			}
		}
		// Every name the checks look up was sent to the daemon. A daemon
		// that answered without it predates per-name resolution.
		return agentView{daemon: true, diag: agent.Diagnosis{
			Name:  name,
			Error: "the daemon did not resolve this agent; restart it so it runs this version of roborev",
		}}
	}
	return agentView{diag: agent.Diagnose(env.repoCfg, name, env.global)}
}

func doctorAgentKey(name string) string {
	return agent.CanonicalName(strings.TrimSpace(name))
}

func (v agentView) where() string {
	if v.daemon {
		return "the daemon"
	}
	return "this shell"
}

func checkDoctorAgents(env *doctorEnv) []doctorCheck {
	var out []doctorCheck
	local := agent.DiagnoseAll(env.repoCfg, env.global)

	if env.daemonUp() && env.daemonAgentsErr != nil {
		out = append(out, doctorCheck{
			ID: "agents.daemon_view", Category: "agents", Status: doctorWarn,
			Summary: "could not ask the daemon which agents it can run; showing this shell's view",
			Details: []string{env.daemonAgentsErr.Error()},
			Fix:     "if the daemon is older than this CLI, run 'roborev daemon restart'",
		})
	}

	if env.daemonAgents != nil && env.daemonAgents.RepoConfigError != "" {
		out = append(out, doctorCheck{
			ID: "agents.daemon_repo_config", Category: "agents", Status: doctorWarn,
			Summary: "the daemon could not load this repository's .roborev.toml; agent results use global config only",
			Details: []string{env.daemonAgents.RepoConfigError},
			Fix:     "fix .roborev.toml; if 'roborev config validate' passes, run 'roborev daemon restart'",
		})
	}

	// Which agents are installed where reviews run.
	var installed []agent.Diagnosis
	source := local
	where := "this shell"
	if env.daemonAgents != nil {
		source = env.daemonAgents.Agents
		where = "the daemon"
	}
	for _, d := range source {
		if d.Available {
			installed = append(installed, d)
		}
	}
	if len(installed) == 0 {
		out = append(out, doctorCheck{
			ID: "agents.installed", Category: "agents", Status: doctorFail,
			Summary: fmt.Sprintf("no review agents are available to %s", where),
			Fix:     "install an agent CLI such as codex or claude, then run 'roborev daemon restart' from a shell where it is on PATH",
		})
	} else {
		names := make([]string, 0, len(installed))
		for _, d := range installed {
			names = append(names, d.Name)
		}
		out = append(out, doctorCheck{
			ID: "agents.installed", Category: "agents", Status: doctorOK,
			Summary: fmt.Sprintf("agents available to %s: %s", where, strings.Join(names, ", ")),
		})
	}

	out = append(out, checkDoctorPathMismatch(env, local)...)
	review, reported := checkDoctorReviewAgents(env)
	out = append(out, review...)
	out = append(out, checkDoctorConfiguredAgents(env, reported)...)
	return out
}

// checkDoctorPathMismatch finds agents this shell can run but the daemon
// cannot, which almost always means the daemon started with a different PATH
// (a login item, service manager, or older shell).
func checkDoctorPathMismatch(env *doctorEnv, local []agent.Diagnosis) []doctorCheck {
	if env.daemonAgents == nil {
		return nil
	}
	daemonHas := map[string]agent.Diagnosis{}
	for _, d := range env.daemonAgents.Agents {
		daemonHas[doctorAgentKey(d.Name)] = d
	}
	var missing []string
	for _, d := range local {
		if !d.Available {
			continue
		}
		if dd, ok := daemonHas[doctorAgentKey(d.Name)]; ok && !dd.Available {
			missing = append(missing, fmt.Sprintf("%s: found at %s in this shell, but not on the daemon's PATH", d.Name, d.Path))
		}
	}
	if len(missing) == 0 {
		return []doctorCheck{{
			ID: "agents.daemon_path", Category: "agents", Status: doctorOK,
			Summary: "the daemon can run every agent this shell can",
		}}
	}
	details := append(missing, "daemon PATH: "+env.daemonAgents.PathEnv)
	return []doctorCheck{{
		ID: "agents.daemon_path", Category: "agents", Status: doctorWarn,
		Summary: fmt.Sprintf("%s installed here cannot be run by the daemon", plural(len(missing), "agent")),
		Details: details,
		Fix:     "run 'roborev daemon restart' from this shell, or set the agent's *_cmd config key to an absolute path",
	}}
}

// checkDoctorReviewAgents checks what runs post-commit and manual reviews. The
// daemon plans each selected panel with the same code that queues a review;
// reviews whose default config selects no panel use the single review agent.
func checkDoctorReviewAgents(env *doctorEnv) ([]doctorCheck, map[string]bool) {
	panels, panelsErr := env.reviewPanels()
	var out []doctorCheck
	if panelsErr != "" {
		out = append(out, doctorCheck{
			ID: "agents.review_panel", Category: "agents", Status: doctorWarn,
			Summary: "review panels were not checked: the review experiment configuration does not apply",
			Details: []string{panelsErr},
			Fix:     "run 'roborev config validate'",
		})
	}
	// An experimental arm covers only some branches; the rest still run the
	// default config, so only default-arm panels replace the single agent.
	covered := map[string]bool{}
	for _, p := range panels {
		if p.Experiment == "" {
			for _, use := range p.UsedFor {
				covered[use] = true
			}
		}
		out = append(out, checkDoctorPanel(env, p))
	}

	subject := "reviews"
	switch {
	case covered["post_commit"] && covered["manual"]:
		return out, nil
	case covered["post_commit"]:
		subject = "manual reviews"
	case covered["manual"]:
		subject = "post-commit reviews"
	}
	single, reported := checkDoctorReviewAgent(env, subject)
	return append([]doctorCheck{single}, out...), reported
}

// reviewPanels returns the daemon's panel plans, or plans them in this
// process when the daemon cannot be asked.
func (env *doctorEnv) reviewPanels() ([]daemon.DoctorPanel, string) {
	if env.daemonAgents != nil {
		return env.daemonAgents.Panels, env.daemonAgents.PanelsError
	}
	panels, err := daemon.ResolveDoctorPanels(env.repoPath, env.repoCfg, env.repoRaw, env.global)
	if err != nil {
		return nil, err.Error()
	}
	return panels, ""
}

var doctorPanelUses = map[string]string{"post_commit": "post-commit reviews", "manual": "manual reviews"}

// checkDoctorPanel reports whether a planned panel can be queued and finished.
// Planning fails exactly when queueing the panel would be rejected, and the
// synthesis agent runs strictly: its configured agent or backup. Only the
// panel as currently configured is checked; panels change over time, so
// doctor never compares them with the settings frozen on earlier runs.
func checkDoctorPanel(env *doctorEnv, p daemon.DoctorPanel) doctorCheck {
	var uses []string
	for _, u := range p.UsedFor {
		uses = append(uses, doctorPanelUses[u])
	}
	label := fmt.Sprintf("panel %q (%s)", p.Name, strings.Join(uses, " and "))
	if p.Experiment != "" {
		label = fmt.Sprintf("panel %q (%s, experimental arm of %s)", p.Name, strings.Join(uses, " and "), p.Experiment)
	}
	c := doctorCheck{ID: "agents.review_panel", Category: "agents"}
	restart := ""
	if env.daemonAgents != nil {
		restart = "; if the agent works in this shell, run 'roborev daemon restart' from it"
	}
	switch {
	case p.Error != "":
		c.Status = doctorFail
		c.Summary = label + " cannot be queued; those reviews will fail"
		c.Details = []string{p.Error}
		c.Fix = "fix the panel under [review.panels] and [review.subagents], or install the missing agent" + restart
	case !p.Synthesis.Available:
		c.Status = doctorFail
		c.Summary = fmt.Sprintf("%s will fail: synthesis agent %s is not available", label, p.Synthesis.Name)
		c.Details = []string{doctorFirstLine(p.Synthesis.Error)}
		c.Fix = "install the synthesis agent or set synthesis_agent or synthesis_backup_agent on the panel" + restart
	default:
		var members []string
		for _, m := range p.Members {
			members = append(members, fmt.Sprintf("%s uses %s", m.Name, m.Agent))
		}
		c.Status = doctorOK
		c.Summary = fmt.Sprintf("%s: %s; synthesis uses %s", label, strings.Join(members, ", "), p.Synthesis.Name)
	}
	return c
}

// checkDoctorReviewAgent checks the agent that ordinary reviews resolve to;
// subject names the reviews it covers, such as "manual reviews".
// Review resolution is strict: it tries the preferred agent and configured
// backups, then fails the job, so an unavailable agent fails every review.
// It also returns the unavailable agent names it reported, so the configured
// agents check does not repeat them.
func checkDoctorReviewAgent(env *doctorEnv, subject string) (doctorCheck, map[string]bool) {
	c := doctorCheck{ID: "agents.review", Category: "agents"}
	reported := map[string]bool{}
	res, err := env.reviewWorkflow()
	if err != nil {
		c.Status = doctorFail
		c.Summary = subject + " have no resolvable agent"
		c.Details = []string{err.Error()}
		c.Fix = "check the agent settings in your config"
		return c, reported
	}
	primary := env.viewAgent(res.PreferredAgent)
	if primary.diag.Available {
		c.Status = doctorOK
		c.Summary = fmt.Sprintf("%s use %s (%s)", subject, primary.diag.Name, primary.diag.Path)
		return c, reported
	}
	reported[doctorAgentKey(res.PreferredAgent)] = true
	details := []string{fmt.Sprintf("%s is not available to %s: %s", res.PreferredAgent, primary.where(), doctorFirstLine(primary.diag.Error))}
	if primary.diag.Unknown {
		// Resolution rejects an unknown preferred name before it tries
		// backups, so a working backup does not help.
		c.Status = doctorFail
		c.Summary = fmt.Sprintf("%s will fail: agent %s is not a known agent", subject, res.PreferredAgent)
		c.Details = details
		if res.BackupAgent != "" {
			c.Details = append(c.Details, fmt.Sprintf("backup %s is never tried, because the preferred name is rejected first", res.BackupAgent))
		}
		c.Fix = fmt.Sprintf("fix the agent name, or add an [acp.<name>] table if %s is meant to be a named ACP agent", res.PreferredAgent)
		return c, reported
	}
	if res.BackupAgent != "" {
		backup := env.viewAgent(res.BackupAgent)
		if backup.diag.Available {
			c.Status = doctorWarn
			c.Summary = fmt.Sprintf("%s fall back to backup agent %s because %s is unavailable", subject, res.BackupAgent, res.PreferredAgent)
			c.Details = details
			c.Fix = doctorAgentFix(env, res.PreferredAgent)
			return c, reported
		}
		reported[doctorAgentKey(res.BackupAgent)] = true
		details = append(details, fmt.Sprintf("backup %s is not available either: %s", res.BackupAgent, doctorFirstLine(backup.diag.Error)))
	}
	c.Status = doctorFail
	c.Summary = fmt.Sprintf("%s will fail: agent %s is not available", subject, res.PreferredAgent)
	c.Details = details
	c.Fix = doctorAgentFix(env, res.PreferredAgent)
	return c, reported
}

func doctorAgentFix(env *doctorEnv, name string) string {
	local := agent.Diagnose(env.repoCfg, name, env.global)
	if local.Available && env.daemonAgents != nil {
		return fmt.Sprintf("%s is installed in this shell (%s); run 'roborev daemon restart' from this shell so the daemon inherits its PATH", name, local.Path)
	}
	return fmt.Sprintf("install %s or set 'agent' in your config to an installed agent", name)
}

// checkDoctorConfiguredAgents checks every agent name set anywhere in config
// (workflow agents, backups, CI, panels). Unavailable names only break the
// workflow that uses them, so they warn rather than fail. Names in skip were
// already reported by the review agent check.
func checkDoctorConfiguredAgents(env *doctorEnv, skip map[string]bool) []doctorCheck {
	keysByName := map[string][]string{}
	var order []string
	add := func(scope string, refs []config.AgentReference) {
		for _, ref := range refs {
			if ref.Name == "test" {
				continue
			}
			if _, seen := keysByName[ref.Name]; !seen {
				order = append(order, ref.Name)
			}
			keysByName[ref.Name] = append(keysByName[ref.Name], scope+ref.Key)
		}
	}
	add("", config.AgentReferences(env.global))
	if env.repoCfg != nil {
		add(".roborev.toml: ", config.AgentReferences(env.repoCfg))
	}
	// An experiment's effective config repeats the repository's own agent
	// names; only names the experiment adds are new.
	var added []config.AgentReference
	for _, ref := range env.experimentAgentReferences() {
		if _, seen := keysByName[ref.Name]; !seen {
			added = append(added, ref)
		}
	}
	add("", added)

	var problems []string
	restartHelps := false
	checked := 0
	for _, name := range order {
		if skip[doctorAgentKey(name)] {
			continue
		}
		checked++
		v := env.viewAgent(name)
		if v.diag.Available {
			continue
		}
		if v.daemon && agent.Diagnose(env.repoCfg, name, env.global).Available {
			restartHelps = true
		}
		problems = append(problems, fmt.Sprintf("%s (set by %s): %s",
			name, strings.Join(keysByName[name], ", "), doctorFirstLine(v.diag.Error)))
	}
	if len(problems) == 0 {
		if checked == 0 {
			return nil
		}
		return []doctorCheck{{
			ID: "agents.configured", Category: "agents", Status: doctorOK,
			Summary: "every agent named in config is available",
		}}
	}
	fix := "install the agent, fix the name, or remove the setting; jobs routed to it fail"
	if restartHelps {
		fix = "some of these are installed in this shell; run 'roborev daemon restart' from it. Otherwise " + fix
	}
	return []doctorCheck{{
		ID: "agents.configured", Category: "agents", Status: doctorWarn,
		Summary: fmt.Sprintf("%s named in config %s not available", plural(len(problems), "agent"),
			map[bool]string{true: "is", false: "are"}[len(problems) == 1]),
		Details: problems,
		Fix:     fix,
	}}
}
