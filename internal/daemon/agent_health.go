package daemon

import (
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
func reviewAgentHealth(cfg *config.Config) storage.ComponentHealth {
	problems := reviewAgentProblems(cfg)
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
// health after two seconds. A request returns the last result and starts at
// most one background refresh, so the result can be one request out of date.
type agentHealthCache struct {
	mu         sync.Mutex
	latest     storage.ComponentHealth
	checked    bool
	refreshing bool
	wg         sync.WaitGroup
}

// check runs the check, stores its result, and returns it.
func (c *agentHealthCache) check(cfg *config.Config) storage.ComponentHealth {
	health := reviewAgentHealth(cfg)
	c.mu.Lock()
	c.latest, c.checked = health, true
	c.mu.Unlock()
	return health
}

// observe returns the last result. Only when no check has run yet does it run
// one itself, because nothing is known.
func (c *agentHealthCache) observe(cfg *config.Config) storage.ComponentHealth {
	c.mu.Lock()
	if !c.checked {
		c.mu.Unlock()
		return c.check(cfg)
	}
	latest := c.latest
	if !c.refreshing {
		c.refreshing = true
		c.wg.Add(1)
		go func() {
			defer c.wg.Done()
			c.check(cfg)
			c.mu.Lock()
			c.refreshing = false
			c.mu.Unlock()
		}()
	}
	c.mu.Unlock()
	return latest
}

// wait blocks until a running background refresh finishes.
func (c *agentHealthCache) wait() { c.wg.Wait() }

// reviewAgentProblems checks every kind of review the global config can start:
// post-commit and manual reviews, in the default arm and in the experimental
// arm of an enabled review experiment. Each one runs the panel it selects, or
// else the single review agent, the way enqueue resolves them.
func reviewAgentProblems(cfg *config.Config) []string {
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
