package review

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"time"

	gitrepo "go.kenn.io/kit/git/repo"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
)

// ErrAllFailed is returned by Synthesize when every review job
// in the batch failed (excluding all-quota-skipped batches).
var ErrAllFailed = errors.New(
	"all review jobs failed")

// getAvailableWithConfig is a variable for dependency injection in tests.
// It preserves SynthesizeOpts.Agent's empty-means-auto-select contract while
// keeping explicit synthesis agents on strict preferred-or-backup resolution.
var getAvailableWithConfig = func(repoPath string, preferred string, cfg *config.Config, backups ...string) (agent.Agent, error) {
	if preferred == "" {
		return agent.GetAvailableWithConfig(repoPath, preferred, cfg, backups...)
	}
	return agent.GetPreferredOrBackupWithConfig(repoPath, preferred, cfg, backups...)
}

// SynthesizeOpts controls synthesis behavior.
type SynthesizeOpts struct {
	Tools *config.ReviewToolPolicy
	// BackupAgent is the configured synthesis backup, validated before invocation.
	BackupAgent string
	// RepoConfigRef marks RepoConfig (including nil absence) as authoritative.
	RepoConfig    *config.RepoConfig
	RepoConfigRef string
	// Neutral preserves custom framing when a caller has already filtered inputs.
	Neutral bool
	// Agent name for synthesis (empty = first available).
	Agent string
	// Model override for the synthesis agent.
	Model string
	// Reasoning override (empty preserves the agent default).
	Reasoning string
	// MinSeverity is the lowest severity that fails the combined review.
	// Findings below it are hidden in the comment but retained in review data.
	MinSeverity string
	// RepoPath is the working directory for the synthesis agent.
	RepoPath string
	// GitRef is the reviewed git ref, passed to the synthesis agent.
	GitRef string
	// HeadSHA is used for comment formatting headers.
	HeadSHA string
	// GlobalConfig allows runtime agent resolution to honor ACP naming/command overrides.
	GlobalConfig *config.Config
}

// SynthesisResult separates complete review output from GitHub presentation.
type SynthesisResult struct {
	Output        string
	GitHubComment string
}

// Synthesize combines reviews once and renders both complete output and the
// filtered GitHub comment. Callers choose the appropriate publication channel.
//
// Single successful result: returns it directly (no LLM call).
// All failed: returns failure comment.
// Multiple results with successes: runs synthesis agent, falls
// back to raw format on error.
func Synthesize(
	ctx context.Context,
	results []ReviewResult,
	opts SynthesizeOpts,
) (SynthesisResult, error) {
	if _, err := config.CanonicalReviewToolPolicy(opts.Tools); err != nil {
		return SynthesisResult{}, err
	}
	for _, result := range results {
		if _, err := config.CanonicalReviewToolPolicy(result.Tools); err != nil {
			return SynthesisResult{}, err
		}
	}
	if derived := SynthesisToolPolicy(results); derived != nil {
		opts.Tools = derived
	}
	opts.Neutral = opts.Neutral || NeedsNeutralSynthesis(results)
	results = slices.Clone(results)
	for i := range results {
		results[i] = results[i].ApplyMinSeverity(opts.MinSeverity)
	}
	opts.MinSeverity = ResolveSynthesisMinSeverity(results, opts.MinSeverity)
	commentConfig := CommentConfig{MinSeverity: opts.MinSeverity}

	successCount := 0
	for _, r := range results {
		if IsSubstantiveOutput(r) {
			successCount++
		}
	}

	// All failed
	if successCount == 0 {
		comment := FormatAllFailedComment(
			results, opts.HeadSHA)
		// Quota skips and completed reviews without output are not
		// actionable. Preserve their successful no-output outcome.
		nonActionable := CountQuotaFailures(results)
		for _, r := range results {
			if r.Status == ResultDone && !IsSubstantiveOutput(r) {
				nonActionable++
			}
		}
		if len(results) > 0 && nonActionable == len(results) {
			return SynthesisResult{Output: comment, GitHubComment: comment}, nil
		}
		return SynthesisResult{Output: comment, GitHubComment: comment}, ErrAllFailed
	}

	// Single result — return directly. Its verdict already honors
	// opts.MinSeverity, so there is nothing for a synthesis agent to add.
	if len(results) == 1 && successCount == 1 {
		return SynthesisResult{
			Output:        formatSingleResult(results[0], opts.HeadSHA, nil),
			GitHubComment: formatSingleResult(results[0], opts.HeadSHA, &commentConfig),
		}, nil
	}

	// Multiple results — synthesize with LLM
	comment, err := runSynthesis(ctx, results, opts, commentConfig)
	if err != nil {
		if errors.Is(err, agent.ErrReviewToolPolicy) {
			return SynthesisResult{}, err
		}
		log.Printf(
			"ci review: synthesis failed: %v "+
				"(falling back to raw format)", err)
		return SynthesisResult{
			Output:        formatRawBatchOutput(results, opts.HeadSHA, nil),
			GitHubComment: FormatRawBatchComment(commentConfig, results, opts.HeadSHA),
		}, nil
	}
	return comment, nil
}

