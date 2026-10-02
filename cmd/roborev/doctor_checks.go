package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	gitrepo "go.kenn.io/kit/git/repo"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/git"
	"go.kenn.io/roborev/internal/githook"
	"go.kenn.io/roborev/internal/prompt"
	"go.kenn.io/roborev/internal/version"
)

// doctorRecentWindow is how far back the failure checks look.
const doctorRecentWindow = 7 * 24 * time.Hour

// doctorRepeatedFailures is the count at which the same failure in the recent
// window becomes a warning instead of a note.
const doctorRepeatedFailures = 3

// doctorEnv is everything the checks read, loaded once up front so each
// check is a pure function of it.
type doctorEnv struct {
	ctx context.Context
	now time.Time

	globalPath string
	global     *config.Config // defaults when the file fails to load
	globalErr  error

	repoPath   string // "" outside a git repository
	repoCfg    *config.RepoConfig
	repoRaw    map[string]any
	repoErr    error
	repoBranch string // default branch; "" when it cannot be resolved

	daemon          doctorDaemon
	ping            *daemon.PingInfo
	pingErr         error
	daemonAgents    *doctorDaemonAgents
	daemonAgentsErr error

	postCommitLog string
}

func loadDoctorEnv(ctx context.Context, repoPath string, d doctorDaemon) *doctorEnv {
	env := &doctorEnv{
		ctx:           ctx,
		now:           time.Now(),
		globalPath:    config.GlobalConfigPath(),
		repoPath:      repoPath,
		daemon:        d,
		postCommitLog: postCommitLogPath(),
	}
	env.global, env.globalErr = config.LoadGlobalFrom(env.globalPath)
	if env.globalErr != nil {
		env.global = config.DefaultConfig()
	}
	if repoPath != "" {
		env.global = env.global.ForRepo(repoPath)
		env.repoCfg, env.repoRaw, env.repoErr = config.LoadRepoConfigWithRaw(repoPath)
		if branch, err := gitrepo.DefaultBranch(ctx, repoPath); err == nil {
			env.repoBranch = branch
		}
	}
	env.ping, env.pingErr = d.Ping()
	if env.pingErr == nil {
		env.daemonAgents, env.daemonAgentsErr = d.Agents(ctx, repoPath, doctorAgentNames(env))
	}
	return env
}

// doctorAgentNames lists the agent names the doctor asks the daemon to
// resolve: the review agent and its backup, then every agent named in config.
func doctorAgentNames(env *doctorEnv) []string {
	var names []string
	seen := map[string]bool{}
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name != "" && name != "test" && !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	if res, err := env.reviewWorkflow(); err == nil {
		add(res.PreferredAgent)
		add(res.BackupAgent)
	}
	for _, ref := range config.AgentReferences(env.global) {
		add(ref.Name)
	}
	if env.repoCfg != nil {
		for _, ref := range config.AgentReferences(env.repoCfg) {
			add(ref.Name)
		}
	}
	return names
}

// reviewWorkflow resolves the agent and backup that ordinary reviews use.
func (env *doctorEnv) reviewWorkflow() (agent.WorkflowConfig, error) {
	reasoning, err := config.ResolveReviewReasoningFromConfig("", env.repoCfg, env.global)
	if err != nil {
		reasoning = ""
	}
	return agent.ResolveWorkflowConfigFromConfig("", env.repoCfg, env.global, "review", reasoning)
}

func (env *doctorEnv) daemonUp() bool { return env.pingErr == nil }

type doctorCheckFunc func(*doctorEnv) []doctorCheck

func runDoctorChecks(env *doctorEnv) []doctorCheck {
	checks := []doctorCheckFunc{
		checkDoctorGlobalConfig,
		checkDoctorRepoConfig,
		checkDoctorDaemon,
		checkDoctorAgents,
		checkDoctorFailedJobs,
		checkDoctorEnqueueFailures,
		checkDoctorRepo,
		checkDoctorGuidelines,
		checkDoctorIntegrations,
	}
	var out []doctorCheck
	for _, check := range checks {
		out = append(out, runDoctorCheck(env, check)...)
	}
	return out
}

// runDoctorCheck turns a panicking check into a failed result so one broken
// check cannot hide the rest of the report.
func runDoctorCheck(env *doctorEnv, check doctorCheckFunc) (out []doctorCheck) {
	defer func() {
		if r := recover(); r != nil {
			out = []doctorCheck{{
				ID: "doctor.internal", Category: "config", Status: doctorFail,
				Summary: fmt.Sprintf("a doctor check crashed: %v", r),
				Fix:     "report this as a roborev bug",
			}}
		}
	}()
	return check(env)
}

