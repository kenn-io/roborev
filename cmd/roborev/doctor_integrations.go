package main

import (
	"fmt"
	"os/exec"
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
	var out []doctorCheck

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

// Lever flags >5 runs per PR and >10% retrospective reviews after merge:
// https://github.com/github/lever/blob/f859d0aa2be96981157ee0480655c84877e3cd98/automations/precision-review/README.md
// The share threshold is adapted here to closed-before-post panel runs, a
// proxy for work that could not reach the PR in time. These are advice
// thresholds, not execution limits; report every observed cohort size.
const (
	doctorCIRunsPerPR     = 5
	doctorCIClosedDivisor = 10
)

func checkDoctorCILoad(env *doctorEnv) []doctorCheck {
	if !env.daemonUp() {
		return nil
	}
	summary, err := env.daemon.Summary(env.ctx)
	if err != nil {
		return []doctorCheck{{
			ID: "integrations.ci_load", Category: "integrations", Status: doctorInfo,
			Summary: "CI review load is unavailable", Details: []string{err.Error()},
		}}
	}
	if summary == nil {
		return nil
	}
	return doctorCILoadSummary(summary.Ci)
}

// doctorCILoadSummary evaluates observed counts without fetching state.
func doctorCILoadSummary(ci *generated.CISummary) []doctorCheck {
	if ci == nil || ci.TotalPanels <= 0 {
		return nil
	}
	check := doctorCheck{
		ID: "integrations.ci_load", Category: "integrations", Status: doctorOK,
		Summary: "CI review load in the last 7 days is within advisory thresholds",
		Details: []string{
			fmt.Sprintf("maximum runs per PR: %d", ci.RunsPerPrMax),
			fmt.Sprintf("%d of %d CI panel runs were closed before posting", ci.ClosedBeforePost, ci.TotalPanels),
		},
	}
	var advice []string
	if ci.RunsPerPrMax > doctorCIRunsPerPR {
		advice = append(advice, "increase ci.throttle_interval to reduce repeated runs per PR")
	}
	// For positive integer counts, closed > total/10 is exactly >10%,
	// without overflowing a multiplication of the numerator.
	if ci.ClosedBeforePost > ci.TotalPanels/doctorCIClosedDivisor {
		advice = append(advice, "check max_workers or use a narrower CI review panel so reviews finish sooner")
	}
	if len(advice) > 0 {
		check.Status = doctorWarn
		check.Summary = "CI review load in the last 7 days exceeds advisory thresholds"
		check.Fix = strings.Join(advice, "; ")
	}
	return []doctorCheck{check}
}
