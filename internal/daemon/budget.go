package daemon

import (
	"cmp"
	"log"
	"maps"
	"slices"
	"sync"
	"time"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
)

type budgetSpendSource interface {
	GetBudgetSpend(time.Time) (storage.CostAggregate, error)
}

// BudgetRouter selects exact available agents using a global soft daily cap.
// Config is supplied per decision so policy reloads never wait for cache expiry.
type BudgetRouter struct {
	source      budgetSpendSource
	coolingDown func(string) bool
	now         func() time.Time
	mu          sync.Mutex
	spend       storage.CostAggregate
	day         time.Time
	expires     time.Time
}

func NewBudgetRouter(source budgetSpendSource, coolingDown func(string) bool) *BudgetRouter {
	return &BudgetRouter{source: source, coolingDown: coolingDown, now: time.Now}
}

// Invalidate refreshes spend after this worker pool records new costs.
func (r *BudgetRouter) Invalidate() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.expires = time.Time{}
}

func (r *BudgetRouter) dailySpend() (storage.CostAggregate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now().UTC()
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	if r.day.Equal(day) && now.Before(r.expires) {
		return r.spend, nil
	}
	spend, err := r.source.GetBudgetSpend(day)
	if err != nil {
		return storage.CostAggregate{}, err
	}
	r.spend = spend
	r.day = day
	r.expires = now.Add(10 * time.Second)
	return spend, nil
}

// ResolveAgent preserves the configured agent below the reserve threshold,
// scores quality/cost inside the reserve, and chooses the cheapest at the cap.
// The caller resolves the configured choice, which gets twice the quality
// weight of alternatives. Model pairing remains the worker's concern.
func (r *BudgetRouter) ResolveAgent(configuredAgent string, repoCfg *config.RepoConfig, cfg *config.Config, reviewType string) (agent.Agent, error) {
	preferred := func() (agent.Agent, error) {
		return agent.GetAvailableExactWithConfigFromConfig(repoCfg, configuredAgent, cfg)
	}
	if cfg == nil || !cfg.Budget.Enabled {
		return preferred()
	}
	canonical := agent.CanonicalName(configuredAgent)
	names := slices.Sorted(maps.Keys(cfg.Budget.AgentCosts))
	if !hasBudgetPrice(configuredAgent, cfg.Budget.AgentCosts) {
		// Without a price for the configured choice, substitution cannot
		// establish a cost saving, even after the soft cap is reached.
		return preferred()
	}
	spend, err := r.dailySpend()
	if err != nil {
		log.Printf("budget routing: read daily spend: %v", err)
		return preferred()
	}
	budget := cfg.Budget
	cents := spend.TotalUSD * 100
	if (spend.JobsTotal > 0 && spend.JobsWithCost == 0) ||
		!budgetBoundaryReached(cents, budget.DailyLimitCents-budget.ReserveFloorCents) {
		return preferred()
	}
	cheapest := budgetBoundaryReached(cents, budget.DailyLimitCents)
	var best agent.Agent
	var bestScore float64
	var bestCost int
	for _, name := range names {
		if agent.CanonicalName(name) == "test" {
			continue
		}
		candidate, err := agent.GetAvailableExactWithConfigFromConfig(repoCfg, name, cfg)
		if err != nil {
			continue
		}
		if agent.ValidateStructuredReviewSelection(reviewType, candidate) != nil {
			continue
		}
		if r.coolingDown != nil && r.coolingDown(candidate.Name()) {
			continue
		}
		cost := budget.AgentCosts[name]
		weight := 1.0
		if !cheapest && candidate.Name() == canonical {
			weight = 2
		}
		score := weight / float64(cost)
		comparison := cmp.Compare(score, bestScore)
		if cheapest {
			comparison = cmp.Compare(bestCost, cost)
		}
		if best != nil && comparison == 0 {
			if best.Name() == canonical || (candidate.Name() != canonical && candidate.Name() >= best.Name()) {
				continue
			}
		}
		if best == nil || comparison >= 0 {
			best = candidate
			bestScore = score
			bestCost = cost
		}
	}
	if best == nil {
		return preferred()
	}
	return best, nil
}

func hasBudgetPrice(name string, costs config.BudgetAgentCosts) bool {
	canonical := agent.CanonicalName(name)
	for candidate := range costs {
		if agent.CanonicalName(candidate) == canonical {
			return true
		}
	}
	return false
}

// budgetBoundaryReached tolerates floating-point accumulation error without
// rounding meaningful sub-cent spend. The tolerance is one billionth of a cent.
func budgetBoundaryReached(cents float64, boundary int) bool {
	return cents >= float64(boundary)-1e-9
}
