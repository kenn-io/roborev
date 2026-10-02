package config

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"go.kenn.io/roborev/internal/agentname"
)

// BudgetAgentCosts contains estimated cents per job for budget candidates.
type BudgetAgentCosts map[string]int

// BudgetConfig configures global soft daily budget routing.
type BudgetConfig struct {
	Enabled           bool             `toml:"enabled" comment:"Route fresh daemon jobs to cheaper agents near the daily soft budget."`
	DailyLimitCents   int              `toml:"daily_limit_cents" comment:"Soft daily spend cap in cents, reset at UTC midnight."`
	ReserveFloorCents int              `toml:"reserve_floor_cents" comment:"Remaining cents at which cost-aware scoring begins."`
	AgentCosts        BudgetAgentCosts `toml:"agent_costs" comment:"Estimated cents per job, keyed by candidate agent name."`
}

// Validate rejects policies with undefined thresholds or ambiguous prices.
func (b BudgetConfig) Validate() error {
	if b.DailyLimitCents < 0 || (b.Enabled && b.DailyLimitCents == 0) {
		return fmt.Errorf("budget.daily_limit_cents must be positive when enabled and nonnegative otherwise")
	}
	if b.ReserveFloorCents < 0 || b.ReserveFloorCents > b.DailyLimitCents {
		return fmt.Errorf("budget.reserve_floor_cents must be between zero and daily_limit_cents")
	}
	seen := make(map[string]string)
	for _, name := range slices.Sorted(maps.Keys(b.AgentCosts)) {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("budget.agent_costs requires a nonempty agent name")
		}
		if err := agentname.ValidateReference(name); err != nil {
			return fmt.Errorf("budget.agent_costs: %w", err)
		}
		if b.AgentCosts[name] <= 0 {
			return fmt.Errorf("budget.agent_costs.%s must be positive", name)
		}
		canonical := agentname.Canonical(name)
		if canonical == "test" {
			return fmt.Errorf("budget.agent_costs: the test agent cannot be used for budget routing")
		}
		if prior, ok := seen[canonical]; ok {
			return fmt.Errorf("budget.agent_costs: %q and %q name the same agent", prior, name)
		}
		seen[canonical] = name
	}
	return nil
}