// doctorFirstLine returns the first non-empty line of s, for compact messages.
func doctorFirstLine(s string) string {
	for line := range strings.SplitSeq(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}

// --- Configuration ---

func checkDoctorGlobalConfig(env *doctorEnv) []doctorCheck {
	c := doctorCheck{ID: "config.global", Category: "config"}
	_, statErr := os.Stat(env.globalPath)
	switch {
	case env.globalErr != nil:
		c.Status = doctorFail
		c.Summary = "global config does not load; roborev falls back to defaults or refuses to run"
		c.Details = []string{env.globalPath, env.globalErr.Error()}
		c.Fix = "fix the error, then run 'roborev config validate --global'"
		return []doctorCheck{c}
	case errors.Is(statErr, os.ErrNotExist):
		c.Status = doctorOK
		c.Summary = "no global config file; using defaults"
	default:
		err := env.global.Validate()
		if err == nil {
			err = config.ValidateExperimentConfigs(env.global, nil, nil)
		}
		if err != nil {
			c.Status = doctorFail
			c.Summary = "global config has invalid values"
			c.Details = []string{env.globalPath, err.Error()}
			c.Fix = "fix the value, then run 'roborev config validate --global'"
		} else {
			c.Status = doctorOK
			c.Summary = "global config is valid (" + env.globalPath + ")"
		}
	}
	out := []doctorCheck{c}
	if unknown, err := config.UnknownGlobalKeys(env.globalPath); err == nil && len(unknown) > 0 {
		out = append(out, unknownKeysCheck("config.global_unknown_keys", env.globalPath, unknown))
	}
	return out
}

func unknownKeysCheck(id, path string, keys []string) doctorCheck {
	return doctorCheck{
		ID: id, Category: "config", Status: doctorWarn,
		Summary: fmt.Sprintf("%s in %s %s ignored", plural(len(keys), "unknown key"), path,
			map[bool]string{true: "is", false: "are"}[len(keys) == 1]),
		Details: keys,
		Fix:     "check the spelling against the configuration docs; roborev silently ignores keys it does not know",
	}
}

func checkDoctorRepoConfig(env *doctorEnv) []doctorCheck {
	if env.repoPath == "" {
		return []doctorCheck{{
			ID: "config.repo", Category: "config", Status: doctorInfo,
			Summary: "not in a git repository; repository checks skipped",
			Fix:     "run 'roborev doctor' inside a repository, or pass --repo",
		}}
	}
	path := config.RepoConfigPath(env.repoPath)
	c := doctorCheck{ID: "config.repo", Category: "config"}
	switch {
	case env.repoErr != nil:
		c.Status = doctorFail
		c.Summary = "repository .roborev.toml does not load"
		c.Details = []string{path, env.repoErr.Error()}
		c.Fix = "fix the error, then run 'roborev config validate --local'"
		return []doctorCheck{c}
	case env.repoCfg == nil:
		c.Status = doctorOK
		c.Summary = "no .roborev.toml; this repository uses global settings"
	default:
		err := config.ValidateEffectiveReviewConfig(env.global, env.repoCfg)
		if err == nil {
			err = config.ValidateExperimentConfigs(env.global, env.repoCfg, env.repoRaw)
		}
		if err != nil {
			c.Status = doctorFail
			c.Summary = "repository config is invalid when merged with global config"
			c.Details = []string{path, err.Error()}
			c.Fix = "fix the value, then run 'roborev config validate'"
		} else {
			c.Status = doctorOK
			c.Summary = "repository config is valid (" + path + ")"
		}
	}
	out := []doctorCheck{c}
	if env.repoCfg != nil {
		if unknown, err := config.UnknownRepoKeys(env.repoPath); err == nil && len(unknown) > 0 {
			out = append(out, unknownKeysCheck("config.repo_unknown_keys", path, unknown))
		}
	}
	if check, ok := checkDoctorDefaultBranchConfig(env); ok {
		out = append(out, check)
	}
	return out
}

// checkDoctorDefaultBranchConfig checks .roborev.toml on the default branch,
// which is where reviews read repository config and guidelines from.
func checkDoctorDefaultBranchConfig(env *doctorEnv) (doctorCheck, bool) {
	if env.repoBranch == "" {
		return doctorCheck{}, false
	}
	_, err := config.LoadRepoConfigFromRef(env.repoPath, env.repoBranch)
	if err == nil {
		return doctorCheck{}, false
	}
	c := doctorCheck{ID: "config.repo_default_branch", Category: "config", Details: []string{err.Error()}}
	if config.IsConfigParseError(err) || config.IsExperimentConfigError(err) {
		c.Status = doctorFail
		c.Summary = fmt.Sprintf(".roborev.toml on %s is invalid", env.repoBranch)
		c.Details = append(c.Details, "reviews read repository config from this branch")
		c.Fix = fmt.Sprintf("fix .roborev.toml and merge the fix into %s", env.repoBranch)
		return c, true
	}
	c.Status = doctorWarn
	c.Summary = fmt.Sprintf("could not read .roborev.toml from %s", env.repoBranch)
	c.Fix = "check that the repository is readable with 'git show " + env.repoBranch + ":.roborev.toml'"
	return c, true
}

// --- Daemon ---

func checkDoctorDaemon(env *doctorEnv) []doctorCheck {
	if !env.daemonUp() {
		c := doctorCheck{
			ID: "daemon.running", Category: "daemon", Status: doctorWarn,
			Summary: "daemon is not running; commits are not reviewed until it starts",
			Details: []string{env.pingErr.Error(), "checks that need the daemon were skipped"},
			Fix:     "roborev daemon start",
		}
		if daemon.IsDaemonAccessDenied(env.pingErr) {
			c.Summary = "cannot reach the daemon: access denied"
			c.Fix = "if roborev runs in a sandbox, allow loopback or Unix socket access and retry"
		}
		return []doctorCheck{c}
	}

	out := []doctorCheck{{
		ID: "daemon.running", Category: "daemon", Status: doctorOK,
		Summary: fmt.Sprintf("daemon is running (pid %d)", env.ping.PID),
	}}
	if env.ping.Version != version.Version {
		out = append(out, doctorCheck{
			ID: "daemon.version", Category: "daemon", Status: doctorWarn,
			Summary: fmt.Sprintf("daemon runs %s but this CLI is %s", env.ping.Version, version.Version),
			Fix:     "roborev daemon restart",
		})
	}

	if status, err := env.daemon.Status(env.ctx); err != nil {
		out = append(out, doctorCheck{
			ID: "daemon.status", Category: "daemon", Status: doctorWarn,
			Summary: "daemon status is unavailable", Details: []string{err.Error()},
		})
	} else if status.QueuePaused {
		out = append(out, doctorCheck{
			ID: "daemon.queue_paused", Category: "daemon", Status: doctorWarn,
			Summary: fmt.Sprintf("review queue is paused; %d job(s) waiting", status.QueuedJobs),
			Fix:     "roborev unpause",
		})
	}

	health, err := env.daemon.Health(env.ctx)
	if err != nil {
		out = append(out, doctorCheck{
			ID: "daemon.health", Category: "daemon", Status: doctorWarn,
			Summary: "daemon health is unavailable", Details: []string{err.Error()},
		})
		return out
	}
	var unhealthy []string
	for _, comp := range health.Components {
		if !comp.Healthy {
			unhealthy = append(unhealthy, fmt.Sprintf("%s: %s", comp.Name, deref(comp.Message)))
		}
	}
	if len(unhealthy) > 0 {
		out = append(out, doctorCheck{
			ID: "daemon.health", Category: "daemon", Status: doctorWarn,
			Summary: "daemon reports unhealthy components", Details: unhealthy,
			Fix: "run 'roborev status' for detail; 'roborev daemon restart' clears stuck workers",
		})
	}
	if health.ErrorCount24H > 0 {
		c := doctorCheck{
			ID: "daemon.errors", Category: "daemon", Status: doctorInfo,
			Summary: fmt.Sprintf("daemon logged %s in the last 24 hours", plural(int(health.ErrorCount24H), "error")),
			Fix:     "run 'roborev log' or read errors.log in the roborev data directory",
		}
		for i, e := range health.RecentErrors {
			if i == 3 {
				break
			}
			c.Details = append(c.Details, fmt.Sprintf("%s: %s", e.Component, doctorFirstLine(e.Message)))
		}
		out = append(out, c)
	}
	return out
}

// --- Agents ---

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

// checkDoctorReviewAgents checks what runs post-commit and manual reviews: a
// selected review panel, or the single review agent when no panel is
// selected for that kind of review.
func checkDoctorReviewAgents(env *doctorEnv) ([]doctorCheck, map[string]bool) {
	var panels []daemon.DoctorPanel
	if env.daemonAgents != nil {
		panels = env.daemonAgents.Panels
	} else {
		panels = daemon.ResolveDoctorPanels(env.repoPath, env.repoCfg, env.global)
	}
	covered := map[string]bool{}
	var out []doctorCheck
	reported := map[string]bool{}
	for _, p := range panels {
		for _, use := range p.UsedFor {
			covered[use] = true
		}
		check, names := checkDoctorPanel(env, p)
		out = append(out, check)
		for name := range names {
			reported[name] = true
		}
	}
	if covered["post_commit"] && covered["manual"] {
		return out, reported
	}
	single, names := checkDoctorReviewAgent(env)
	// The single-agent summaries start with "reviews"; name which reviews
	// they cover when a panel handles the other kind.
	switch {
	case covered["post_commit"]:
		single.Summary = "manual " + single.Summary
	case covered["manual"]:
		single.Summary = "post-commit " + single.Summary
	}
	for name := range names {
		reported[name] = true
	}
	return append([]doctorCheck{single}, out...), reported
}

var doctorPanelUses = map[string]string{"post_commit": "post-commit reviews", "manual": "manual reviews"}

// checkDoctorPanel reports whether a selected panel can be queued and
// finished. The daemon selects every member's agent when it queues the
// panel and rejects the whole review if any member has none, even a member
// marked allow_failure; allow_failure only covers a member that fails while
// running. The synthesis agent runs strictly: its configured agent or backup.
//
// Only the panel as currently configured is checked. Panels change over
// time, so doctor never compares them with the member settings frozen on
// earlier panel runs.
func checkDoctorPanel(env *doctorEnv, p daemon.DoctorPanel) (doctorCheck, map[string]bool) {
	var uses []string
	for _, u := range p.UsedFor {
		uses = append(uses, doctorPanelUses[u])
	}
	label := fmt.Sprintf("panel %q (%s)", p.Name, strings.Join(uses, " and "))
	c := doctorCheck{ID: "agents.review_panel", Category: "agents"}
	reported := map[string]bool{}
	merged := config.MergeReviewConfigFromConfig(env.repoCfg, env.global)

	if p.Error != "" {
		c.Status = doctorFail
		c.Summary = label + " cannot be resolved; those reviews will fail"
		c.Details = []string{p.Error}
		c.Fix = "fix the panel definition under [review.panels] and [review.subagents]"
		return c, reported
	}
	var problems, agents []string
	for _, m := range p.Members {
		if m.Error != "" {
			problems = append(problems, fmt.Sprintf("member %s: %s", m.Name, doctorFirstLine(m.Error)))
			if name := merged.Subagents[m.Name].Agent; name != "" {
				reported[doctorAgentKey(name)] = true
			}
			continue
		}
		agents = append(agents, fmt.Sprintf("%s uses %s", m.Name, m.Agent))
	}
	if !p.Synthesis.Available {
		problems = append(problems, fmt.Sprintf("synthesis agent %s: %s", p.Synthesis.Name, doctorFirstLine(p.Synthesis.Error)))
		reported[doctorAgentKey(p.Synthesis.Name)] = true
		if spec, ok := merged.Panels[p.Name]; ok && spec.SynthesisBackupAgent != "" {
			reported[doctorAgentKey(spec.SynthesisBackupAgent)] = true
		}
	}
	if len(problems) == 0 {
		c.Status = doctorOK
		c.Summary = fmt.Sprintf("%s: %s; synthesis uses %s", label, strings.Join(agents, ", "), p.Synthesis.Name)
		return c, reported
	}
	c.Status = doctorFail
	c.Summary = label + " will fail: some of its agents are not available"
	c.Details = append(problems,
		"the daemon rejects the whole panel review when any member has no agent, even a member marked allow_failure")
	c.Fix = "install the agent, give the member a backup_agent, or remove the member from the panel"
	if env.daemonAgents != nil {
		c.Fix += "; if the agent works in this shell, run 'roborev daemon restart' from it"
	}
	return c, reported
}

// checkDoctorReviewAgent checks the agent that ordinary reviews resolve to.
// Review resolution is strict: it tries the preferred agent and configured
// backups, then fails the job, so an unavailable agent fails every review.
// It also returns the unavailable agent names it reported, so the configured
// agents check does not repeat them.
func checkDoctorReviewAgent(env *doctorEnv) (doctorCheck, map[string]bool) {
	c := doctorCheck{ID: "agents.review", Category: "agents"}
	reported := map[string]bool{}
	res, err := env.reviewWorkflow()
	if err != nil {
		c.Status = doctorFail
		c.Summary = "reviews have no resolvable agent"
		c.Details = []string{err.Error()}
		c.Fix = "check the agent settings in your config"
		return c, reported
	}
	primary := env.viewAgent(res.PreferredAgent)
	if primary.diag.Available {
		c.Status = doctorOK
		c.Summary = fmt.Sprintf("reviews use %s (%s)", primary.diag.Name, primary.diag.Path)
		return c, reported
	}
	reported[doctorAgentKey(res.PreferredAgent)] = true
	details := []string{fmt.Sprintf("%s is not available to %s: %s", res.PreferredAgent, primary.where(), doctorFirstLine(primary.diag.Error))}
	if primary.diag.Unknown {
		// Resolution rejects an unknown preferred name before it tries
		// backups, so a working backup does not help.
		c.Status = doctorFail
		c.Summary = fmt.Sprintf("reviews will fail: agent %s is not a known agent", res.PreferredAgent)
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
			c.Summary = fmt.Sprintf("reviews fall back to backup agent %s because %s is unavailable", res.BackupAgent, res.PreferredAgent)
			c.Details = details
			c.Fix = doctorAgentFix(env, res.PreferredAgent)
			return c, reported
		}
		reported[doctorAgentKey(res.BackupAgent)] = true
		details = append(details, fmt.Sprintf("backup %s is not available either: %s", res.BackupAgent, doctorFirstLine(backup.diag.Error)))
	}
	c.Status = doctorFail
	c.Summary = fmt.Sprintf("reviews will fail: agent %s is not available", res.PreferredAgent)
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

// --- Recent failures ---

func checkDoctorFailedJobs(env *doctorEnv) []doctorCheck {
	if !env.daemonUp() {
		return nil
	}
	cutoff := env.now.Add(-doctorRecentWindow)
	jobs, err := env.daemon.FailedJobs(env.ctx, cutoff)
	if err != nil {
		return []doctorCheck{{
			ID: "jobs.failed", Category: "jobs", Status: doctorWarn,
			Summary: "could not list failed jobs", Details: []string{err.Error()},
		}}
	}
	type group struct {
		count  int
		errors map[string]int
	}
	groups := map[string]*group{}
	total := 0
	for _, j := range jobs {
		if j.EnqueuedAt.Before(cutoff) {
			continue
		}
		total++
		name := j.Agent
		if name == "" {
			name = "(unknown agent)"
		}
		g := groups[name]
		if g == nil {
			g = &group{errors: map[string]int{}}
			groups[name] = g
		}
		g.count++
		g.errors[truncateString(doctorFirstLine(deref(j.ErrorData)), 200)]++
	}
	if total == 0 {
		return []doctorCheck{{
			ID: "jobs.failed", Category: "jobs", Status: doctorOK,
			Summary: "no failed jobs queued in the last 7 days",
		}}
	}
	names := make([]string, 0, len(groups))
	for name := range groups {
		names = append(names, name)
	}
	sort.Slice(names, func(i, k int) bool {
		if groups[names[i]].count != groups[names[k]].count {
			return groups[names[i]].count > groups[names[k]].count
		}
		return names[i] < names[k]
	})
	status := doctorInfo
	var details []string
	for _, name := range names {
		g := groups[name]
		if g.count >= doctorRepeatedFailures {
			status = doctorWarn
		}
		msg, n := mostCommon(g.errors)
		details = append(details, fmt.Sprintf("%s: %s; most common (%dx): %s", name, plural(g.count, "failure"), n, msg))
	}
	return []doctorCheck{{
		ID: "jobs.failed", Category: "jobs", Status: status,
		Summary: fmt.Sprintf("%s queued in the last 7 days", plural(total, "failed job")),
		Details: details,
		Fix:     "inspect one with 'roborev show <job-id>'; repeated agent errors usually mean a missing agent, expired auth, or quota limits",
	}}
}

func mostCommon(counts map[string]int) (string, int) {
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	best, bestN := "", 0
	for _, k := range keys {
		if counts[k] > bestN {
			best, bestN = k, counts[k]
		}
	}
	if best == "" {
		best = "(no error message)"
	}
	return best, bestN
}

type postCommitLogEntry struct {
	TS      string `json:"ts"`
	Repo    string `json:"repo"`
	Outcome string `json:"outcome"`
	Message string `json:"message"`
}

// checkDoctorEnqueueFailures reads the post-commit hook log. The hook never
// blocks a commit, so a hook that cannot queue reviews fails silently except
// for this log.
func checkDoctorEnqueueFailures(env *doctorEnv) []doctorCheck {
	f, err := os.Open(env.postCommitLog)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return []doctorCheck{{
			ID: "jobs.enqueue_failures", Category: "jobs", Status: doctorWarn,
			Summary: "cannot read the post-commit hook log", Details: []string{err.Error()},
		}}
	}
	defer f.Close()

	cutoff := env.now.Add(-doctorRecentWindow)
	counts := map[string]int{}
	total := 0
	lastForRepo := ""
	r := bufio.NewReader(f)
	for {
		line, readErr := r.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			var e postCommitLogEntry
			if json.Unmarshal(line, &e) == nil {
				ts, tsErr := time.Parse(time.RFC3339, e.TS)
				if tsErr == nil && !ts.Before(cutoff) {
					if env.repoPath != "" && e.Repo == env.repoPath {
						lastForRepo = e.Outcome
					}
					if e.Outcome == "fail" {
						total++
						counts[truncateString(doctorFirstLine(e.Message), 200)]++
					}
				}
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				return []doctorCheck{{
					ID: "jobs.enqueue_failures", Category: "jobs", Status: doctorWarn,
					Summary: "cannot read the post-commit hook log", Details: []string{readErr.Error()},
				}}
			}
			break
		}
	}

	if total == 0 {
		return []doctorCheck{{
			ID: "jobs.enqueue_failures", Category: "jobs", Status: doctorOK,
			Summary: "post-commit hook queued every review in the last 7 days",
		}}
	}
	status := doctorInfo
	if total >= doctorRepeatedFailures || lastForRepo == "fail" {
		status = doctorWarn
	}
	msg, n := mostCommon(counts)
	details := []string{fmt.Sprintf("most common (%dx): %s", n, msg)}
	if lastForRepo == "fail" {
		details = append(details, "the most recent commit in this repository was not queued")
	}
	return []doctorCheck{{
		ID: "jobs.enqueue_failures", Category: "jobs", Status: status,
		Summary: fmt.Sprintf("post-commit hook failed to queue %s in the last 7 days", plural(total, "review")),
		Details: details,
		Fix:     "see " + env.postCommitLog + "; failures usually mean the daemon was down or could not start",
	}}
}

