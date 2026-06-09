package tui

import (
	"errors"
	"fmt"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
)

// setupFilterTree is a helper that sets filterTree and rebuilds the flat list.
func setupFilterTree(m *model, nodes []treeFilterNode) {
	m.filterTree = nodes
	m.rebuildFilterFlatList()
}

func TestTUITreeFilterExpandCollapse(t *testing.T) {
	m := initFilterModel([]treeFilterNode{
		{
			name:      "repo-a",
			rootPaths: []string{"/path/to/repo-a"},
			count:     5,
			children: []branchFilterItem{
				{name: "main", count: 3},
				{name: "feature", count: 2},
			},
		},
		makeNode("repo-b", 3),
	})

	assert.Len(t, m.filterFlatList, 3)

	m.filterSelectedIdx = 1
	m2, _ := pressSpecial(m, tea.KeyRight)

	assert.True(t, m2.filterTree[0].expanded)

	assert.Len(t, m2.filterFlatList, 5)

	m2.filterSelectedIdx = 2
	m3, _ := pressSpecial(m2, tea.KeyLeft)

	assert.False(t, m3.filterTree[0].expanded)

	assert.Len(t, m3.filterFlatList, 3)

	assert.Equal(t, 1, m3.filterSelectedIdx)
}

func TestTUITreeFilterSelectBranch(t *testing.T) {
	m := initFilterModel([]treeFilterNode{
		{
			name:      "repo-a",
			rootPaths: []string{"/path/to/repo-a"},
			count:     5,
			expanded:  true,
			children: []branchFilterItem{
				{name: "main", count: 3},
				{name: "feature", count: 2},
			},
		},
	})

	m.filterSelectedIdx = 3
	m.jobs = []storage.ReviewJob{
		makeJob(1, withBranch("main")),
		makeJob(2, withBranch("feature")),
	}

	m2, cmd := pressSpecial(m, tea.KeyEnter)

	assert.Equal(t, viewQueue, m2.currentView)
	assert.False(t, len(m2.activeRepoFilter) != 1 || m2.activeRepoFilter[0] != "/path/to/repo-a")
	assert.Equal(t, "feature", m2.activeBranchFilter)

	foundRepo := false
	foundBranch := false
	for _, f := range m2.filterStack {
		if f == "repo" {
			foundRepo = true
		}
		if f == "branch" {
			foundBranch = true
		}
	}
	assert.True(t, foundRepo)
	assert.True(t, foundBranch)
	assert.NotNil(t, cmd)
}

func TestTUITreeFilterLazyLoadBranches(t *testing.T) {
	m := initFilterModel([]treeFilterNode{
		makeNode("repo-a", 5),
	})

	m.filterSelectedIdx = 1

	m2, cmd := pressSpecial(m, tea.KeyRight)

	assert.True(t, m2.filterTree[0].loading)
	assert.NotNil(t, cmd)

	m3, _ := updateModel(t, m2, repoBranchesMsg{
		repoIdx:      0,
		rootPaths:    []string{"/path/to/repo-a"},
		branches:     []branchFilterItem{{name: "main", count: 3}, {name: "dev", count: 2}},
		expandOnLoad: true,
	})

	assert.False(t, m3.filterTree[0].loading)
	assert.True(t, m3.filterTree[0].expanded)
	assert.Len(t, m3.filterTree[0].children, 2)

	assert.Len(t, m3.filterFlatList, 4)
}

func TestTUITreeFilterBranchFetchFailureClearsLoading(t *testing.T) {
	m := initFilterModel([]treeFilterNode{
		{
			name:      "repo-a",
			rootPaths: []string{"/path/to/repo-a"},
			count:     5,
			loading:   true,
		},
	})
	m.filterSelectedIdx = 1

	m2, _ := updateModel(t, m, repoBranchesMsg{
		repoIdx:   0,
		rootPaths: []string{"/path/to/repo-a"},
		err:       errors.New("connection refused"),
	})

	assert.False(t, m2.filterTree[0].loading)
	assert.False(t, m2.filterTree[0].expanded)
	require.Error(t, m2.err)
}

