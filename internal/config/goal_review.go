package config

import (
	"fmt"
	"slices"
	"strings"
)

// GoalReviewConfig selects Superpowers intent artifacts and automatic triggers.
// Nil paths use artifact discovery; a nil Watch uses the default triggers.
type GoalReviewConfig struct {
	Enabled  bool     `toml:"enabled" comment:"Review Superpowers specs/plans and the open Kata graph automatically. Off by default."`
	SpecFile *string  `toml:"spec_file" comment:"Superpowers design spec path; omit for unambiguous discovery."`
	PlanFile *string  `toml:"plan_file" comment:"Superpowers implementation plan path; omit to discover a plan linked to the spec."`
	Watch    []string `toml:"watch" comment:"Automatic triggers: goal (spec/plan artifacts) and kata_graph. An empty list disables polling."`
}

// GoalReviewGateConfig controls the trusted downstream candidate gate policy.
type GoalReviewGateConfig struct {
	Default *string `toml:"default" comment:"Candidate gate mode: block (default), warn, or off."`
}

// GoalReviewPolicyConfig is loaded from global configuration so checkout files
// cannot weaken a candidate gate.
type GoalReviewPolicyConfig struct {
	KataGate GoalReviewGateConfig `toml:"kata_gate"`
}

// ResolveGoalReview applies defaults without changing the supplied repo config.
func ResolveGoalReview(repo *RepoConfig) GoalReviewConfig {
	var resolved GoalReviewConfig
	if repo != nil {
		resolved = repo.GoalReview
	}
	if resolved.Watch == nil {
		resolved.Watch = []string{"goal", "kata_graph"}
	} else {
		resolved.Watch = slices.Clone(resolved.Watch)
	}
	return resolved
}

// ResolveGoalReviewGate returns the gate mode from trusted global config.
func ResolveGoalReviewGate(global *Config) string {
	if global != nil && global.GoalReview.KataGate.Default != nil {
		return *global.GoalReview.KataGate.Default
	}
	return "block"
}

func validateGoalReview(cfg GoalReviewConfig) error {
	for _, field := range []struct {
		name string
		path *string
	}{{"spec_file", cfg.SpecFile}, {"plan_file", cfg.PlanFile}} {
		if field.path != nil && strings.TrimSpace(*field.path) == "" {
			return fmt.Errorf("goal_review.%s must not be blank", field.name)
		}
	}
	seen := make(map[string]bool)
	for _, trigger := range cfg.Watch {
		if trigger != "goal" && trigger != "kata_graph" {
			return fmt.Errorf("goal_review.watch: unknown trigger %q (valid: goal, kata_graph)", trigger)
		}
		if seen[trigger] {
			return fmt.Errorf("goal_review.watch: duplicate trigger %q", trigger)
		}
		seen[trigger] = true
	}
	return nil
}

func validateGoalReviewGate(cfg GoalReviewGateConfig) error {
	if cfg.Default == nil {
		return nil
	}
	switch *cfg.Default {
	case "block", "warn", "off":
		return nil
	default:
		return fmt.Errorf("goal_review.kata_gate.default: invalid mode %q (valid: block, warn, off)", *cfg.Default)
	}
}
