package daemon

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
)

// reviewAgentHealth reports whether this daemon can run the review agents
// that the global config selects for post-commit and manual reviews. Enqueue
// rejects a review whose agents cannot run, and a post-commit hook has no
// terminal to report that on, so the daemon reports it here instead. Repo
// configs can select other agents; `roborev doctor` checks those per repo.
func reviewAgentHealth(ctx context.Context, cfg *config.Config) storage.ComponentHealth {
	problems := reviewAgentProblems(ctx, cfg)
	if len(problems) == 0 {
		return storage.ComponentHealth{Name: "agents", Healthy: true}
	}
	return storage.ComponentHealth{
		Name:    "agents",
		Healthy: false,
		Message: fmt.Sprintf("%s (daemon PATH=%s)", strings.Join(problems, "; "), os.Getenv("PATH")),
	}
}

// agentHealthCache serves the agents health component without running the
// check inside a health request. The check can run an agent executable to
// identify it, and that can hang, while roborev status stops waiting for
// health after two seconds. At most one background check runs at a time. A
// request returns the last result, or a pending observation before the first
// one exists, and starts a refresh. Stopping the cache cancels its context and
// waits for the running check, so no check outlives the daemon.
type agentHealthCache struct {
	ctx    context.Context //nolint:containedctx // cache lifetime; Stop cancels it and waits for the check
	cancel context.CancelFunc
	// run performs one check; it is a field so tests can control it.
	run func(ctx context.Context, cfg *config.Config) storage.ComponentHealth
	// onFirst, when set, receives the first completed result.
	onFirst func(storage.ComponentHealth)

	mu         sync.Mutex
	latest     storage.ComponentHealth
	checked    bool
	refreshing bool
	wg         sync.WaitGroup
}

func newAgentHealthCache() *agentHealthCache {
	ctx, cancel := context.WithCancel(context.Background())
	return &agentHealthCache{ctx: ctx, cancel: cancel, run: reviewAgentHealth}
}

// refreshLocked starts a background check unless one is running or the cache is
// stopped. The caller holds c.mu.
func (c *agentHealthCache) refreshLocked(cfg *config.Config) {
	if c.refreshing || c.ctx.Err() != nil {
		return
	}
	c.refreshing = true
	c.wg.Go(func() {
		health := c.run(c.ctx, cfg)
		c.mu.Lock()
		defer c.mu.Unlock()
		c.refreshing = false
		if c.ctx.Err() != nil {
			return
		}
		first := !c.checked
		c.latest, c.checked = health, true
		if first && c.onFirst != nil {
			c.onFirst(health)
		}
	})
}

// start begins the first check at daemon startup.
func (c *agentHealthCache) start(cfg *config.Config) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refreshLocked(cfg)
}

// observe returns the last result and starts a refresh. Before the first check
// finishes it reports that the check is still running, as healthy, so that
// no request waits for an executable probe.
func (c *agentHealthCache) observe(cfg *config.Config) storage.ComponentHealth {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refreshLocked(cfg)
	if !c.checked {
		return storage.ComponentHealth{Name: "agents", Healthy: true, Message: "first agent check is still running"}
	}
	return c.latest
}

// wait blocks until a running background check finishes.
func (c *agentHealthCache) wait() { c.wg.Wait() }

// stop cancels the context, which ends the running check between routes, and
// waits for it. A probe in progress ends at its own deadline.
func (c *agentHealthCache) stop() {
	c.cancel()
	c.wg.Wait()
}

// reviewAgentProblems checks every kind of review the global config can start:
// post-commit and manual reviews, in the default arm and in the experimental
// arm of an enabled review experiment. Each one runs the panel it selects, or
// else the single review agent, the way enqueue resolves them.
func reviewAgentProblems(ctx context.Context, cfg *config.Config) []string {
	arms, err := config.WorkflowExperimentArms(config.ExperimentWorkflowReview, cfg, nil, nil)
	if err != nil {
		return []string{firstLine(err.Error())}
	}
	var problems []string
	add := func(problem string) {
		if !slices.Contains(problems, problem) {
			problems = append(problems, problem)
		}
	}
	for _, arm := range arms {
		if !arm.Assignable {
			continue
		}
		for _, src := range doctorPanelSources {
			if ctx.Err() != nil {
				return problems
			}
			name := arm.PanelName(src.source)
			if name == "" {
				if err := singleReviewAgentError(arm.RepoConfig, cfg); err != nil {
					add(firstLine(err.Error()))
				}
				continue
			}
			p := planDoctorPanel("", name, arm.RepoConfig, cfg)
			if p.Error != "" {
				add(fmt.Sprintf("panel %s: %s", p.Name, firstLine(p.Error)))
			} else if p.Synthesis.Error != "" {
				add(fmt.Sprintf("panel %s synthesis: %s", p.Name, firstLine(p.Synthesis.Error)))
			}
		}
	}
	return problems
}

// singleReviewAgentError reports why the single review agent cannot run. It
// resolves the reasoning level first because reasoning-specific agent
// settings such as review_agent_thorough select the agent.
func singleReviewAgentError(repoCfg *config.RepoConfig, cfg *config.Config) error {
	reasoning, err := config.ResolveReviewReasoningFromConfig("", repoCfg, cfg)
	if err != nil {
		return err
	}
	resolution, err := agent.ResolveWorkflowConfigFromConfig("", repoCfg, cfg, "review", reasoning)
	if err != nil {
		return err
	}
	_, err = agent.GetPreferredOrBackupWithConfigFromConfig(
		repoCfg, resolution.PreferredAgent, cfg, resolution.BackupAgent,
	)
	return err
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}
