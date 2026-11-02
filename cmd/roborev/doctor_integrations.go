package main

import (
	"fmt"
	"os/exec"
	"strings"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/daemon"
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
	}
	return out
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
