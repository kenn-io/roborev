package review

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"go.kenn.io/roborev/internal/agent"
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

// RunAgentReview validates JSON for every review, including agents without a
// native schema mode. Markdown is a presentation of the validated document.
func RunAgentReview(
	ctx context.Context,
	a agent.Agent,
	repoPath, gitRef, reviewPrompt, reviewType, minSeverity string,
	out io.Writer,
) (ReviewResult, error) {
	raw, err := invokeReview(
		ctx, a, repoPath, gitRef, reviewPrompt, CustomReviewSchema, out,
	)
	if err != nil {
		return ReviewResult{}, err
	}
	if !json.Valid([]byte(raw)) {
		if noVerdict := NoVerdict(raw); noVerdict != nil {
			return ReviewResult{}, noVerdict
		}
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
