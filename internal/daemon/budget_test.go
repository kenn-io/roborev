package daemon

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
)

type budgetSpendStub struct {
	mu    sync.Mutex
	spend storage.CostAggregate
	err   error
	calls int
}

func (s *budgetSpendStub) GetBudgetSpend(time.Time) (storage.CostAggregate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return s.spend, s.err
}

func TestBudgetRoutingZones(t *testing.T) {
	// A command override proves exact availability without invoking an agent.
	for _, tc := range []struct {
		name  string
		usd   float64
		costs config.BudgetAgentCosts
		want  string
	}{
		{"below reserve", 3.99, config.BudgetAgentCosts{"codex": 15, "gemini": 5}, "codex"},
		{"reserve boundary", 4, config.BudgetAgentCosts{"codex": 15, "gemini": 5}, "gemini"},
		{"quality wins", 4, config.BudgetAgentCosts{"codex": 8, "gemini": 5}, "codex"},
		{"score tie", 4, config.BudgetAgentCosts{"codex": 10, "gemini": 5}, "codex"},
		{"cap boundary", 5, config.BudgetAgentCosts{"codex": 8, "gemini": 5}, "gemini"},
		{"cost tie", 5, config.BudgetAgentCosts{"codex": 5, "gemini": 5}, "codex"},
		{"unavailable cheap agent", 5, config.BudgetAgentCosts{"codex": 15, "gemini": 1}, "codex"},
		{"unpriced configured agent in reserve", 4.5, config.BudgetAgentCosts{"gemini": 500}, "codex"},
		{"unpriced configured agent at cap", 5, config.BudgetAgentCosts{"gemini": 500}, "codex"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			cfg.CodexCmd = "go"
			cfg.GeminiCmd = "go"
			if tc.name == "unavailable cheap agent" {
				cfg.GeminiCmd = "missing-budget-agent-command"
			}
			cfg.Budget = config.BudgetConfig{Enabled: true, DailyLimitCents: 500, ReserveFloorCents: 100, AgentCosts: tc.costs}
			source := &budgetSpendStub{spend: storage.CostAggregate{TotalUSD: tc.usd, JobsWithCost: 1}}
			router := NewBudgetRouter(source, nil)
			got, err := router.ResolveAgent("codex", nil, cfg, config.ReviewTypeDefault, nil)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got.Name())
		})
	}
}

func TestBudgetCacheReloadAndRollover(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.CodexCmd = "go"
	cfg.GeminiCmd = "go"
	cfg.Budget = config.BudgetConfig{Enabled: true, DailyLimitCents: 500, ReserveFloorCents: 100, AgentCosts: config.BudgetAgentCosts{"codex": 15, "gemini": 5}}
	source := &budgetSpendStub{spend: storage.CostAggregate{TotalUSD: 5, JobsWithCost: 1}}
	router := NewBudgetRouter(source, nil)
	now := time.Date(2026, 1, 2, 23, 59, 55, 0, time.UTC)
	router.now = func() time.Time { return now }
	got, err := router.ResolveAgent("codex", nil, cfg, config.ReviewTypeDefault, nil)
	require.NoError(t, err)
	assert.Equal(t, "gemini", got.Name())
	changed := *cfg
	changed.Budget.DailyLimitCents = 1000
	got, err = router.ResolveAgent("codex", nil, &changed, config.ReviewTypeDefault, nil)
	require.NoError(t, err)
	assert.Equal(t, "codex", got.Name())
	assert.Equal(t, 1, source.calls)
	source.spend = storage.CostAggregate{}
	now = now.Add(5 * time.Second)
	got, err = router.ResolveAgent("codex", nil, cfg, config.ReviewTypeDefault, nil)
	require.NoError(t, err)
	assert.Equal(t, "codex", got.Name())
	assert.Equal(t, 2, source.calls)
	source.spend = storage.CostAggregate{TotalUSD: 5, JobsWithCost: 1}
	now = now.Add(10 * time.Second)
	got, err = router.ResolveAgent("codex", nil, cfg, config.ReviewTypeDefault, nil)
	require.NoError(t, err)
	assert.Equal(t, "gemini", got.Name())
	assert.Equal(t, 3, source.calls)
	router.Invalidate()
	_, err = router.ResolveAgent("codex", nil, cfg, config.ReviewTypeDefault, nil)
	require.NoError(t, err)
	assert.Equal(t, 4, source.calls)
}

