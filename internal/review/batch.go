package review

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/prompt"
)

// BatchConfig holds parameters for a parallel review batch.
type BatchConfig struct {
	RepoPath    string
	GitRef      string   // "BASE..HEAD" range
	Agents      []string // agent names (resolved per-job)
	ReviewTypes []string // resolved review types
	// Reasoning is an explicit CLI reasoning override. Repository CI
	// reasoning and per-type defaults are resolved separately for each job.
	Reasoning    string
	ContextCount int
	// GlobalConfig supplies trusted operator settings. When it or RepoConfig
	// is set, each job resolves its agent and model through
	// config.ResolveAgentForWorkflow / ResolveModelForWorkflow,
	// matching the CI poller's behavior. Without either config, agents are
	// used as-is.
	GlobalConfig *config.Config
	// RepoConfig is the trusted repository configuration used to resolve
	// custom review templates. When nil, the working tree config is loaded.
	RepoConfig *config.RepoConfig
	// RepoConfigRef is the trusted git ref used to read repository-relative
	// custom templates and includes.
	RepoConfigRef string
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
	workflow := config.WorkflowForReviewType(reviewType)
	reasoning, err := config.ResolveCIReviewReasoningForType(
		cfg.Reasoning, cfg.RepoConfig, cfg.GlobalConfig, reviewType,
	)
	if err != nil {
		result.Status = ResultFailed
		result.Error = fmt.Sprintf("resolve reasoning: %v", err)
		return result
	}

	// Workflow-aware agent/model resolution when config
	// is available; otherwise use the agent name as-is.
	resolvedName := agentName
	var model string
	var backupAgent string
	var resolution agent.WorkflowConfig
	hasConfig := cfg.GlobalConfig != nil || cfg.RepoConfig != nil
	if hasConfig {
		resolution, err = agent.ResolveWorkflowConfigFromConfig(
			agentName, cfg.RepoConfig, cfg.GlobalConfig, workflow, reasoning,
		)
		if err != nil {
			result.Status = ResultFailed
			result.Error = fmt.Sprintf("resolve workflow config: %v", err)
			return result
		}
		resolvedName = resolution.PreferredAgent
		backupAgent = resolution.BackupAgent
	}
	strictWorkflowAgent := hasConfig && (config.HasWorkflowAgentOverrideFromConfig(
		resolution.RepoConfig, cfg.GlobalConfig, workflow, reasoning,
	) || strings.TrimSpace(backupAgent) != "")
	autoDetectAgent := strings.TrimSpace(agentName) == "" && cfg.AgentRegistry == nil && !strictWorkflowAgent

	var resolvedAgent agent.Agent
	if cfg.AgentRegistry != nil {
		if a, ok := cfg.AgentRegistry[resolvedName]; ok {
			resolvedAgent = a
		} else {
			err = fmt.Errorf("no agents available (mock registry)")
		}
	} else if autoDetectAgent {
		resolvedAgent, err = agent.GetAvailableWithConfigFromConfig(
			cfg.RepoConfig, resolvedName, cfg.GlobalConfig, backupAgent)
	} else {
		resolvedAgent, err = agent.GetPreferredOrBackupWithConfigFromConfig(
			cfg.RepoConfig, resolvedName, cfg.GlobalConfig, backupAgent)
	}

	if err == nil && hasConfig {
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
	result.Agent = resolvedAgent.Name()
	if err := agent.ValidateStructuredReviewSelection(reviewType, resolvedAgent); err != nil {
		result.Status = ResultFailed
		result.Error = fmt.Sprintf("resolve agent %q: %v", resolvedName, err)
		return result
	}

	// Apply model override
	if model != "" {
		resolvedAgent = resolvedAgent.WithModel(model)
	}

	// Apply reasoning level
	if reasoning != "" {
		resolvedAgent = resolvedAgent.WithReasoning(
			agent.ParseReasoningLevel(reasoning))
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

	// Build prompt (nil DB = no previous review context). No kata client:
	// daemon-free CI reviews an untrusted checkout, where the PR head
	// controls .roborev.toml and commit messages, so neither kata_context
	// settings nor commit-message refs have a trusted source (unlike the
	// daemon poller, which gates on PR author trust and default-branch
	// config).
	builder := prompt.NewBuilderWithConfig(nil, cfg.GlobalConfig).
		WithContext(ctx).
		ForRepo(cfg.RepoPath, 0).
		WithRepoConfig(cfg.RepoConfig, cfg.RepoConfigRef)

	// Normalize review type for prompt building
	promptReviewType := reviewType
	if config.IsDefaultReviewType(reviewType) {
		promptReviewType = ""
	}

	excludes := config.ResolveExcludePatterns(
		ctx, cfg.RepoPath, cfg.GlobalConfig, promptReviewType,
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

	agentReview, err := RunAgentReview(
		ctx, resolvedAgent, cfg.RepoPath, cfg.GitRef, reviewPrompt,
		reviewType, cfg.MinSeverity, nil,
	)
	if err != nil {
		result.Status = ResultFailed
		result.Error = formatBatchAgentError(resolvedAgent.Name(), err)
		return result
	}

	result.Status = ResultDone
	result.Output = agentReview.Output
	result.Structured = agentReview.Structured
	result.StructuredOutput = agentReview.StructuredOutput
	result.StructuredMinSeverity = agentReview.StructuredMinSeverity
	result.Verdict = agentReview.Verdict
	return result
}

func formatBatchAgentError(agentName string, err error) string {
	msg := fmt.Sprintf("agent review: %v", err)
	classification, ok := agent.LimitClassificationFromError(err)
	if !ok {
		classification = agent.ClassifyLimit(agent.CanonicalName(agentName), msg)
	}
	switch classification.Kind {
	case agent.LimitKindQuota:
		if strings.HasPrefix(msg, QuotaErrorPrefix) {
			return msg
		}
		return QuotaErrorPrefix + msg
	case agent.LimitKindSession, agent.LimitKindTransient:
		return OutageError(msg)
	case agent.LimitKindNone:
		if agent.IsUnavailable(err) {
			return UnavailableError(msg)
		}
		return msg
	default:
		return msg
	}
}
