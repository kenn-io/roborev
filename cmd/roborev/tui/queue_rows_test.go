package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/roborev/internal/storage"
)

func TestFlattenQueueRowsNoPanels(t *testing.T) {
	jobs := []storage.ReviewJob{makeJob(3), makeJob(2), makeJob(1)}
	rows := flattenQueueRows(jobs, map[string]bool{}, nil)
	assert.Len(t, rows, 3)
	for i, r := range rows {
		assert.Equal(t, jobs[i].ID, r.job.ID)
		assert.Equal(t, 0, r.depth)
		assert.False(t, r.hasChildren, "non-panel rows have no children")
	}
}

func TestFlattenQueueRowsCollapsedParent(t *testing.T) {
	parent := makeJob(10, withSynthesis("R", storage.PanelSummary{MembersTotal: 2, MembersTerminal: 1}))
	rows := flattenQueueRows([]storage.ReviewJob{parent}, map[string]bool{}, nil)
	assert.Len(t, rows, 1, "collapsed parent shows no members")
	assert.True(t, rows[0].hasChildren)
	assert.False(t, rows[0].expanded)
}

func TestFlattenQueueRowsExpandedParent(t *testing.T) {
	assert := assert.New(t)
	parent := makeJob(10, withSynthesis("R", storage.PanelSummary{MembersTotal: 2, MembersTerminal: 2}))
	members := map[string][]storage.ReviewJob{
		"R": {
			makeJob(11, withPanelMember("R", "default", 0)),
			makeJob(12, withPanelMember("R", "security", 1)),
		},
	}
	rows := flattenQueueRows([]storage.ReviewJob{parent}, map[string]bool{"R": true}, members)
	assert.Len(rows, 3)
	assert.Equal(int64(10), rows[0].job.ID)
	assert.True(rows[0].expanded)
	assert.Equal(1, rows[1].depth)
	assert.Equal(int64(11), rows[1].job.ID)
	assert.False(rows[1].lastChild)
	assert.Equal(int64(12), rows[2].job.ID)
	assert.True(rows[2].lastChild, "final member is lastChild for the └─ connector")
}

func TestFlattenQueueRowsExpandedButMembersNotYetFetched(t *testing.T) {
	parent := makeJob(10, withSynthesis("R", storage.PanelSummary{MembersTotal: 2}))
	rows := flattenQueueRows([]storage.ReviewJob{parent}, map[string]bool{"R": true}, nil)
	assert.Len(t, rows, 1)
	assert.True(t, rows[0].expanded)
}

func TestFlattenMembersSortedByIndex(t *testing.T) {
	parent := makeJob(10, withSynthesis("R", storage.PanelSummary{MembersTotal: 2}))
	members := map[string][]storage.ReviewJob{"R": {
		makeJob(12, withPanelMember("R", "security", 1)),
		makeJob(11, withPanelMember("R", "default", 0)),
	}}
	rows := flattenQueueRows([]storage.ReviewJob{parent}, map[string]bool{"R": true}, members)
	assert.Equal(t, int64(11), rows[1].job.ID, "members render by PanelMemberIndex")
	assert.Equal(t, int64(12), rows[2].job.ID)
}

func TestDisclosureGlyph(t *testing.T) {
	assert := assert.New(t)
	assert.Equal("▾", disclosureGlyph(true, true, true))
	assert.Equal("▸", disclosureGlyph(true, false, true))
	assert.Equal(" ", disclosureGlyph(false, false, true))
	assert.Equal("-", disclosureGlyph(true, true, false))
	assert.Equal("+", disclosureGlyph(true, false, false))
}

func TestChildConnector(t *testing.T) {
	assert := assert.New(t)
	assert.Equal("├─", childConnector(false, true))
	assert.Equal("└─", childConnector(true, true))
	assert.Equal("+-", childConnector(false, false))
	assert.Equal(`\-`, childConnector(true, false))
}

func TestGroupBandingSharedBand(t *testing.T) {
	rows := []queueRow{{depth: 0}, {depth: 1}, {depth: 1, lastChild: true}, {depth: 0}}
	bands := groupBanding(rows)
	assert.Equal(t, bands[0], bands[1], "parent and members share a band")
	assert.Equal(t, bands[1], bands[2])
	assert.NotEqual(t, bands[0], bands[3], "band flips at the next parent")
}

func TestPanelCountLabelInProgressVsTerminal(t *testing.T) {
	assert := assert.New(t)
	running := makeJob(1, withStatus(storage.JobStatusQueued),
		withSynthesis("R", storage.PanelSummary{MembersTotal: 3, MembersTerminal: 2}))
	assert.Equal("synthesizing… 2/3 reviewers done", panelStatusCell(running))
	terminal := makeJob(2, withStatus(storage.JobStatusDone),
		withSynthesis("R", storage.PanelSummary{MembersTotal: 3, MembersTerminal: 3, MembersSucceeded: 2, MembersFailed: 1}))
	assert.Equal("2 ok · 1 failed", panelStatusCell(terminal))
}
