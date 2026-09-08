package review

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProseCommentSeverityFiltering(t *testing.T) {
	for _, prose := range []string{
		"## Summary\nNaming and persistence changes.\n\n## Review Findings\n\n- **Severity**: Low\n- **Problem**: Minor naming issue.\n- **Fix**: Rename it.\n\n---\n\n- **Severity**: High\n- **Problem**: State is lost.\n- **Fix**: Persist it.\n\n## Summary\nThe naming issue remains.",
		"### 1. Low\n\nMinor naming issue.\n\n### 2. High\n\nState is lost. Persist it.",
		"- Low: Minor naming issue.\n- High: State is lost. Persist it.",
	} {
		t.Run(strings.Split(prose, "\n")[0], func(t *testing.T) {
			result := ReviewResult{Status: ResultDone, Output: prose}.ApplyMinSeverity("high")
			synthesis, err := Synthesize(context.Background(), []ReviewResult{result}, SynthesizeOpts{MinSeverity: "high"})
			comment := synthesis.GitHubComment
			require.NoError(t, err)
			assert := assert.New(t)
			for _, output := range []string{comment, FormatRawBatchComment([]ReviewResult{result}, "abc1234")} {
				assert.NotContains(output, "Minor naming issue.")
				assert.Contains(output, "State is lost.")
				assert.Contains(output, "Persist it.")
			}
			assert.Contains(synthesis.Output, "Minor naming issue.")
			assert.Equal(prose, result.Output)
		})
	}
}

func TestProseCommentAllFindingsBelowThreshold(t *testing.T) {
	result := ReviewResult{Status: ResultDone, Output: "### Low\n\nMinor naming issue."}
	synthesis, err := Synthesize(context.Background(), []ReviewResult{result}, SynthesizeOpts{MinSeverity: "high"})
	comment := synthesis.GitHubComment
	require.NoError(t, err)
	assert.Contains(t, comment, "Review Passed")
	assert.Contains(t, comment, "No findings at or above high severity.")
	assert.NotContains(t, comment, "Minor naming issue.")
}

func TestProseCommentKeepsFindingCodeAndUnlabelledText(t *testing.T) {
	prose := "### High\nState is lost.\n\n```text\nLow: this is example data\n---\n```\nPersist it.\n\n---\n\nAn unlabelled finding.\n\n---\n\n### Low\nMinor naming issue."
	result := ReviewResult{Output: prose, MinSeverity: "high"}
	comment := result.CommentMarkdown()
	assert := assert.New(t)
	assert.Contains(comment, "```text\nLow: this is example data\n---\n```\nPersist it.")
	assert.Contains(comment, "An unlabelled finding.")
	assert.NotContains(comment, "Minor naming issue.")
	assert.Equal(prose, result.Output)
	assert.Equal("Unlabelled review text.", (ReviewResult{Output: "Unlabelled review text.", MinSeverity: "high"}).CommentMarkdown())
}
