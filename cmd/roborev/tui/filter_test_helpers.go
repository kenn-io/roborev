package tui

import (
	"testing"

	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/storage"
)

type testModelOption func(*model)

func withCurrentView(v viewKind) testModelOption {
	return func(m *model) { m.currentView = v }
}

func withTestJobs(jobs ...storage.ReviewJob) testModelOption {
	return func(m *model) { m.jobs = jobs }
}

func withSelection(idx int, jobID int64) testModelOption {
	return func(m *model) {
		m.selectedIdx = idx
		m.selectedJobID = jobID
	}
}

func withActiveRepoFilter(f []string) testModelOption {
	return func(m *model) { m.activeRepoFilter = f }
}

func withActiveBranchFilter(f string) testModelOption {
	return func(m *model) { m.activeBranchFilter = f }
}

func withFilterStack(f ...string) testModelOption {
	return func(m *model) { m.filterStack = f }
}

func withFilterSelectedIdx(idx int) testModelOption {
	return func(m *model) { m.filterSelectedIdx = idx }
}

func withFilterTree(nodes []treeFilterNode) testModelOption {
	return func(m *model) {
		m.filterTree = nodes
		m.rebuildFilterFlatList()
	}
}

func initTestModel(opts ...testModelOption) model {
	m := newModel(daemon.DaemonEndpoint{Network: "tcp", Address: "localhost"}, withExternalIODisabled())
	for _, opt := range opts {
		opt(&m)
	}
	return m
}

func assertSearch(t *testing.T, m model, expected string) {
	t.Helper()
	if m.filterSearch != expected {
		t.Errorf("Expected search=%q, got %q", expected, m.filterSearch)
	}
}

func countLoading(m *model) int {
	n := 0
	for _, node := range m.filterTree {
		if node.loading {
			n++
		}
	}
	return n
}

func countLoaded(m *model) int {
	n := 0
	for _, node := range m.filterTree {
		if node.children != nil {
			n++
		}
	}
	return n
}
