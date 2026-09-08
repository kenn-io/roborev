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
		output = TruncateComment(FormatComment(PrepareComment(*commentConfig, r, nil)))
	}
	return header + output
}

func runSynthesis(
	ctx context.Context,
	results []ReviewResult,
	opts SynthesizeOpts,
	commentConfig CommentConfig,
) (SynthesisResult, error) {
	synthAgent, err := getAvailableWithConfig(opts.RepoPath, opts.Agent, opts.GlobalConfig)
	if err != nil {
		return SynthesisResult{}, fmt.Errorf("get synthesis agent: %w", err)
	}

	if opts.Model != "" {
		synthAgent = synthAgent.WithModel(opts.Model)
	}

	if opts.Reasoning != "" {
		reasoning, err := config.NormalizeReasoning(opts.Reasoning)
		if err != nil {
			return SynthesisResult{}, fmt.Errorf("synthesis reasoning: %w", err)
		}
		synthAgent = synthAgent.WithReasoning(agent.ParseReasoningLevel(reasoning))
	}

	synthPrompt := BuildSynthesisPrompt(
		results, opts.MinSeverity)

	synthCtx, cancel := context.WithTimeout(
		ctx, 5*time.Minute)
	defer cancel()

	doc, err := RunSynthesisAgent(synthCtx, synthAgent, results, synthPrompt, opts.MinSeverity, nil, SynthesisHooks{
		ConfigRepoPath: opts.RepoPath,
		GlobalConfig:   opts.GlobalConfig,
		Checkout: func() (SynthesisCheckout, error) {
			return SynthesisCheckout{RepoPath: opts.RepoPath, GitRef: opts.GitRef}, nil
		},
	})
	if err != nil {
		return SynthesisResult{}, fmt.Errorf("synthesis review: %w", err)
	}

	return SynthesisResult{
		Output:        FormatSynthesizedComment(doc.Markdown(opts.MinSeverity), results, opts.HeadSHA),
		GitHubComment: FormatSynthesizedComment(FormatComment(PrepareComment(commentConfig, ReviewResult{Structured: &doc}, nil)), results, opts.HeadSHA),
	}, nil
}