// --- Repository ---

func checkDoctorRepo(env *doctorEnv) []doctorCheck {
	if env.repoPath == "" {
		return nil
	}
	var out []doctorCheck

	if env.daemonUp() {
		tracked, err := env.daemon.RepoTracked(env.ctx, env.repoPath)
		switch {
		case err != nil:
			out = append(out, doctorCheck{
				ID: "repo.registered", Category: "repo", Status: doctorWarn,
				Summary: "could not check whether the daemon knows this repository",
				Details: []string{err.Error()},
			})
		case tracked:
			out = append(out, doctorCheck{
				ID: "repo.registered", Category: "repo", Status: doctorOK,
				Summary: "repository is registered with the daemon",
			})
		default:
			out = append(out, doctorCheck{
				ID: "repo.registered", Category: "repo", Status: doctorInfo,
				Summary: "repository is not registered yet; it registers on its first review",
				Fix:     "roborev init",
			})
		}
	}

	if githook.NotInstalled(env.ctx, env.repoPath, "post-commit") {
		out = append(out, doctorCheck{
			ID: "repo.hooks", Category: "repo", Status: doctorWarn,
			Summary: "post-commit hook is not installed; commits are not reviewed automatically",
			Fix:     "roborev init",
		})
	} else {
		binary := ""
		if res, err := githook.ResolveRoborevPath(""); err == nil {
			binary = res.Path
		}
		var warnings []string
		for _, w := range daemon.ReadOnlyHookWarnings(env.ctx, env.repoPath, binary) {
			w = strings.TrimPrefix(w, "Warning: ")
			w = strings.Replace(w, " in "+env.repoPath, "", 1)
			w, _, _ = strings.Cut(w, " -- ")
			warnings = append(warnings, w)
		}
		if len(warnings) > 0 {
			out = append(out, doctorCheck{
				ID: "repo.hooks", Category: "repo", Status: doctorWarn,
				Summary: "git hooks need attention", Details: warnings,
				Fix: "roborev init",
			})
		} else {
			out = append(out, doctorCheck{
				ID: "repo.hooks", Category: "repo", Status: doctorOK,
				Summary: "git hooks are installed and current",
			})
		}
	}

	out = append(out, checkDoctorSnapshotDir(env)...)
	return out
}

