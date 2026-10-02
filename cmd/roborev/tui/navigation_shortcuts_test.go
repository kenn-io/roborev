package tui

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/roborev/internal/storage"
)

func navigationJobs(count int) []storage.ReviewJob {
	jobs := make([]storage.ReviewJob, count)
	for i := range jobs {
		jobs[i] = makeJob(int64(i + 1))
	}
	return jobs
}

func TestEmacsNavigation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		view  viewKind
		setup func(*model)
		index func(model) int
	}{
		{"queue", viewQueue, func(m *model) {
			m.jobs = navigationJobs(3)
			m.selectedIdx, m.selectedJobID = 1, m.jobs[1].ID
		}, func(m model) int { return m.selectedIdx }},
		{"review", viewReview, func(m *model) { m.reviewScroll = 1 }, func(m model) int { return m.reviewScroll }},
		{"prompt", viewKindPrompt, func(m *model) { m.promptScroll = 1 }, func(m model) int { return m.promptScroll }},
		{"commit", viewCommitMsg, func(m *model) { m.commitMsgScroll = 1 }, func(m model) int { return m.commitMsgScroll }},
		{"help", viewHelp, func(m *model) { m.helpScroll = 1 }, func(m model) int { return m.helpScroll }},
		{"release notes", viewReleaseNotes, func(m *model) {
			m.releaseNotes = []storage.ReleaseNote{{Name: "Release", Body: strings.Repeat("line\n\n", 80)}}
			m.releaseNotesScroll = 1
		}, func(m model) int { return m.releaseNotesScroll }},
		{"log", viewLog, func(m *model) { m.logScroll, m.logFollow = 1, true }, func(m model) int { return m.logScroll }},
		{"patch", viewPatch, func(m *model) { m.patchScroll = 1 }, func(m model) int { return m.patchScroll }},
		{"tasks", viewTasks, func(m *model) {
			m.fixJobs, m.fixSelectedIdx = navigationJobs(3), 1
		}, func(m model) int { return m.fixSelectedIdx }},
		{"columns", viewColumnOptions, func(m *model) {
			m.colOptionsList = []columnOption{{id: 0}, {id: 1}, {id: 2}}
			m.colOptionsIdx = 1
		}, func(m model) int { return m.colOptionsIdx }},
		{"filter", viewFilter, func(m *model) {
			m.filterTree = []treeFilterNode{{name: "alpha"}, {name: "beta"}}
			m.rebuildFilterFlatList()
			m.filterSelectedIdx = 1
		}, func(m model) int { return m.filterSelectedIdx }},
		{"rerun", viewRerunAgent, func(m *model) {
			m.rerunAgentOptions = []string{"test", "test-two", "test-three"}
			m.rerunAgentSelected = 1
		}, func(m model) int { return m.rerunAgentSelected }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := initTestModel(withCurrentView(tt.view))
			m.currentReview = &storage.Review{Output: "review", Prompt: "prompt"}
			m.mdCache.lastReviewMaxScroll, m.mdCache.lastPromptMaxScroll = 100, 100
			tt.setup(&m)
			m, _ = pressCtrl(m, 'p')
			assert.Equal(t, 0, tt.index(m), "Ctrl-P moves up")
			m, _ = pressCtrl(m, 'n')
			assert.Equal(t, 1, tt.index(m), "Ctrl-N moves down")
			assert.Equal(t, tt.view, m.currentView)
			if tt.view == viewLog {
				assert.False(t, m.logFollow)
			}
			if tt.view == viewFilter {
				assert.Empty(t, m.filterSearch)
			}
		})
	}
}
