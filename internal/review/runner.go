package review

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"strings"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/storage"
)

// invokeReview uses the same read-capable agent entry point for ordinary and
// synthesis reviews. The caller supplies the prompt and output schema.
func invokeReview(ctx context.Context, a agent.Agent, repoPath, gitRef, prompt string, schema jsontext.Value, out io.Writer) (string, error) {
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
	if err := agent.ValidateStructuredReviewSelection(reviewType, a); err != nil {
		return ReviewResult{}, err
	}
	raw, err := invokeReview(
		ctx, a, repoPath, gitRef, reviewPrompt, CustomReviewSchema, out,
	)
	if err != nil {
		return ReviewResult{}, err
	}
	document, err := agentReviewJSON(raw)
	if err != nil {
		return ReviewResult{}, err
	}
	structured, err := DecodeStructuredReview(document)
	if err != nil {
		return ReviewResult{}, err
	}
	if err := validateLiveDocument(a.Name(), structured); err != nil {
		return ReviewResult{}, err
	}
	settleFindings(ctx, repoPath, gitRef, &structured)
	document, err = json.Marshal(structured)
	if err != nil {
		return ReviewResult{}, fmt.Errorf("encode structured review: %w", err)
	}
	output := structured.Markdown(minSeverity)
	if out != nil {
		_, _ = fmt.Fprintln(out, output)
	}
	return ReviewResult{
		Output:           output,
		Verdict:          storage.VerdictFromPassed(structured.Passed(minSeverity)),
		Structured:       &structured,
		StructuredOutput: jsontext.Value(document),
		MinSeverity:      minSeverity,
	}, nil
}

// agentReviewJSON returns the review document in an agent's raw output.
// Agents without a native schema mode rely on prompt instructions alone and
// sometimes write prose before the document, often ending with a fenced json
// block. Native schema adapters reject invalid JSON themselves, so only those
// prompt-only agents reach the recovery. The trailing document is recovered
// before any prose classification, so narration that mentions a transient
// read failure cannot hide a valid review; the decoder and
// validateLiveDocument then judge the document, including unable_to_review.
// Output with no document is classified by NoVerdict, and anything else is
// left for the decoder to reject.
func agentReviewJSON(raw string) (jsontext.Value, error) {
	if jsontext.Value(raw).IsValid() {
		return jsontext.Value(raw), nil
	}
	if document := extractTrailingJSONObject(raw); document != "" {
		return jsontext.Value(document), nil
	}
	if noVerdict := NoVerdict(raw); noVerdict != nil {
		return nil, noVerdict
	}
	return jsontext.Value(raw), nil
}

// extractTrailingJSONObject returns the JSON object that ends the output,
// optionally closed by a ``` fence. Only an object that starts a line is
// tried, so braces inside prose cost nothing. The object must run to the end:
// text after it, including after a closing fence, is not recovered. Returns ""
// when no line starts a complete JSON value.
func extractTrailingJSONObject(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.TrimSpace(strings.TrimSuffix(s, "```"))
	offset := 0
	for line := range strings.Lines(s) {
		if strings.HasPrefix(line, "{") && jsontext.Value(s[offset:]).IsValid() {
			return s[offset:]
		}
		offset += len(line)
	}
	return ""
}