// checkDoctorSnapshotDir checks the directory used to hand oversized diffs to
// sandboxed agents. Tracked files there block snapshot creation.
func checkDoctorSnapshotDir(env *doctorEnv) []doctorCheck {
	if env.repoErr != nil {
		return nil
	}
	dir, err := config.ResolveSnapshotDir(env.repoPath)
	if err != nil {
		return []doctorCheck{{
			ID: "repo.snapshot_dir", Category: "repo", Status: doctorFail,
			Summary: "snapshot_dir is invalid; reviews of large diffs will fail",
			Details: []string{err.Error()},
			Fix:     "set snapshot_dir in .roborev.toml to a relative path outside .git, or remove it",
		}}
	}
	err = git.ValidateRepoLocalPathNoSymlinks(env.repoPath, dir)
	if err == nil {
		err = git.EnsureNoTrackedFilesUnder(env.repoPath, dir)
	}
	if err != nil {
		return []doctorCheck{{
			ID: "repo.snapshot_dir", Category: "repo", Status: doctorFail,
			Summary: "reviews cannot write snapshots to snapshot_dir; reviews of large diffs will fail",
			Details: []string{err.Error()},
			Fix:     "point snapshot_dir at an unused directory inside the repository that is not a symlink and has no tracked files",
		}}
	}
	return nil
}

