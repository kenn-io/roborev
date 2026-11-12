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
	if tool == "kata" && env.kata != nil && env.kata.Enabled {
		// The daemon files through the Kata API, so it does not run the CLI.
		return ""
	}
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

	out = append(out, checkDoctorKata(env)...)

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
		if !config.ResolveCICommentWhenClean(env.repoCfg, env.global) {
			out = append(out, doctorCheck{
				ID: "integrations.ci_comments", Category: "integrations", Status: doctorInfo,
				Summary: "clean GitHub daemon reviews rely on commit statuses for visibility",
				Details: []string{"Requires commit status-write permission for your GitHub App or personal token."},
			})
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

// kataFixes says what to do about each way the connection can be unusable.
var kataFixes = map[string]string{
	"unreachable":             "check that Kata is running and that integrations.kata.endpoint in the daemon's config is right",
	"authentication_required": "give roborev its own Kata credential in integrations.kata.api_key_file so its writes are not attributed to a person",
	"incompatible":            "use a Kata with API schema 0.21.0 through 0.43.0 (Kata v0.23.0 reports 0.43.0) and an allowed endpoint: https, loopback http, or unix://",
	"project_not_found":       "create the Kata project, bind the repository with .kata.local.toml or .kata.toml, or set integrations.kata.default_project",
}

// checkDoctorKata reports the daemon's Kata connection: its state, the project
// the daemon evaluated (this repository's binding as this machine sees it,
// else the daemon's default), the server version, and whether the server
// stores typed replies. The daemon owns the connection, so its answer wins;
// the shell's config is only used to notice that the two disagree.
func checkDoctorKata(env *doctorEnv) []doctorCheck {
	localEnabled := env.global != nil && env.global.Integrations.Kata.Enabled
	const id = "integrations.kata"
	if env.kata == nil {
		if !localEnabled {
			return nil
		}
		if !env.daemonUp() {
			return []doctorCheck{{
				ID: id, Category: "integrations", Status: doctorInfo,
				Summary: "the Kata connection is enabled in config; it is checked when the daemon is running",
			}}
		}
		check := doctorCheck{
			ID: id, Category: "integrations", Status: doctorInfo,
			Summary: "the daemon's Kata connection status could not be read",
			Fix:     "restart the daemon so it runs this version of roborev",
		}
		if env.kataErr != nil {
			check.Details = []string{env.kataErr.Error()}
		}
		return []doctorCheck{check}
	}
	status := *env.kata
	var out []doctorCheck
	if status.Enabled && env.kataBindingErr != nil {
		out = append(out, doctorCheck{
			ID: id + ".binding", Category: "integrations", Status: doctorWarn,
			Summary: "the repository's Kata binding file cannot be read, so Kata calls from this repository fail",
			Details: []string{env.kataBindingErr.Error()},
			Fix:     "fix or remove the .kata.local.toml or .kata.toml in this repository",
		})
	}
	switch {
	case !status.Enabled && !localEnabled:
		return out
	case !status.Enabled:
		return append(out, doctorCheck{
			ID: id, Category: "integrations", Status: doctorWarn,
			Summary: "[integrations.kata] is enabled in this config, but the daemon has it disabled",
			Details: []string{"the daemon may read a different config file, or may have rejected this one; its log says which"},
			Fix:     "enable [integrations.kata] in the config the daemon reads, then run roborev doctor again",
		})
	}
	project := ""
	if status.Project != "" {
		project = fmt.Sprintf("project %q, ", status.Project)
	}
	if status.State != "ready" {
		details := []string{status.Message}
		if status.Project != "" {
			details = append(details, fmt.Sprintf("project: %q", status.Project))
		}
		if status.ServerVersion != "" {
			details = append(details, "server: "+status.ServerVersion+", API schema "+status.APISchemaVersion)
		}
		if status.SecurityNote != "" {
			details = append(details, status.SecurityNote)
		}
		return append(out, doctorCheck{
			ID: id, Category: "integrations", Status: doctorWarn,
			Summary: "the Kata connection is not usable (" + status.State + ")",
			Details: details, Fix: kataFixes[status.State],
		})
	}
	if !status.SupportsReplies {
		return append(out, doctorCheck{
			ID: id, Category: "integrations", Status: doctorInfo,
			Summary: fmt.Sprintf("the Kata connection is ready (%sserver %s), but the server does not store typed replies (API schema %s; needs 0.28.0 or later)", project, status.ServerVersion, status.APISchemaVersion),
			Fix:     "upgrade Kata to v0.19.0 or later to use typed replies; everything else works without them",
		})
	}
	return append(out, doctorCheck{
		ID: id, Category: "integrations", Status: doctorOK,
		Summary: fmt.Sprintf("the Kata connection is ready (%sserver %s, server stores typed replies)", project, status.ServerVersion),
	})
}
