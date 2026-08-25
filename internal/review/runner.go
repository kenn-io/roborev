package review

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
)

// RunAgentReview owns the built-in versus custom review execution contract.
// Built-in reviews derive their verdict once from prose. Custom reviews derive
// it from schema output, then carry that verdict beside the rendered Markdown.
func RunAgentReview(
	ctx context.Context,
	a agent.Agent,
	repoPath, gitRef, reviewPrompt, reviewType, minSeverity string,
	out io.Writer,
) (ReviewResult, error) {
	if config.IsBuiltInReviewType(reviewType) {
		output, err := a.Review(ctx, repoPath, gitRef, reviewPrompt, out)
		if err != nil {
			return ReviewResult{}, err
		}
		result := ReviewResult{Output: output}
		if output != "" {
			result.Verdict = storage.ParseVerdict(output)
		}
		return result, nil
	}

	structuredAgent, ok := a.(agent.StructuredReviewAgent)
	if !ok {
		return ReviewResult{}, fmt.Errorf(
			"agent %q does not support schema-constrained reviews", a.Name(),
		)
	}
	raw, err := structuredAgent.ReviewWithSchema(
		ctx, repoPath, gitRef, reviewPrompt, CustomReviewSchema, out,
	)
	if err != nil {
		return ReviewResult{}, err
	}
	structured, err := DecodeStructuredReview(raw)
	if err != nil {
		return ReviewResult{}, err
	}
	filtered := structured.Filter(minSeverity)
	output := filtered.Markdown()
	if out != nil {
		_, _ = fmt.Fprintln(out, output)
	}
	return ReviewResult{
		Output:                output,
		Verdict:               storage.VerdictFromPassed(filtered.Passed()),
		Structured:            &structured,
		StructuredOutput:      append(json.RawMessage(nil), raw...),
		StructuredMinSeverity: minSeverity,
	}, nil
}