// --- Review guidelines ---

// securityGuidanceTerms are words whose presence suggests the guidelines tell
// a security reviewer what matters. Their absence is a hint, not a proof.
var securityGuidanceTerms = []string{"security", "threat", "trust boundar", "attack", "untrusted", "sensitive"}

func checkDoctorGuidelines(env *doctorEnv) []doctorCheck {
	if env.repoPath == "" {
		return nil
	}
	guidelines := strings.TrimSpace(prompt.LoadGuidelinesWithConfig(env.ctx, env.repoPath, env.global))
	security := securityReviewSources(env)

	branchNote := "the repository's default branch"
	if env.repoBranch != "" {
		branchNote = env.repoBranch
	}
	var hints []string
	if guidelines == "" {
		if _, err := os.Stat(filepath.Join(env.repoPath, "REVIEW.md")); err == nil {
			hints = append(hints, fmt.Sprintf("REVIEW.md exists in the working tree, but reviews read it from %s; commit and merge it there", branchNote))
		}
	}

	c := doctorCheck{ID: "repo.guidelines", Category: "repo"}
	switch {
	case guidelines == "" && len(security) > 0:
		c.Status = doctorFail
		c.Summary = "security reviews are enabled, but this repository has no review guidelines or threat model"
		c.Details = append(append(security,
			"without a threat model, security reviewers guess at what is trusted and report noise or miss real risks"),
			hints...)
		c.Fix = fmt.Sprintf("add a REVIEW.md at the repository root on %s describing the threat model: who and what is trusted, "+
			"where untrusted input enters, sensitive data, and what is out of scope (or set review_guidelines in .roborev.toml)", branchNote)
	case guidelines == "":
		c.Status = doctorWarn
		c.Summary = "no review guidelines; reviewers only see generic instructions"
		c.Details = hints
		c.Fix = fmt.Sprintf("add a REVIEW.md at the repository root on %s with project conventions and known non-issues "+
			"(or set review_guidelines in .roborev.toml)", branchNote)
	case len(security) > 0 && !mentionsAny(guidelines, securityGuidanceTerms):
		c.Status = doctorWarn
		c.Summary = "security reviews are enabled, but the review guidelines do not describe security or a threat model"
		c.Details = security
		c.Fix = "add a threat model section to REVIEW.md or review_guidelines: trust boundaries, untrusted inputs, sensitive data, and accepted risks"
	case len(security) > 0:
		c.Status = doctorOK
		c.Summary = "review guidelines include security guidance for the enabled security reviews"
	default:
		c.Status = doctorOK
		c.Summary = "review guidelines are configured"
	}
	return []doctorCheck{c}
}

