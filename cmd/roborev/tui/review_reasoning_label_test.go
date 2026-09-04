package tui

import (
	"strings"
	"testing"

	"github.com/mattn/go-runewidth"
	"github.com/stretchr/testify/assert"

	"go.kenn.io/roborev/internal/storage"
)

func TestReviewDetailReasoningLabel(t *testing.T) {
	tests := []struct {
		name      string
		model     string
		reasoning string
		want      string
		absent    string
	}{
		{name: "explicit", model: "gpt-5.5", reasoning: "xhigh", want: "(codex: gpt-5.5-xhigh)", absent: "gpt-5.5)"},
		{name: "legacy default", model: "gpt-5.5", want: "(codex: gpt-5.5-thorough)", absent: "gpt-5.5-standard"},
		{name: "case normalization", model: "gpt-5.5", reasoning: "XHigh", want: "(codex: gpt-5.5-xhigh)", absent: "XHigh"},
		{name: "unknown normalization", model: "gpt-5.5", reasoning: "ultra", want: "(codex: gpt-5.5-standard)", absent: "ultra"},
		{name: "without model", reasoning: "xhigh", want: "(codex)", absent: "(codex:"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := setupRenderModel(viewReview, &storage.Review{
				ID:    10,
				Agent: "codex",
				Job: &storage.ReviewJob{
					ID:        1,
					RepoName:  "myrepo",
					GitRef:    "abc1234",
					Model:     tt.model,
					Reasoning: tt.reasoning,
				},
			})
			fullScreen := stripANSI(m.renderReviewView())
			pane := stripANSI(strings.Join(m.reviewPaneHeaderLines(88), "\n"))
			assert.Contains(t, fullScreen, tt.want)
			assert.Contains(t, pane, tt.want)
			assert.NotContains(t, fullScreen, tt.absent)
			assert.NotContains(t, pane, tt.absent)
		})
	}
}

func TestReviewDetailReasoningLabelFitsPane(t *testing.T) {
	m := setupRenderModel(viewReview, &storage.Review{
		ID:  10,
		Job: &storage.ReviewJob{ID: 1, RepoName: "myrepo", GitRef: "abc1234", Agent: "codex", Model: "gpt-5.5", Reasoning: "xhigh"},
	})
	lines := m.reviewPaneHeaderLines(24)
	assert.Len(t, lines, 3)
	for _, line := range lines {
		assert.LessOrEqual(t, runewidth.StringWidth(stripANSI(line)), 24)
	}
}
