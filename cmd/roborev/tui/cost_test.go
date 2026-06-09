package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/roborev/internal/storage"
)

func TestCostSegmentText(t *testing.T) {
	assert := assert.New(t)

	var m model
	_, show := m.costSegmentText()
	assert.False(show, "nil cost hides segment")

	m.cost = &storage.CostAggregate{}
	_, show = m.costSegmentText()
	assert.False(show, "zero eligible hides segment")

	m.cost = &storage.CostAggregate{TotalUSD: 1.23, JobsWithCost: 18, JobsTotal: 40}
	text, show := m.costSegmentText()
	assert.True(show)
	assert.Equal("~$1.23 (18/40)", text)

	m.cost = &storage.CostAggregate{TotalUSD: 1.23, JobsWithCost: 5, JobsTotal: 5, Complete: true}
	text, show = m.costSegmentText()
	assert.True(show)
	assert.Equal("~$1.23", text)
}

func TestHandleCostMsgStaleGuard(t *testing.T) {
	assert := assert.New(t)

	m := model{fetchSeq: 5}
	fresh := &storage.CostAggregate{TotalUSD: 2.0, JobsWithCost: 1, JobsTotal: 1}

	updated, _ := m.handleCostMsg(costMsg{cost: fresh, seq: 5})
	assert.Equal(fresh, updated.(model).cost)

	// Stale response from an earlier filter is discarded.
	stale := &storage.CostAggregate{TotalUSD: 99.0, JobsWithCost: 9, JobsTotal: 9}
	updated, _ = updated.(model).handleCostMsg(costMsg{cost: stale, seq: 4})
	assert.Equal(fresh, updated.(model).cost, "stale seq must not overwrite")
}

// TestCostSegmentHiddenAfterFilterChange covers the window between a filter
// change and the arrival of the refreshed cost: a jobsMsg for the new filter may
// already have rendered, but the stored cost still describes the old scope, so
// the segment must hide until the matching cost response lands.
func TestCostSegmentHiddenAfterFilterChange(t *testing.T) {
	assert := assert.New(t)

	// Cost fetched for the current filter generation (seq 2) shows.
	m := model{fetchSeq: 2}
	updated, _ := m.handleCostMsg(costMsg{
		cost: &storage.CostAggregate{TotalUSD: 1.50, JobsWithCost: 1, JobsTotal: 2},
		seq:  2,
	})
	m = updated.(model)
	text, show := m.costSegmentText()
	assert.True(show)
	assert.Equal("~$1.50 (1/2)", text)

	// A filter change bumps fetchSeq; the prior scope's cost must hide.
	m.fetchSeq = 3
	_, show = m.costSegmentText()
	assert.False(show, "stale cost hidden after filter change")

	// The refreshed cost for the new generation restores the segment.
	updated, _ = m.handleCostMsg(costMsg{
		cost: &storage.CostAggregate{TotalUSD: 0.75, JobsWithCost: 1, JobsTotal: 1, Complete: true},
		seq:  3,
	})
	m = updated.(model)
	text, show = m.costSegmentText()
	assert.True(show)
	assert.Equal("~$0.75", text)
}