func TestBudgetFallbacksAndConcurrentCache(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.CodexCmd = "go"
	cfg.GeminiCmd = "go"
	cfg.Budget = config.BudgetConfig{Enabled: true, DailyLimitCents: 500, AgentCosts: config.BudgetAgentCosts{"codex": 15, "gemini": 5}}
	for _, tc := range []struct {
		name               string
		spend              storage.CostAggregate
		err                error
		disabled, cooldown bool
	}{
		{name: "no cost data"},
		{name: "storage failure", err: errors.New("unavailable storage")},
		{name: "disabled", spend: storage.CostAggregate{TotalUSD: 5, JobsWithCost: 1}, disabled: true},
		{name: "cooldown", spend: storage.CostAggregate{TotalUSD: 5, JobsWithCost: 1}, cooldown: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			local := *cfg
			local.Budget.Enabled = !tc.disabled
			source := &budgetSpendStub{spend: tc.spend, err: tc.err}
			router := NewBudgetRouter(source, func(name string) bool { return tc.cooldown && name == "gemini" })
			got, err := router.ResolveAgent("codex", nil, &local, config.ReviewTypeDefault, nil)
			require.NoError(t, err)
			assert.Equal(t, "codex", got.Name())
			if tc.disabled {
				assert.Equal(t, 0, source.calls)
			}
		})
	}
	source := &budgetSpendStub{spend: storage.CostAggregate{TotalUSD: 5, JobsWithCost: 1}}
	router := NewBudgetRouter(source, nil)
	type outcome struct {
		selected agent.Agent
		err      error
	}
	results := make(chan outcome, 20)
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			got, err := router.ResolveAgent("codex", nil, cfg, config.ReviewTypeDefault, nil)
			results <- outcome{selected: got, err: err}
		})
	}
	wg.Wait()
	for range 20 {
		result := <-results
		require.NoError(t, result.err)
		require.NotNil(t, result.selected)
		assert.Equal(t, "gemini", result.selected.Name())
	}
	assert.Equal(t, 1, source.calls)
}

func TestBudgetFractionalBoundaries(t *testing.T) {
	accumulated := 0.0
	for range 50 {
		accumulated += 0.10
	}
	for _, tc := range []struct {
		name           string
		usd            float64
		limit, reserve int
		want           string
	}{
		{"fractional reserve boundary", 0.29, 39, 10, "gemini"},
		{"fractional cap boundary", 0.29, 29, 10, "gemini"},
		{"accumulated cap boundary", accumulated, 500, 100, "gemini"},
		{"accumulated zero reserve", accumulated, 500, 0, "gemini"},
		{"meaningfully below cap", 4.999999, 500, 100, "codex"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			cfg.CodexCmd = "go"
			costs := config.BudgetAgentCosts{"codex": 8, "gemini": 5}
			if tc.name == "fractional reserve boundary" {
				costs["codex"] = 15
			}
			cfg.Budget = config.BudgetConfig{Enabled: true, DailyLimitCents: tc.limit, ReserveFloorCents: tc.reserve, AgentCosts: costs}
			router := NewBudgetRouter(&budgetSpendStub{spend: storage.CostAggregate{TotalUSD: tc.usd, JobsWithCost: 1}}, nil)
			cfg.GeminiCmd = "go"
			got, err := router.ResolveAgent("codex", nil, cfg, config.ReviewTypeDefault, nil)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got.Name())
		})
	}
}

func TestBudgetRoutingWithZeroThresholdOnEmptyDay(t *testing.T) {
	original, err := agent.Get("gemini")
	require.NoError(t, err)
	agent.Register(&agent.FakeAgent{NameStr: "gemini"})
	t.Cleanup(func() { agent.Register(original) })

	cfg := config.DefaultConfig()
	cfg.CodexCmd = "go"
	cfg.Budget = config.BudgetConfig{
		Enabled:           true,
		DailyLimitCents:   100,
		ReserveFloorCents: 100,
		AgentCosts:        config.BudgetAgentCosts{"codex": 15, "gemini": 5},
	}
	router := NewBudgetRouter(&budgetSpendStub{}, nil)

	got, err := router.ResolveAgent("codex", nil, cfg, config.ReviewTypeDefault, nil)
	require.NoError(t, err)
	assert.Equal(t, "gemini", got.Name())
}

func TestBudgetRoutingDoesNotSelectTestAgent(t *testing.T) {
	original, err := agent.Get("test")
	require.NoError(t, err)
	agent.Register(&agent.FakeAgent{NameStr: "test"})
	t.Cleanup(func() { agent.Register(original) })

	cfg := config.DefaultConfig()
	cfg.CodexCmd = "go"
	cfg.Budget = config.BudgetConfig{
		Enabled:           true,
		DailyLimitCents:   500,
		ReserveFloorCents: 100,
		AgentCosts:        config.BudgetAgentCosts{"codex": 15, "test": 1},
	}
	router := NewBudgetRouter(&budgetSpendStub{spend: storage.CostAggregate{TotalUSD: 5, JobsWithCost: 1, JobsTotal: 1}}, nil)

	got, err := router.ResolveAgent("codex", nil, cfg, config.ReviewTypeDefault, nil)
	require.NoError(t, err)
	assert.Equal(t, "codex", got.Name())
}
