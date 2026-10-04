package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/roborev/internal/storage"
)

func TestFocusedInputsKeepNavigationKeysAsText(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		view  viewKind
		model func() model
		text  func(model) string
	}{
		{
			name: "comment",
			view: viewKindComment,
			model: func() model {
				return initTestModel(withCurrentView(viewKindComment))
			},
			text: func(m model) string { return m.commentText },
		},
		{
			name: "filter search",
			view: viewFilter,
			model: func() model {
				return initTestModel(
					withCurrentView(viewFilter),
					withFilterTree([]treeFilterNode{makeNode("repo", 1)}),
				)
			},
			text: func(m model) string { return m.filterSearch },
		},
		{
			name: "patch save path",
			view: viewPatch,
			model: func() model {
				m := initTestModel(withCurrentView(viewPatch))
				m.savePatchInputActive = true
				return m
			},
			text: func(m model) string { return m.savePatchInput },
		},
		{
			name: "fix prompt",
			view: viewReview,
			model: func() model {
				job := makeJob(1)
				return initTestModel(
					withCurrentView(viewReview),
					withReview(&storage.Review{JobID: 1, Job: &job}),
					withFixPanel(true, true),
				)
			},
			text: func(m model) string { return m.fixPromptText },
		},
	}

	for _, key := range []rune{'u', 'd'} {
		for _, tc := range tests {
			t.Run(tc.name+"/"+string(key), func(t *testing.T) {
				t.Parallel()
				got, _ := pressKey(tc.model(), key)

				assert.Equal(t, string(key), tc.text(got))
				assert.Equal(t, tc.view, got.currentView)
			})
		}
	}
}
