package review

import (
	"context"
	"errors"
	"fmt"
	"log"
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
	// MinSeverity filters findings below this level.
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

// Synthesize combines multiple review results into a single
// formatted comment string.
//
// Single successful result: returns it directly (no LLM call).
// All failed: returns failure comment.
// Multiple results with successes: runs synthesis agent, falls
// back to raw format on error.
func Synthesize(
	ctx context.Context,
	results []ReviewResult,
	opts SynthesizeOpts,
) (string, error) {
	successCount := 0
	for _, r := range results {
		if r.Status == ResultDone {
			successCount++
		}
	}

	// All failed
	if successCount == 0 {
		comment := FormatAllFailedComment(
			results, opts.HeadSHA)
		// All-quota is not an error (nothing actionable).
		quotaSkips := CountQuotaFailures(results)
		if len(results) > 0 && quotaSkips == len(results) {
			return comment, nil
		}
		return comment, ErrAllFailed
	}

	// Single result — return directly unless CI min-severity
	// filtering is needed (synthesis applies the filter).
	// "low" means no filtering, so treat same as empty.
	if len(results) == 1 && successCount == 1 &&
		(opts.MinSeverity == "" || opts.MinSeverity == "low") {
		return formatSingleResult(
			results[0], opts.HeadSHA), nil
	}

	// Multiple results — synthesize with LLM
	comment, err := runSynthesis(ctx, results, opts)
	if err != nil {
		log.Printf(
			"ci review: synthesis failed: %v "+
				"(falling back to raw format)", err)
		return FormatRawBatchComment(
			results, opts.HeadSHA), nil
	}
	return comment, nil
}

func formatSingleResult(
	r ReviewResult,
	headSHA string,
) string {
	var header string
	if r.Output == "" || r.Output == "No issues found." ||
		storage.ParseVerdict(r.Output) == "P" {
		header = fmt.Sprintf(
			"## roborev: Review Passed (`%s`)\n\n",
			gitrepo.ShortSHA(headSHA))
	} else {
		header = fmt.Sprintf(
			"## roborev: Review Complete (`%s`)\n\n",
			gitrepo.ShortSHA(headSHA))
	}

	output := r.Output
	const truncSuffix = "\n\n...(truncated)"
	maxLen := MaxCommentLen - len(truncSuffix)
	if len(output) > MaxCommentLen {
		output = TrimPartialRune(output[:maxLen]) + truncSuffix
	}

	return header + output
}

func runSynthesis(
	ctx context.Context,
	results []ReviewResult,
	opts SynthesizeOpts,
) (string, error) {
	synthAgent, err := getAvailableWithConfig(opts.RepoPath, opts.Agent, opts.GlobalConfig)
	if err != nil {
		return "", fmt.Errorf("get synthesis agent: %w", err)
	}

	if opts.Model != "" {
		synthAgent = synthAgent.WithModel(opts.Model)
	}

	synthPrompt := BuildSynthesisPrompt(
		results, opts.MinSeverity)

	synthCtx, cancel := context.WithTimeout(
		ctx, 5*time.Minute)
	defer cancel()

	var output string
	if sa, ok := synthAgent.(agent.SynthesisAgent); ok {
		output, err = sa.Synthesize(synthCtx, synthPrompt, nil)
	} else {
		output, err = synthAgent.Review(
			synthCtx, opts.RepoPath, opts.GitRef, synthPrompt, nil)
	}
	if err != nil {
		return "", fmt.Errorf("synthesis review: %w", err)
	}

	return FormatSynthesizedComment(
		output, results, opts.HeadSHA), nil
}