func mentionsAny(text string, terms []string) bool {
	lower := strings.ToLower(text)
	for _, t := range terms {
		if strings.Contains(lower, t) {
			return true
		}
	}
	return false
}

// securityReviewSources lists the review panels that run security reviews
// for the current repository: the panel for manual reviews and the panel for
// post-commit reviews.
func securityReviewSources(env *doctorEnv) []string {
	var sources []string
	merged := config.MergeReviewConfigFromConfig(env.repoCfg, env.global)
	for _, sel := range []struct{ key, name string }{
		{"review.default_panel", merged.DefaultPanel},
		{"review.hook_review_panel", merged.HookPanel},
	} {
		if sel.name != "" && panelHasSecurityMember(env, merged, sel.name) {
			sources = append(sources, fmt.Sprintf("%s = %q includes a security reviewer", sel.key, sel.name))
		}
	}
	return sources
}

func panelHasSecurityMember(env *doctorEnv, merged config.ReviewConfig, name string) bool {
	panel, ok := merged.Panels[name]
	if !ok {
		return false
	}
	for _, member := range panel.Members {
		if spec, ok := merged.Subagents[member]; ok && isSecurityReviewType(env, spec.ReviewType) {
			return true
		}
	}
	return false
}

func isSecurityReviewType(env *doctorEnv, reviewType string) bool {
	if reviewType == "" {
		return false
	}
	canonical, err := config.ValidateReviewTypesFromConfig([]string{reviewType}, env.repoCfg, env.global)
	return err == nil && len(canonical) == 1 && canonical[0] == config.ReviewTypeSecurity
}