func TestTUITreeFilterBranchFetchFailureOutOfView(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.currentView = viewQueue
	m.filterBranchMode = true
	setupFilterTree(&m, []treeFilterNode{
		{
			name:      "repo-a",
			rootPaths: []string{"/path/to/repo-a"},
			count:     5,
			loading:   true,
		},
	})

	m2, _ := updateModel(t, m, repoBranchesMsg{
		repoIdx:   0,
		rootPaths: []string{"/path/to/repo-a"},
		err:       errors.New("server error"),
	})

	require.Error(t, m2.err)

	assert.False(t, m2.filterTree[0].loading)

	assert.False(t, m2.filterBranchMode)
}

func TestTUITreeFilterBranchFetchConnectionErrorTriggersReconnect(t *testing.T) {
	m := initFilterModel([]treeFilterNode{
		{
			name:      "repo-a",
			rootPaths: []string{"/path/to/repo-a"},
			count:     5,
			loading:   true,
		},
	})
	m.consecutiveErrors = 2

	m2, cmd := updateModel(t, m, repoBranchesMsg{
		repoIdx:   0,
		rootPaths: []string{"/path/to/repo-a"},
		err:       mockConnError("connection refused"),
	})

	assert.Equal(t, 3, m2.consecutiveErrors)
	assert.True(t, m2.reconnecting)
	assert.NotNil(t, cmd)
	assert.False(t, m2.filterTree[0].loading)
}

func TestTUITreeFilterSearchTriggersLazyBranchLoad(t *testing.T) {
	m := initFilterModel([]treeFilterNode{
		{
			name:      "repo-a",
			rootPaths: []string{"/path/to/repo-a"},
			count:     5,
		},
		{
			name:      "repo-b",
			rootPaths: []string{"/path/to/repo-b"},
			count:     3,
			children:  []branchFilterItem{{name: "main", count: 3}},
		},
	})

	m2, cmd := pressKey(m, 'f')

	assert.True(t, m2.filterTree[0].loading)
	assert.False(t, m2.filterTree[1].loading)
	assert.NotNil(t, cmd)
}

func TestTUITreeFilterSearchExpandsMatchingBranches(t *testing.T) {
	m := initFilterModel([]treeFilterNode{
		{
			name:      "repo-a",
			rootPaths: []string{"/path/to/repo-a"},
			count:     5,
			children: []branchFilterItem{
				{name: "main", count: 3},
				{name: "feature-xyz", count: 2},
			},
		},
		{
			name:      "repo-b",
			rootPaths: []string{"/path/to/repo-b"},
			count:     3,
			children: []branchFilterItem{
				{name: "main", count: 3},
			},
		},
	})

	m.filterSearch = "xyz"
	m.rebuildFilterFlatList()

	assert.Len(t, m.filterFlatList, 2)
	if len(m.filterFlatList) >= 2 {
		assert.False(t, m.filterFlatList[0].repoIdx != 0 || m.filterFlatList[0].branchIdx != -1)
		assert.False(t, m.filterFlatList[1].repoIdx != 0 || m.filterFlatList[1].branchIdx != 1)
	}
}

func TestTUISearchTriggeredLoadDoesNotExpand(t *testing.T) {
	m := initFilterModel([]treeFilterNode{
		makeNode("repo-a", 5),
	})

	m2, _ := updateModel(t, m, repoBranchesMsg{
		repoIdx:      0,
		rootPaths:    []string{"/path/to/repo-a"},
		branches:     []branchFilterItem{{name: "main", count: 3}},
		expandOnLoad: false,
	})

	assert.False(t, m2.filterTree[0].expanded)

	m2.filterSearch = "main"
	m2.rebuildFilterFlatList()

	assert.Len(t, m2.filterFlatList, 2)

	m2.filterSearch = ""
	m2.rebuildFilterFlatList()

	assert.Len(t, m2.filterFlatList, 2)
}

