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
	}{
		{name: "explicit", model: "gpt-5.5", reasoning: "xhigh", want: "xhigh"},
		{name: "missing", model: "gpt-5.5", want: "—"},
		{name: "recorded value", model: "gpt-5.5", reasoning: "ultra", want: "ultra"},
		{name: "without model", reasoning: "xhigh", want: "xhigh"},
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
			assert := assert.New(t)
			label := "(codex)"
			if tt.model != "" {
				label = "(codex: " + tt.model + ")"
			}
			assert.Contains(fullScreen, label)
			assert.Contains(pane, label)
			assert.Contains(fullScreen, "Reasoning: "+tt.want)
			assert.Contains(pane, "Reasoning: "+tt.want)
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
