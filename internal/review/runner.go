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

// invokeReview uses the same read-capable agent entry point for ordinary and
// synthesis reviews. The caller supplies the prompt and output schema.
func invokeReview(ctx context.Context, a agent.Agent, repoPath, gitRef, prompt string, schema json.RawMessage, out io.Writer) (string, error) {
	if structured, ok := a.(agent.StructuredReviewAgent); ok {
		raw, err := structured.ReviewWithSchema(ctx, repoPath, gitRef, prompt, schema, out)
		return string(raw), err
	}
	return a.Review(ctx, repoPath, gitRef, prompt, out)
}

// RunAgentReview owns the structured versus prose review execution contract.
// Agents that support schema-constrained output return structured findings
// for every review type, so the verdict comes from the reported severities
// rather than from parsing Markdown. Other agents run built-in review types
// as prose and derive the verdict from the rendered output; custom review
// types require schema support. minSeverity never removes findings: it only
// decides which severities count against the verdict.
func RunAgentReview(
	ctx context.Context,
	a agent.Agent,
	repoPath, gitRef, reviewPrompt, reviewType, minSeverity string,
	out io.Writer,
) (ReviewResult, error) {
	_, ok := a.(agent.StructuredReviewAgent)
	if !ok {
		if !config.IsBuiltInReviewType(reviewType) {
			return ReviewResult{}, fmt.Errorf(
				"agent %q does not support schema-constrained reviews", a.Name(),
			)
		}
		output, err := invokeReview(ctx, a, repoPath, gitRef, reviewPrompt, CustomReviewSchema, out)
		if err != nil {
			return ReviewResult{}, err
		}
		result := ReviewResult{Output: output, MinSeverity: minSeverity}
		if noVerdict := NoVerdict(output); noVerdict != nil {
			return result, noVerdict
		}
		result.Verdict = storage.ParseVerdictAtSeverity(output, minSeverity)
		return result, nil
	}

	raw, err := invokeReview(
		ctx, a, repoPath, gitRef, reviewPrompt, CustomReviewSchema, out,
	)
	if err != nil {
		return ReviewResult{}, err
	}
	structured, err := DecodeStructuredReview(json.RawMessage(raw))
	if err != nil {
		return ReviewResult{}, err
	}
	if err := validateLiveDocument(a.Name(), structured); err != nil {
		return ReviewResult{}, err
	}
	output := structured.Markdown(minSeverity)
	if out != nil {
		_, _ = fmt.Fprintln(out, output)
	}
	return ReviewResult{
		Output:           output,
		Verdict:          storage.VerdictFromPassed(structured.Passed(minSeverity)),
		Structured:       &structured,
		StructuredOutput: append(json.RawMessage(nil), raw...),
		MinSeverity:      minSeverity,
	}, nil
}