func TestTUILeftArrowCollapsesDuringSearch(t *testing.T) {
	m := initFilterModel([]treeFilterNode{
		{
			name:      "repo-a",
			rootPaths: []string{"/path/to/repo-a"},
			count:     5,
			children: []branchFilterItem{
				{name: "feature-xyz", count: 3},
				{name: "main", count: 2},
			},
		},
	})

	m.filterSearch = "xyz"
	m.rebuildFilterFlatList()
	assert.Len(t, m.filterFlatList, 2)

	m.filterSelectedIdx = 0
	m2, _ := pressSpecial(m, tea.KeyLeft)

	assert.True(t, m2.filterTree[0].userCollapsed)

	assert.Len(t, m2.filterFlatList, 1)

	m2.filterSelectedIdx = 0
	m3, _ := pressSpecial(m2, tea.KeyRight)

	assert.False(t, m3.filterTree[0].userCollapsed)
	assert.True(t, m3.filterTree[0].expanded)
}

func TestTUIUserCollapsedResetsWhenSearchClears(t *testing.T) {
	m := initFilterModel([]treeFilterNode{
		{
			name:      "repo-a",
			rootPaths: []string{"/path/to/repo-a"},
			count:     5,
			children: []branchFilterItem{
				{name: "feature-xyz", count: 3},
			},
		},
	})

	m.filterSearch = "xyz"
	m.filterTree[0].userCollapsed = true
	m.rebuildFilterFlatList()

	assert.Len(t, m.filterFlatList, 1)

	m.filterSearch = ""
	m.rebuildFilterFlatList()
	assert.False(t, m.filterTree[0].userCollapsed)
}

func TestRootPathsMatchOrderIndependent(t *testing.T) {
	tests := []struct {
		name  string
		a, b  []string
		match bool
	}{
		{"both nil", nil, nil, true},
		{"both empty", []string{}, []string{}, true},
		{"single equal", []string{"/a"}, []string{"/a"}, true},
		{"single differ", []string{"/a"}, []string{"/b"}, false},
		{"same order", []string{"/a", "/b"}, []string{"/a", "/b"}, true},
		{"diff order", []string{"/b", "/a"}, []string{"/a", "/b"}, true},
		{"diff length", []string{"/a"}, []string{"/a", "/b"}, false},
		{
			"three paths reordered",
			[]string{"/c", "/a", "/b"},
			[]string{"/a", "/b", "/c"},
			true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := rootPathsMatch(tt.a, tt.b)
			assert.Equal(t, got, tt.match)
		})
	}
}

func TestTUIBranchResponseReorderedRootPaths(t *testing.T) {
	m := initFilterModel([]treeFilterNode{
		{
			name:      "multi-root",
			rootPaths: []string{"/path/b", "/path/a"},
			count:     5,
			loading:   true,
		},
	})

	m2, _ := updateModel(t, m, repoBranchesMsg{
		repoIdx:      0,
		rootPaths:    []string{"/path/a", "/path/b"},
		branches:     []branchFilterItem{{name: "main", count: 5}},
		expandOnLoad: true,
	})

	assert.False(t, m2.filterTree[0].loading)
	assert.Len(t, m2.filterTree[0].children, 1)
	assert.True(t, m2.filterTree[0].expanded)

	m3 := initFilterModel([]treeFilterNode{
		{
			name:      "other-repo",
			rootPaths: []string{"/path/c"},
			count:     3,
			loading:   true,
		},
	})

	m4, _ := updateModel(t, m3, repoBranchesMsg{
		repoIdx:      0,
		rootPaths:    []string{"/path/d"},
		branches:     []branchFilterItem{{name: "main", count: 3}},
		expandOnLoad: true,
	})

	assert.True(t, m4.filterTree[0].loading)
	assert.Nil(t, m4.filterTree[0].children)
}

func TestTUIFetchUnloadedBranchesCapped(t *testing.T) {
	m := initFilterModel(nil)

	nodes := make([]treeFilterNode, 10)
	for i := range nodes {
		nodes[i] = treeFilterNode{
			name:      fmt.Sprintf("repo-%d", i),
			rootPaths: []string{fmt.Sprintf("/path/repo-%d", i)},
			count:     1,
		}
	}
	setupFilterTree(&m, nodes)

	m.filterSearch = "test"
	cmd := m.fetchUnloadedBranches()
	assert.NotNil(t, cmd, "Expected command from fetchUnloadedBranches")

	loading := countLoading(&m)
	assert.Equal(t, maxSearchBranchFetches, loading)

	cmd2 := m.fetchUnloadedBranches()
	assert.Nil(t, cmd2)
	assert.Equal(t, maxSearchBranchFetches, countLoading(&m))
}

