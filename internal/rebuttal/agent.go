package rebuttal

import (
	"context"
	"encoding/json/jsontext"
	"fmt"
	"io"

	"go.kenn.io/roborev/internal/agent"
)

const assessmentJSONSchema = `{"type":"object","additionalProperties":false,"required":["outcome","explanation"],"properties":{"outcome":{"type":"string","enum":["confirmed","refuted","superseded"]},"explanation":{"type":"string","minLength":1}}}`

// RunAgentAssessment returns the final assessment output, using the agent's
// native structured-review mode when available.
func RunAgentAssessment(
	ctx context.Context,
	a agent.Agent,
	repoPath, gitRef, prompt string,
	out io.Writer,
) (string, error) {
	readOnly, ok := a.(agent.ReadOnlyReviewAgent)
	if !ok || !readOnly.SupportsReadOnlyReview() {
		return "", fmt.Errorf("agent %q cannot enforce read-only assessments", a.Name())
	}
	ctx = agent.WithReadOnlyReview(ctx)
	if structured, ok := a.(agent.StructuredReviewAgent); ok {
		result, err := structured.ReviewWithSchema(
			ctx, repoPath, gitRef, prompt, jsontext.Value(assessmentJSONSchema), out,
		)
		if err != nil {
			return "", err
		}
		return string(result), nil
	}
	return a.Review(ctx, repoPath, gitRef, prompt, out)
}
