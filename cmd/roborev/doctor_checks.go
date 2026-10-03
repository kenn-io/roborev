package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	gitrepo "go.kenn.io/kit/git/repo"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/daemon"
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
	for _, ref := range env.experimentAgentReferences() {
		add(ref.Name)
	}
	return names
}

// experimentAgentReferences returns agent names that the experimental arm of
// a review or CI experiment sets, keyed "experiments.<id>: <key>". Arms are
// built the way the daemon selects them. An experiment that does not apply is
// reported by the config checks, so it contributes no names here.
func (env *doctorEnv) experimentAgentReferences() []config.AgentReference {
	var refs []config.AgentReference
	for _, workflow := range []config.ExperimentWorkflow{config.ExperimentWorkflowReview, config.ExperimentWorkflowCI} {
		arms, err := config.WorkflowExperimentArms(workflow, env.global, env.repoCfg, env.repoRaw)
		if err != nil {
			continue
		}
		for _, arm := range arms {
			if arm.Arm != config.ExperimentArmExperimental {
				continue
			}
			for _, ref := range config.AgentReferences(arm.RepoConfig) {
				ref.Key = "experiments." + arm.ExperimentID + ": " + ref.Key
				refs = append(refs, ref)
			}
		}
	}
	return refs
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