func TestTUIManualExpandFailureDoesNotBlockSearch(t *testing.T) {
	m := initFilterModel([]treeFilterNode{
		{
			name:      "repo-a",
			rootPaths: []string{"/path/repo-a"},
			count:     5,
			loading:   true,
		},
	})

	m2, _ := updateModel(t, m, repoBranchesMsg{
		repoIdx:      0,
		rootPaths:    []string{"/path/repo-a"},
		err:          errors.New("connection refused"),
		expandOnLoad: true,
	})

	assert.False(t, m2.filterTree[0].fetchFailed)

	m2.filterSearch = "test"
	cmd := m2.fetchUnloadedBranches()
	assert.NotNil(t, cmd)
	assert.True(t, m2.filterTree[0].loading)
}

func TestTUITreeFilterSelectBranchTriggersRefetch(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.currentView = viewFilter
	setupFilterTree(&m, []treeFilterNode{
		{
			name:      "repo-a",
			rootPaths: []string{"/path/to/repo-a"},
			count:     8,
			expanded:  true,
			children: []branchFilterItem{
				{name: "main", count: 5},
				{name: "feature", count: 3},
			},
		},
	})

	m.filterSelectedIdx = 3
	m.jobs = []storage.ReviewJob{
		makeJob(1, withBranch("main")),
		makeJob(2, withBranch("feature")),
	}
	m.loadingJobs = false

	m2, cmd := pressSpecial(m, tea.KeyEnter)

	assert.True(t, m2.loadingJobs)
	assert.NotNil(t, cmd)
}

func TestTUIBranchBackfillDoneSetWhenNoNullsRemain(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.branchBackfillDone = false

	m2, _ := updateModel(t, m, branchesMsg{
		backfillCount: 0,
	})

	assert.True(t, m2.branchBackfillDone)
}

func TestTUIBranchBackfillDoneSetEvenWhenNullsRemain(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.branchBackfillDone = false

	m2, _ := updateModel(t, m, branchesMsg{
		backfillCount: 2,
	})

	assert.True(t, m2.branchBackfillDone)
}

func TestTUIBranchBackfillIsOneTimeOperation(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.branchBackfillDone = false

	m2, _ := updateModel(t, m, branchesMsg{
		backfillCount: 1,
	})

	assert.True(t, m2.branchBackfillDone)

	m3, _ := updateModel(t, m2, branchesMsg{
		backfillCount: 0,
	})

	assert.True(t, m3.branchBackfillDone)
}

func TestTUIBranchBackfillDoneStaysTrueAfterNewJobs(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.branchBackfillDone = true

	m2, _ := updateModel(t, m, branchesMsg{
		backfillCount: 0,
	})

	assert.True(t, m2.branchBackfillDone)
}

func TestTUITreeFilterCollapseOnExpandedRepo(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.currentView = viewFilter
	setupFilterTree(&m, []treeFilterNode{
		{
			name:      "repo-a",
			rootPaths: []string{"/path/to/repo-a"},
			count:     5,
			expanded:  true,
			children: []branchFilterItem{
				{name: "main", count: 3},
				{name: "feature", count: 2},
			},
		},
	})

	m.filterSelectedIdx = 1

	m2, _ := pressSpecial(m, tea.KeyLeft)

	assert.False(t, m2.filterTree[0].expanded)

	assert.Len(t, m2.filterFlatList, 2)
}

func TestTUIBKeyAutoExpandsCwdRepo(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.currentView = viewFilter
	m.filterBranchMode = true
	m.cwdRepoRoot = "/path/to/repo-b"

	repos := []repoFilterItem{
		{name: "repo-a", rootPaths: []string{"/path/to/repo-a"}, count: 3},
		{name: "repo-b", rootPaths: []string{"/path/to/repo-b"}, count: 2},
		{name: "repo-c", rootPaths: []string{"/path/to/repo-c"}, count: 1},
	}
	msg := reposMsg{repos: repos}

	m2, cmd := updateModel(t, m, msg)

	assert.True(t, m2.filterTree[0].loading)
	assert.Equal(t, "repo-b", m2.filterTree[0].name)
	assert.NotNil(t, cmd)
}

