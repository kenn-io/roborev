package review

import (
	"context"
	"encoding/json/jsontext"
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
	decodeInput := raw
	if !jsontext.Value([]byte(raw)).IsValid() {
		if noVerdict := NoVerdict(raw); noVerdict != nil {
			return ReviewResult{}, noVerdict
		}
		// Agents without a native structured-output mode (no
		// agent.StructuredReviewAgent, e.g. Kiro) rely on prompt
		// instructions alone to return bare JSON. Under thorough
		// reasoning they sometimes narrate their investigation first
		// and land the JSON answer at the end, often inside a
		// ```json fenced block. Recover that trailing object instead
		// of failing the job outright.
		if extracted := extractTrailingJSONObject(raw); extracted != "" {
			decodeInput = extracted
		}
	}
	structured, err := DecodeStructuredReview(jsontext.Value(decodeInput))
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
		StructuredOutput: append(jsontext.Value(nil), decodeInput...),
		MinSeverity:      minSeverity,
	}, nil
}

// extractTrailingJSONObject recovers a JSON object narrated ahead of by prose,
// optionally wrapped in a trailing ```-fenced code block. It scans forward for
// the first '{' whose remainder parses as one complete, valid JSON value with
// nothing left over, so stray braces earlier in the prose are skipped rather
// than mismatched. Returns "" when no such object is found.
//
// ponytail: this is a textual heuristic, not a parser — it can be fooled by
// prose that happens to contain a brace-balanced, otherwise-valid JSON object
// before the real answer. Upgrade path: once Kiro (or any other
// non-structured-output agent) exposes a true event stream with tool-call
// boundaries, segment on that instead, as the JSONL-based adapters already do
// via newTrailingReviewText.ResetAfterTool.
func extractTrailingJSONObject(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.TrimSpace(strings.TrimSuffix(s, "```"))
	for i := strings.IndexByte(s, '{'); i >= 0; {
		if jsontext.Value(s[i:]).IsValid() {
			return s[i:]
		}
		next := strings.IndexByte(s[i+1:], '{')
		if next < 0 {
			return ""
		}
		i += 1 + next
	}
	return ""
}