// --- Integrations ---

// doctorHookEvents are the event names the daemon broadcasts to [[hooks]].
// Keep in sync with the review.* events published in internal/daemon.
var doctorHookEvents = []string{
	"review.started", "review.completed", "review.failed", "review.canceled",
	"review.closed", "review.reopened", "review.commented", "review.remapped",
}

// doctorHookTools maps a [[hooks]] type to the CLI the daemon runs for it.
var doctorHookTools = map[string]string{"kata": "kata", "beads": "bd"}

// hookToolProblem reports why the daemon cannot run a hook tool, or "" when
// it can. The daemon runs hooks, so its answer wins; the shell is only a
// fallback when the daemon cannot be asked.
func (env *doctorEnv) hookToolProblem(tool string) string {
	if env.daemonAgents != nil {
		for _, d := range env.daemonAgents.HookTools {
			if d.Name == tool {
				if d.Available {
					return ""
				}
				return fmt.Sprintf("the %s CLI is not on the daemon's PATH", tool)
			}
		}
		return fmt.Sprintf("the daemon did not report the %s CLI; restart it so it runs this version of roborev", tool)
	}
	if _, err := exec.LookPath(tool); err != nil {
		return fmt.Sprintf("the %s CLI is not on this shell's PATH (the daemon could not be asked)", tool)
	}
	return ""
}