func TestTUIBKeyPositionsCursorOnBranch(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.currentView = viewFilter
	m.filterBranchMode = true
	setupFilterTree(&m, []treeFilterNode{
		{name: "repo-a", rootPaths: []string{"/path/to/repo-a"}, count: 5},
	})

	msg := repoBranchesMsg{
		repoIdx:      0,
		rootPaths:    []string{"/path/to/repo-a"},
		branches:     []branchFilterItem{{name: "main", count: 3}, {name: "dev", count: 2}},
		expandOnLoad: true,
	}

	m2, _ := updateModel(t, m, msg)

	assert.False(t, m2.filterBranchMode)

	assert.Equal(t, 2, m2.filterSelectedIdx)
	assert.False(t, len(m2.filterFlatList) > 2 && m2.filterFlatList[2].branchIdx != 0)
}

func TestTUIBKeyEscapeClearsBranchMode(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.currentView = viewFilter
	m.filterBranchMode = true
	setupFilterTree(&m, []treeFilterNode{
		{name: "repo-a", count: 1},
	})

	m2, _ := pressSpecial(m, tea.KeyEscape)

	assert.False(t, m2.filterBranchMode)
	assert.Equal(t, viewQueue, m2.currentView)
}

func TestTUIFilterCwdBranchSortsFirst(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.currentView = viewFilter
	m.cwdRepoRoot = "/path/to/repo-a"
	m.cwdBranch = "feature"

	setupFilterTree(&m, []treeFilterNode{
		{name: "repo-a", rootPaths: []string{"/path/to/repo-a"}, count: 5},
	})

	msg := repoBranchesMsg{
		repoIdx:   0,
		rootPaths: []string{"/path/to/repo-a"},
		branches: []branchFilterItem{
			{name: "main", count: 3},
			{name: "develop", count: 1},
			{name: "feature", count: 1},
		},
	}

	m2, _ := updateModel(t, m, msg)

	children := m2.filterTree[0].children
	assert.Len(t, children, 3)
	assert.Equal(t, "feature", children[0].name)
	assert.Equal(t, "main", children[1].name)
	assert.Equal(t, "develop", children[2].name)
}

func TestTUIFilterEnterClearsBranchMode(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.currentView = viewFilter
	m.filterBranchMode = true
	setupFilterTree(&m, []treeFilterNode{
		{
			name:      "repo-a",
			rootPaths: []string{"/path/to/repo-a"},
			count:     5,
			children:  []branchFilterItem{{name: "main", count: 3}},
		},
	})

	m.filterSelectedIdx = 1

	m2, _ := pressSpecial(m, tea.KeyEnter)

	assert.False(t, m2.filterBranchMode)
	assert.Equal(t, viewQueue, m2.currentView)
}

func TestTUILockedBranchPreservedOnRepoSelect(t *testing.T) {
	nodes := []treeFilterNode{
		makeNode("repo-a", 3),
	}
	m := initFilterModel(nodes)
	m.activeBranchFilter = "locked-branch"
	m.filterStack = []string{"branch"}
	m.lockedBranchFilter = true

	m.filterSelectedIdx = 1
	m2, _ := pressKey(m, '\r')
	assert.Equal(t, "locked-branch", m2.activeBranchFilter,

		"Locked branch filter changed: got %q, want %q",
		m2.activeBranchFilter, "locked-branch")
}

func TestTUILockedRepoPreservedOnBranchSelect(t *testing.T) {
	node := makeNode("repo-a", 2)
	node.children = []branchFilterItem{
		{name: "feature-x", count: 2},
	}
	node.expanded = true
	m := initFilterModel([]treeFilterNode{node})
	m.activeRepoFilter = []string{"/locked/repo"}
	m.filterStack = []string{"repo"}
	m.lockedRepoFilter = true

	m.filterSelectedIdx = 1
	m2, _ := pressKey(m, '\r')
	assert.False(t, len(m2.activeRepoFilter) != 1 ||
		m2.activeRepoFilter[0] != "/locked/repo",

		"Locked repo filter changed: got %v, want [/locked/repo]",
		m2.activeRepoFilter)
}
