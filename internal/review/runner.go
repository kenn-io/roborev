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
	structuredAgent, ok := a.(agent.StructuredReviewAgent)
	if !ok {
		if !config.IsBuiltInReviewType(reviewType) {
			return ReviewResult{}, fmt.Errorf(
				"agent %q does not support schema-constrained reviews", a.Name(),
			)
		}
		output, err := a.Review(ctx, repoPath, gitRef, reviewPrompt, out)
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
