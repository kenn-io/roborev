package review

import (
	"context"
	"fmt"
	"io"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
)

// AgentReview is the canonical result of one agent review. Output and Verdict
// are resolved together so callers do not reinterpret rendered review text.
// Structured is populated for schema-constrained custom reviews.
type AgentReview struct {
	Output     string
	Verdict    storage.Verdict
	Structured *StructuredReview
}

// RunAgentReview owns the built-in versus custom review execution contract.
// Built-in reviews derive their verdict once from prose. Custom reviews derive
// it from schema output, then carry that verdict beside the rendered Markdown.
func RunAgentReview(
	ctx context.Context,
	a agent.Agent,
	repoPath, gitRef, reviewPrompt, reviewType, minSeverity string,
	out io.Writer,
) (AgentReview, error) {
	if config.IsBuiltInReviewType(reviewType) {
		output, err := a.Review(ctx, repoPath, gitRef, reviewPrompt, out)
		if err != nil {
			return AgentReview{}, err
		}
		result := AgentReview{Output: output}
		if output != "" {
			result.Verdict = storage.ParseVerdict(output)
		}
		return result, nil
	}

	structuredAgent, ok := a.(agent.StructuredReviewAgent)
	if !ok {
		return AgentReview{}, fmt.Errorf(
			"agent %q does not support schema-constrained reviews", a.Name(),
		)
	}
	raw, err := structuredAgent.ReviewWithSchema(
		ctx, repoPath, gitRef, reviewPrompt, CustomReviewSchema, out,
	)
	if err != nil {
		return AgentReview{}, err
	}
	structured, err := DecodeStructuredReview(raw)
	if err != nil {
		return AgentReview{}, err
	}
	structured = structured.Filter(minSeverity)
	output := structured.Markdown()
	if out != nil {
		_, _ = fmt.Fprintln(out, output)
	}
	return AgentReview{
		Output:     output,
		Verdict:    storage.VerdictFromPassed(structured.Passed()),
		Structured: &structured,
	}, nil
}
