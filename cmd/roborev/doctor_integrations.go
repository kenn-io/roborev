package main

import (
	"cmp"
	"fmt"
	"os/exec"
	"slices"
	"strings"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/pkg/client/generated"
)

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
	out := []doctorCheck{checkDoctorUsageProvider(env)}

	hooks := append([]config.HookConfig{}, env.global.Hooks...)
	if env.repoCfg != nil {
		hooks = append(hooks, env.repoCfg.Hooks...)
	}
	var hookProblems []string
	for i, h := range hooks {
		label := fmt.Sprintf("hook %d (event %q)", i+1, h.Event)
		if !daemon.HookEventFires(h.Event) {
			hookProblems = append(hookProblems, fmt.Sprintf("%s: event never fires; use one of %s, or review.*", label, strings.Join(daemon.HookEvents, ", ")))
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
		out = append(out, checkDoctorCILoad(env)...)
	}
	return out
}

// doctorCIBurstRuns is the most CI panel runs one PR may start within an
// hour before doctor warns. A fast refinement loop by a throttle-bypass author
// runs a round every 5-10 minutes, and the default ci.throttle_interval allows
// one run per hour for everyone else, so more than 10 points at repeated
// reruns or a poller re-enqueuing the PR rather than ordinary iteration. The
// total number of runs on a PR is not a problem by itself.
const doctorCIBurstRuns = 10

func checkDoctorCILoad(env *doctorEnv) []doctorCheck {
	if !env.daemonUp() {
		return nil
	}
	summary, err := env.daemon.Summary(env.ctx)
	if err != nil {
		return []doctorCheck{{
			ID: "integrations.ci_bursts", Category: "integrations", Status: doctorInfo,
			Summary: "CI run history is unavailable", Details: []string{err.Error()},
		}}
	}
	if summary == nil {
		return nil
	}
	return doctorCIBursts(summary.Ci)
}

// doctorCIBursts reports PRs whose busiest hour exceeded doctorCIBurstRuns.
func doctorCIBursts(ci *generated.CISummary) []doctorCheck {
	if ci == nil || len(ci.Prs) == 0 {
		return nil
	}
	prs := slices.Clone(ci.Prs)
	slices.SortStableFunc(prs, func(a, b generated.CIPRSummary) int {
		return cmp.Compare(b.PeakHourRuns, a.PeakHourRuns)
	})
	busiest := prs[0]
	check := doctorCheck{
		ID: "integrations.ci_bursts", Category: "integrations", Status: doctorOK,
		Summary: fmt.Sprintf("no PR started more than %d CI runs within an hour in the last 7 days", doctorCIBurstRuns),
		Details: []string{fmt.Sprintf("busiest: %s#%d with %d runs within an hour", busiest.GithubRepo, busiest.PrNumber, busiest.PeakHourRuns)},
	}
	var bursts []string
	for _, pr := range prs {
		if pr.PeakHourRuns <= doctorCIBurstRuns {
			break
		}
		bursts = append(bursts, fmt.Sprintf("%s#%d: %d runs within an hour", pr.GithubRepo, pr.PrNumber, pr.PeakHourRuns))
	}
	if len(bursts) > 0 {
		check.Status = doctorWarn
		check.Summary = fmt.Sprintf("%d PR(s) started more than %d CI runs within an hour in the last 7 days", len(bursts), doctorCIBurstRuns)
		check.Details = bursts
		check.Fix = "check whether these PRs were really pushed or rerun that often; if not, look in the daemon log for the CI poller re-enqueuing them"
	}
	return []doctorCheck{check}
}

func checkDoctorUsageProvider(env *doctorEnv) doctorCheck {
	check := doctorCheck{ID: "integrations.usage", Category: "integrations", Status: doctorOK}
	var result daemon.DoctorUsageProvider
	source := "daemon"
	if env.daemonAgents != nil && env.daemonAgents.UsageProvider != nil {
		result = *env.daemonAgents.UsageProvider
	} else {
		source = "this shell only; daemon usage provider was not checked"
		check.Status = doctorWarn
		switch {
		case env.daemonAgentsErr != nil:
			check.Details = append(check.Details, env.daemonAgentsErr.Error())
		case env.pingErr != nil:
			check.Details = append(check.Details, env.pingErr.Error())
		case env.daemonAgents != nil:
			check.Details = append(check.Details, "the daemon did not report its usage provider; restart it to use this diagnostic")
		}
		if env.globalErr != nil {
			check.Summary = "usage provider (" + source + "): configuration could not be loaded"
			check.Details = append(check.Details, env.globalErr.Error())
			return check
		}
		result = daemon.ResolveDoctorUsageProvider(env.ctx, env.global)
	}
	provider := "agentsview local CLI"
	switch result.Provider {
	case "agentsview":
		provider = "AgentsView at " + result.URL
	case "cost_endpoint":
		provider = "[cost] endpoint"
	}
	check.Summary = "usage provider (" + source + "): " + provider
	if result.Error != "" {
		check.Status = doctorWarn
		check.Details = append(check.Details, result.Error)
	}
	return check
}