func formatSingleResult(
	r ReviewResult,
	headSHA string,
	commentConfig *CommentConfig,
) string {
	passed := r.Passed()
	if r.Verdict == storage.VerdictUnknown &&
		(r.Output == "" || r.Output == "No issues found.") {
		passed = true
	}
	var header string
	if passed {
		header = fmt.Sprintf(
			"## roborev: Review Passed (`%s`)\n\n",
			gitrepo.ShortSHA(headSHA))
	} else {
		header = fmt.Sprintf(
			"## roborev: Review Complete (`%s`)\n\n",
			gitrepo.ShortSHA(headSHA))
	}

	output := r.Output
	if commentConfig != nil {
		output = TruncateComment(FormatComment(PrepareComment(*commentConfig, r)))
	}
	return header + output
}

func resolveSynthesisAgent(opts SynthesizeOpts) (agent.Agent, error) {
	var synthAgent agent.Agent
	var err error
	if opts.RepoConfigRef != "" || opts.RepoConfig != nil {
		if opts.Agent == "" {
			synthAgent, err = agent.GetAvailableWithConfigFromConfig(opts.RepoConfig, opts.Agent, opts.GlobalConfig)
		} else {
			synthAgent, err = agent.GetPreferredOrBackupWithConfigFromConfig(opts.RepoConfig, opts.Agent, opts.GlobalConfig, opts.BackupAgent)
		}
	} else if opts.Agent == "" {
		synthAgent, err = getAvailableWithConfig(opts.RepoPath, opts.Agent, opts.GlobalConfig)
	} else {
		synthAgent, err = getAvailableWithConfig(opts.RepoPath, opts.Agent, opts.GlobalConfig, opts.BackupAgent)
	}
	// Empty means ordinary primary discovery, not backup-first selection.
	// Record that primary before considering a configured backup so the model
	// override cannot leak onto a backup when discovery fails.
	primary := opts.Agent
	if opts.Agent == "" {
		if err == nil {
			primary = synthAgent.Name()
		} else if opts.BackupAgent != "" {
			if opts.RepoConfigRef != "" || opts.RepoConfig != nil {
				synthAgent, err = agent.GetPreferredOrBackupWithConfigFromConfig(opts.RepoConfig, opts.BackupAgent, opts.GlobalConfig)
			} else {
				synthAgent, err = getAvailableWithConfig(opts.RepoPath, opts.BackupAgent, opts.GlobalConfig)
			}
		}
	}
	if err != nil {
		if opts.Tools != nil {
			return nil, fmt.Errorf("%w: get synthesis agent: %w", agent.ErrReviewToolPolicy, err)
		}
		return nil, fmt.Errorf("get synthesis agent: %w", err)
	}

	if err := agent.ValidateReviewToolBackup(opts.Tools, agent.WorkflowConfig{
		GlobalConfig: opts.GlobalConfig, RepoConfig: opts.RepoConfig, BackupAgent: opts.BackupAgent,
	}, synthAgent.Name()); err != nil {
		return nil, err
	}

	// The explicit model belongs to the preferred adapter. A selected backup
	// keeps its own configured model or default.
	selection := agent.WorkflowConfig{RepoConfig: opts.RepoConfig, GlobalConfig: opts.GlobalConfig}
	if selection.RepoConfig == nil {
		selection.RepoConfig = &config.RepoConfig{}
	}
	if opts.Model != "" && primary != "" && selection.AgentMatches(synthAgent.Name(), primary) {
		synthAgent = synthAgent.WithModel(opts.Model)
	}

	return synthAgent, nil
}

