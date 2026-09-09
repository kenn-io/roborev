package tui

import (
	"net/url"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
)

func TestTUIFindingsQueue(t *testing.T) {
	closed := false
	m := newTuiModel("http://localhost")
	m.currentView = viewQueue
	m.width = 200
	m.height = 30
	m.hiddenColumns = map[int]bool{}
	m.columnOrder = append([]int(nil), toggleableColumns...)
	m.selectedJobID = 2
	m.jobs = []storage.ReviewJob{{
		ID:       1,
		Status:   storage.JobStatusDone,
		GitRef:   "abc1234",
		RepoName: "repo",
		Closed:   &closed,
		Verdict:  new("F"),
		FindingCounts: &storage.FindingCounts{
			High: 1, Medium: 2,
		},
	}}

	output := stripANSI(m.renderQueueView())
	assert.Contains(t, output, "H/M/L")
	assert.Contains(t, output, "1/2/0")
	assert.Contains(t, output, "P/F")
	assert.Contains(t, output, "Closed")
	assert.Equal(t, 5, lipgloss.Width(findingCountsCell(m.jobs[0].FindingCounts)))
}

func TestTUIFindingsUnavailable(t *testing.T) {
	assert.Equal(t, "-", findingCountsCell(nil))
	assert.Equal(t, "0/0/0", findingCountsCell(&storage.FindingCounts{}))
	assert.Equal(t, "~1/0/2", findingCountsCell(&storage.FindingCounts{High: 1, Low: 2, Approximate: true}))
}

func TestTUIFindingsColor(t *testing.T) {
	assert.Equal(t, failStyle.GetForeground(), findingCountsColor(&storage.FindingCounts{Critical: 1}))
	assert.Equal(t, failStyle.GetForeground(), findingCountsColor(&storage.FindingCounts{High: 1, Medium: 2}))
	assert.Equal(t, failedStyle.GetForeground(), findingCountsColor(&storage.FindingCounts{Medium: 1}))
	assert.Equal(t, queuedStyle.GetForeground(), findingCountsColor(&storage.FindingCounts{Low: 1}))
	assert.Nil(t, findingCountsColor(nil))
	assert.Nil(t, findingCountsColor(&storage.FindingCounts{}))
}

func TestTUIFindingsPanels(t *testing.T) {
	m := seededPanelModel(t)
	m.jobs[1].FindingCounts = &storage.FindingCounts{High: 1}
	m.panelMembers[testUUID("R")][0].FindingCounts = &storage.FindingCounts{Medium: 2}
	m.panelMembers[testUUID("R")][1].FindingCounts = &storage.FindingCounts{Low: 3}
	m.expandedPanels[testUUID("R")] = true

	rows := m.visibleQueueRows()
	assert.Equal(t, int64(10), rows[1].job.ID)
	assert.Equal(t, "1/0/0", findingCountsCell(rows[1].job.FindingCounts))
	assert.Equal(t, "0/2/0", findingCountsCell(rows[2].job.FindingCounts))
	assert.Equal(t, "0/0/3", findingCountsCell(rows[3].job.FindingCounts))
	assert.NotContains(t, findingCountsCell(rows[1].job.FindingCounts), "2")
}

func TestTUIFindingsRenderProof(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("ROBOREV_COLOR_MODE", "dark")
	m := newTuiModel("http://localhost")
	m.currentView = viewQueue
	m.height = 30
	m.hiddenColumns = map[int]bool{}
	m.columnOrder = append([]int(nil), toggleableColumns...)
	m.jobs = []storage.ReviewJob{{
		ID: 1, Status: storage.JobStatusDone, GitRef: "abc1234", RepoName: "repo",
		Verdict: new("F"), FindingCounts: &storage.FindingCounts{High: 1, Medium: 2},
	}}
	for _, width := range []int{60, 80, 120, 200} {
		m.width = width
		output := m.renderQueueView()
		line := ""
		for _, candidate := range strings.Split(output, "\n") {
			if strings.Contains(candidate, "H/M/L") || strings.Contains(candidate, "1/2/0") {
				line = candidate
				break
			}
		}
		if line == "" {
			for _, candidate := range strings.Split(output, "\n") {
				if strings.Contains(candidate, "abc1234") {
					line = candidate
					break
				}
			}
		}
		maxWidth := 0
		for _, candidate := range strings.Split(output, "\n") {
			maxWidth = max(maxWidth, lipgloss.Width(candidate))
		}
		t.Logf("queue width=%d max_display_width=%d header=%t cell=%t line=%q", width, maxWidth, strings.Contains(output, "H/M/L"), strings.Contains(output, "1/2/0"), line)
		assert.LessOrEqual(t, maxWidth, width)
	}

	t.Setenv("NO_COLOR", "1")
	m.width = 120
	output := m.renderQueueView()
	t.Logf("queue no_color=1 contains_sgr=%t", strings.Contains(output, "\x1b[38;"))
}

func TestTUIFindingsRequestOption(t *testing.T) {
	query := listJobsQuery(url.Values{"include_findings": []string{"true"}})
	require.NotNil(t, query.IncludeFindings)
	assert.Equal(t, "true", string(*query.IncludeFindings))
}