func checkDoctorIntegrations(env *doctorEnv) []doctorCheck {
	var out []doctorCheck

	hooks := append([]config.HookConfig{}, env.global.Hooks...)
	if env.repoCfg != nil {
		hooks = append(hooks, env.repoCfg.Hooks...)
	}
	var hookProblems []string
	for i, h := range hooks {
		label := fmt.Sprintf("hook %d (event %q)", i+1, h.Event)
		if h.Event != "review.*" && !slices.Contains(doctorHookEvents, h.Event) {
			hookProblems = append(hookProblems, fmt.Sprintf("%s: event never fires; use one of %s, or review.*", label, strings.Join(doctorHookEvents, ", ")))
		}
		switch h.Type {
		case "webhook":
			if strings.TrimSpace(h.URL) == "" {
				hookProblems = append(hookProblems, label+": webhook has no url, so it is skipped")
			}
		case "kata", "beads":
			if problem := env.hookToolProblem(doctorHookTools[h.Type]); problem != "" {
				hookProblems = append(hookProblems, label+": "+problem)
			}
		case "", "command":
			if strings.TrimSpace(h.Command) == "" {
				hookProblems = append(hookProblems, label+": no command, so it is skipped")
			}
		default:
			hookProblems = append(hookProblems, fmt.Sprintf("%s: unknown type %q runs as a command hook", label, h.Type))
		}
	}
	if len(hookProblems) > 0 {
		out = append(out, doctorCheck{
			ID: "integrations.hooks", Category: "integrations", Status: doctorWarn,
			Summary: "some [[hooks]] entries will not run as intended", Details: hookProblems,
			Fix: "fix the [[hooks]] entries in your config",
		})
	}

	if warnings := env.global.Sync.Validate(); len(warnings) > 0 {
		out = append(out, doctorCheck{
			ID: "integrations.sync", Category: "integrations", Status: doctorWarn,
			Summary: "PostgreSQL sync settings have problems", Details: warnings,
			Fix: "fix the [sync] section of your global config",
		})
	}

	ci := env.global.CI
	if ci.Enabled {
		var problems []string
		status := doctorWarn
		if len(ci.Repos) == 0 {
			problems = append(problems, "ci.repos is empty, so the CI poller watches nothing")
		}
		if ci.GitHubAppConfigured() {
			if _, err := ci.GitHubAppPrivateKeyResolved(); err != nil {
				problems = append(problems, "GitHub App private key cannot be read: "+err.Error())
				status = doctorFail
			}
		}
		if _, err := ci.QuietHours.Resolve(); err != nil {
			problems = append(problems, "ci.quiet_hours is invalid and ignored: "+err.Error())
		}
		if len(problems) > 0 {
			out = append(out, doctorCheck{
				ID: "integrations.ci", Category: "integrations", Status: status,
				Summary: "CI poller settings have problems", Details: problems,
				Fix: "fix the [ci] section of your global config",
			})
		}
	}
	return out
}