func runSynthesis(
	ctx context.Context,
	results []ReviewResult,
	opts SynthesizeOpts,
	commentConfig CommentConfig,
) (SynthesisResult, error) {
	synthAgent, err := resolveSynthesisAgent(opts)
	if err != nil {
		return SynthesisResult{}, err
	}

	if opts.Reasoning != "" {
		reasoning, err := config.NormalizeReasoning(opts.Reasoning)
		if err != nil {
			return SynthesisResult{}, fmt.Errorf("synthesis reasoning: %w", err)
		}
		synthAgent = synthAgent.WithReasoning(agent.ParseReasoningLevel(reasoning))
	}

	synthPrompt := BuildSynthesisPrompt(
		results, opts.MinSeverity, opts.Neutral)

	synthCtx, cancel := context.WithTimeout(
		ctx, 5*time.Minute)
	defer cancel()

	doc, err := RunSynthesisAgent(synthCtx, synthAgent, results, synthPrompt, opts.MinSeverity, nil, SynthesisHooks{
		Tools:          opts.Tools,
		ConfigRepoPath: opts.RepoPath,
		GlobalConfig:   opts.GlobalConfig,
		Checkout: func() (SynthesisCheckout, error) {
			return SynthesisCheckout{RepoPath: opts.RepoPath, GitRef: opts.GitRef}, nil
		},
	})
	if err != nil {
		selection := agent.WorkflowConfig{RepoConfig: opts.RepoConfig, GlobalConfig: opts.GlobalConfig}
		if selection.RepoConfig == nil {
			selection.RepoConfig = &config.RepoConfig{}
		}
		if opts.BackupAgent != "" && !selection.AgentMatches(synthAgent.Name(), opts.BackupAgent) &&
			synthesisCanFailover(synthCtx, synthAgent.Name(), err) {
			backupOpts := opts
			backupOpts.Agent = opts.BackupAgent
			backupOpts.Model = ""
			backupOpts.BackupAgent = ""
			return runSynthesis(ctx, results, backupOpts, commentConfig)
		}
		return SynthesisResult{}, fmt.Errorf("synthesis review: %w", err)
	}

	return SynthesisResult{
		Output:        FormatSynthesizedComment(doc.Markdown(opts.MinSeverity), results, opts.HeadSHA),
		GitHubComment: FormatSynthesizedComment(FormatComment(PrepareComment(commentConfig, ReviewResult{Structured: &doc})), results, opts.HeadSHA),
	}, nil
}

// synthesisCanFailover excludes policy and checkout errors even when their
// diagnostics happen to contain provider-limit wording.
func synthesisCanFailover(ctx context.Context, name string, err error) bool {
	var checkout *SynthesisCheckoutError
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, agent.ErrReviewToolPolicy) || errors.As(err, &checkout) {
		return false
	}
	classification, attached := agent.LimitClassificationFromError(err)
	if !attached {
		classification = agent.ClassifyLimit(agent.CanonicalName(name), err.Error())
	}
	return classification.Kind != agent.LimitKindNone || agent.IsUnavailable(err)
}
