package review

import (
	"context"
	"fmt"
	"log"
	"sync"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/prompt"
)

// BatchConfig holds parameters for a parallel review batch.
type BatchConfig struct {
	RepoPath     string
	GitRef       string   // "BASE..HEAD" range
	Agents       []string // agent names (resolved per-job)
	ReviewTypes  []string // resolved review types
	Reasoning    string
	ContextCount int
	// GlobalConfig enables workflow-aware agent/model resolution.
	// When set, each job resolves its agent and model through
	// config.ResolveAgentForWorkflow / ResolveModelForWorkflow,
	// matching the CI poller's behavior. When nil, agents are
	// used as-is (backward compatible).
	GlobalConfig *config.Config
	// AgentRegistry is an optional registry for dependency injection in testing.
	// If nil, the global agent registry is used.
	AgentRegistry map[string]agent.Agent
	// MinSeverity is the minimum severity threshold for the review prompt.
	// When non-empty (and not "low"), agents are instructed to filter findings.
	MinSeverity string
}

// RunBatch executes all review_type x agent combinations in
// parallel. Uses goroutines + sync.WaitGroup, no daemon/database.
func RunBatch(
	ctx context.Context,
	cfg BatchConfig,
) []ReviewResult {
	type job struct {
		agent      string
		reviewType string
	}

	var jobs []job
	for _, rt := range cfg.ReviewTypes {
		for _, ag := range cfg.Agents {
			jobs = append(jobs, job{
				agent:      ag,
				reviewType: rt,
			})
		}
	}

	results := make([]ReviewResult, len(jobs))
	var wg sync.WaitGroup

	for i, j := range jobs {
		wg.Add(1)
		go func(idx int, j job) {
			defer wg.Done()
			results[idx] = runSingle(
				ctx, cfg, j.agent, j.reviewType)
		}(i, j)
	}

	wg.Wait()
	return results
}

func runSingle(
	ctx context.Context,
	cfg BatchConfig,
	agentName string,
	reviewType string,
) ReviewResult {
	result := ReviewResult{
		Agent:      agentName,
		ReviewType: reviewType,
	}

	// Map review type to workflow name for config
	// resolution (same mapping as CI poller).
	workflow := "review"
	if !config.IsDefaultReviewType(reviewType) {
		workflow = reviewType
	}

	// Workflow-aware agent/model resolution when config
	// is available; otherwise use the agent name as-is.
	resolvedName := agentName
	var model string
	var backupAgent string
	var resolution agent.WorkflowConfig
	var err error
	if cfg.GlobalConfig != nil {
		resolution, err = agent.ResolveWorkflowConfig(
			agentName, cfg.RepoPath, cfg.GlobalConfig, workflow, cfg.Reasoning,
		)
		if err != nil {
			result.Status = ResultFailed
			result.Error = fmt.Sprintf("resolve workflow config: %v", err)
			return result
		}
		resolvedName = resolution.PreferredAgent
		backupAgent = resolution.BackupAgent
	}

	var resolvedAgent agent.Agent
	if cfg.AgentRegistry != nil {
		if a, ok := cfg.AgentRegistry[resolvedName]; ok {
			resolvedAgent = a
		} else {
			err = fmt.Errorf("no agents available (mock registry)")
		}
	} else {
		resolvedAgent, err = agent.GetAvailableWithConfig(
			cfg.RepoPath, resolvedName, cfg.GlobalConfig, backupAgent)
	}

	if err == nil && cfg.GlobalConfig != nil {
		model = resolution.ModelForSelectedAgent(
			resolvedAgent.Name(), "",
		)
	}

	if err != nil {
		result.Status = ResultFailed
		result.Error = fmt.Sprintf(
			"resolve agent %q: %v",
			resolvedName, err)
		return result
	}

	// Apply model override
	if model != "" {
		resolvedAgent = resolvedAgent.WithModel(model)
	}

	// Apply reasoning level
	if cfg.Reasoning != "" {
		resolvedAgent = resolvedAgent.WithReasoning(
			agent.ParseReasoningLevel(cfg.Reasoning))
	}
	resolvedAgent = agent.WithCodexSkillsDisabled(
		resolvedAgent,
		config.ResolveDisableCodexReviewSkills(cfg.RepoPath, cfg.GlobalConfig),
	)
	resolvedAgent = agent.WithCodexUserConfigIgnored(
		resolvedAgent,
		config.ResolveIgnoreCodexReviewUserConfig(cfg.RepoPath, cfg.GlobalConfig),
	)

	// Record the resolved agent name
	result.Agent = resolvedAgent.Name()

	// Build prompt (nil DB = no previous review context)
	builder := prompt.NewBuilderWithConfig(nil, cfg.GlobalConfig).ForRepo(cfg.RepoPath, 0)

	// Normalize review type for prompt building
	promptReviewType := reviewType
	if config.IsDefaultReviewType(reviewType) {
		promptReviewType = ""
	}

	excludes := config.ResolveExcludePatterns(
		cfg.RepoPath, cfg.GlobalConfig, promptReviewType,
	)
	snapResult, err := builder.BuildWithSnapshot(
		cfg.GitRef, cfg.ContextCount,
		resolvedAgent.Name(), promptReviewType, cfg.MinSeverity, excludes)
	if err != nil {
		result.Status = ResultFailed
		result.Error = fmt.Sprintf(
			"build prompt: %v", err)
		return result
	}
	reviewPrompt := snapResult.Prompt
	if snapResult.Cleanup != nil {
		defer snapResult.Cleanup()
	}

	// Run review
	log.Printf(
		"ci review: running agent=%s type=%s ref=%s",
		resolvedAgent.Name(), reviewType, cfg.GitRef)

	output, err := resolvedAgent.Review(
		ctx, cfg.RepoPath, cfg.GitRef, reviewPrompt, nil)
	if err != nil {
		result.Status = ResultFailed
		result.Error = fmt.Sprintf(
			"agent review: %v", err)
		return result
	}

	result.Status = ResultDone
	result.Output = output
	return result
}
