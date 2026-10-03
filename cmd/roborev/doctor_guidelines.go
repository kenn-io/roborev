package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/prompt"
)

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
// post-commit reviews, in the default config and in a review experiment's
// experimental arm.
func securityReviewSources(env *doctorEnv) []string {
	arms, err := config.WorkflowExperimentArms(config.ExperimentWorkflowReview, env.global, env.repoCfg, env.repoRaw)
	if err != nil {
		// The config checks report an experiment that does not apply.
		arms = []config.ExperimentArmConfig{{RepoConfig: env.repoCfg}}
	}
	var sources []string
	// The experimental arm repeats the default selections; report each
	// setting and panel once, under the first arm that selects it.
	seen := map[string]bool{}
	for _, arm := range arms {
		prefix := ""
		if arm.Arm == config.ExperimentArmExperimental {
			prefix = "experiments." + arm.ExperimentID + ": "
		}
		merged := config.MergeReviewConfigFromConfig(arm.RepoConfig, env.global)
		for _, sel := range []struct{ key, name string }{
			{"review.default_panel", merged.DefaultPanel},
			{"review.hook_review_panel", merged.HookPanel},
		} {
			if sel.name == "" || seen[sel.key+"/"+sel.name] || !panelHasSecurityMember(env, merged, sel.name) {
				continue
			}
			seen[sel.key+"/"+sel.name] = true
			sources = append(sources, fmt.Sprintf("%s%s = %q includes a security reviewer", prefix, sel.key, sel.name))
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
