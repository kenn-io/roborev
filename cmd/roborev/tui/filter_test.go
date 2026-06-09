package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"

	"go.kenn.io/roborev/internal/storage"
)

// Helper function to initialize model for filter tests
func initFilterModel(nodes []treeFilterNode) model {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.currentView = viewFilter
	if nodes != nil {
		setupFilterTree(&m, nodes)
	}
	return m
}

// Helper function to create a treeFilterNode for tests
func makeNode(name string, count int) treeFilterNode {
	return treeFilterNode{
		name:      name,
		rootPaths: []string{"/path/to/" + name},
		count:     count,
	}
}

func TestTUIFilterOpenModal(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())

	m.jobs = []storage.ReviewJob{
		makeJob(1, withRepoName("repo-a")),
		makeJob(2, withRepoName("repo-b")),
		makeJob(3, withRepoName("repo-a")),
	}
	m.selectedIdx = 0
	m.selectedJobID = 1
	m.currentView = viewQueue

	m2, cmd := pressKey(m, 'f')

	assert.Equal(t, viewFilter, m2.currentView)

	assert.Nil(t, m2.filterTree)
	assert.Equal(t, 0, m2.filterSelectedIdx)
	assert.Empty(t, m2.filterSearch)
	assert.NotNil(t, cmd)
}

func TestTUIFilterReposMsg(t *testing.T) {
	m := initFilterModel(nil)

	repos := []repoFilterItem{
		{name: "repo-a", count: 2},
		{name: "repo-b", count: 1},
		{name: "repo-c", count: 1},
	}
	msg := reposMsg{repos: repos}

	m2, _ := updateModel(t, m, msg)

	assert.Len(t, m2.filterTree, 3)
	assert.False(t, m2.filterTree[0].name != "repo-a" || m2.filterTree[0].count != 2)
	assert.False(t, m2.filterTree[1].name != "repo-b" || m2.filterTree[1].count != 1)
	assert.False(t, m2.filterTree[2].name != "repo-c" || m2.filterTree[2].count != 1)

	assert.Len(t, m2.filterFlatList, 4)
}

func TestTUIFilterSelectRepo(t *testing.T) {
	m := initFilterModel([]treeFilterNode{
		makeNode("repo-a", 2),
		makeNode("repo-b", 1),
	})

	m.jobs = []storage.ReviewJob{
		makeJob(1, withRepoName("repo-a")),
		makeJob(2, withRepoName("repo-b")),
		makeJob(3, withRepoName("repo-a")),
	}

	m.filterSelectedIdx = 1

	m2, _ := pressSpecial(m, tea.KeyEnter)

	assert.Equal(t, viewQueue, m2.currentView)
	assert.False(t, len(m2.activeRepoFilter) != 1 || m2.activeRepoFilter[0] != "/path/to/repo-a")

	assert.Equal(t, -1, m2.selectedIdx)
}

func TestTUIFilterSelectAll(t *testing.T) {
	m := initFilterModel([]treeFilterNode{
		makeNode("repo-a", 2),
	})
	m.activeRepoFilter = []string{"/path/to/repo-a"}
	m.activeBranchFilter = "main"
	m.filterStack = []string{"repo", "branch"}
	m.filterSelectedIdx = 0

	m2, _ := pressSpecial(m, tea.KeyEnter)

	assert.Equal(t, viewQueue, m2.currentView)
	assert.Empty(t, m2.activeRepoFilter)
	assert.Empty(t, m2.activeBranchFilter)
	assert.Empty(t, m2.filterStack)
}

func TestTUIFilterPreselectsCurrent(t *testing.T) {
	m := initFilterModel(nil)
	m.activeRepoFilter = []string{"/path/to/repo-b"}

	repos := []repoFilterItem{
		{name: "repo-a", rootPaths: []string{"/path/to/repo-a"}, count: 1},
		{name: "repo-b", rootPaths: []string{"/path/to/repo-b"}, count: 1},
	}
	msg := reposMsg{repos: repos}

	m2, _ := updateModel(t, m, msg)

	assert.Equal(t, 2, m2.filterSelectedIdx)
}

func TestTUIFilterPreselectsMultiPathReordered(t *testing.T) {
	m := initFilterModel(nil)

	m.activeRepoFilter = []string{"/path/b", "/path/a"}

	repos := []repoFilterItem{
		{name: "other", rootPaths: []string{"/path/c"}, count: 1},
		{name: "multi", rootPaths: []string{"/path/a", "/path/b"}, count: 2},
	}
	m2, _ := updateModel(t, m, reposMsg{repos: repos})

	assert.Equal(t, 2, m2.filterSelectedIdx)
}

func TestTUIFilterAggregatedDisplayName(t *testing.T) {
	m := initFilterModel([]treeFilterNode{
		{name: "backend", rootPaths: []string{"/path/to/backend-dev", "/path/to/backend-prod"}, count: 2},
		{name: "frontend", rootPaths: []string{"/path/to/frontend"}, count: 1},
	})

	m.jobs = []storage.ReviewJob{
		makeJob(1, withRepoName("backend-dev"), withRepoPath("/path/to/backend-dev")),
		makeJob(2, withRepoName("backend-prod"), withRepoPath("/path/to/backend-prod")),
		makeJob(3, withRepoName("frontend"), withRepoPath("/path/to/frontend"), withStatus(storage.JobStatusFailed)),
	}

	m.filterSelectedIdx = 1

	m2, _ := pressSpecial(m, tea.KeyEnter)

	assert.Len(t, m2.activeRepoFilter, 2)

	assert.True(t, m2.repoMatchesFilter("/path/to/backend-dev"))
	assert.True(t, m2.repoMatchesFilter("/path/to/backend-prod"))

	assert.False(t, m2.repoMatchesFilter("/path/to/frontend"))
}

func TestTUIFilterViewSmallTerminal(t *testing.T) {
	m := initFilterModel([]treeFilterNode{
		makeNode("repo-a", 5),
		makeNode("repo-b", 3),
		makeNode("repo-c", 2),
	})

	m.filterSelectedIdx = 0

	t.Run("tiny terminal shows message", func(t *testing.T) {
		m.height = 5
		output := m.renderFilterView()

		assert.Contains(t, output, "(terminal too small)")

		assert.NotContains(t, output, "repo-a")
	})

	t.Run("exactly reservedLines shows no items", func(t *testing.T) {
		m.height = 7
		output := m.renderFilterView()

		assert.Contains(t, output, "(terminal too small)")
	})

	t.Run("one row available", func(t *testing.T) {
		m.height = 8
		output := m.renderFilterView()

		assert.NotContains(t, output, "(terminal too small)")

		assert.Contains(t, output, "All")

		assert.Contains(t, output, "[showing 1-1 of 4]")
	})

	t.Run("fits all items without scroll", func(t *testing.T) {
		m.height = 15
		output := m.renderFilterView()

		assert.Contains(t, output, "All")
		assert.Contains(t, output, "repo-a")
		assert.Contains(t, output, "repo-c")

		assert.NotContains(t, output, "[showing")
	})

	t.Run("needs scrolling shows scroll info", func(t *testing.T) {
		m.height = 9
		m.filterSelectedIdx = 2
		output := m.renderFilterView()

		assert.Contains(t, output, "[showing")

		assert.Contains(t, output, "repo-b")
	})
}

func TestTUIFilterViewScrollWindow(t *testing.T) {
	m := initFilterModel([]treeFilterNode{
		makeNode("repo-1", 5),
		makeNode("repo-2", 4),
		makeNode("repo-3", 3),
		makeNode("repo-4", 2),
		makeNode("repo-5", 1),
	})

	m.height = 10

	t.Run("scroll keeps selected item visible at top", func(t *testing.T) {
		m.filterSelectedIdx = 0
		output := m.renderFilterView()

		assert.Contains(t, output, "[showing 1-3 of 6]")
	})

	t.Run("scroll keeps selected item visible at bottom", func(t *testing.T) {
		m.filterSelectedIdx = 5
		output := m.renderFilterView()

		assert.Contains(t, output, "[showing 4-6 of 6]")
		assert.Contains(t, output, "repo-5")
	})

	t.Run("scroll centers selected item in middle", func(t *testing.T) {
		m.filterSelectedIdx = 3
		output := m.renderFilterView()

		assert.Contains(t, output, "repo-3")
	})
}

func TestTUIFilterLoadingRendersPaddedHeight(t *testing.T) {
	m := initFilterModel(nil)
	m.width = 100
	m.height = 20

	output := m.View().Content

	lines := strings.Split(output, "\n")

	assert.GreaterOrEqual(t, len(lines), m.height-3)

	assert.Contains(t, output, "Loading repos...")
}

func TestTUIRightArrowRetriesAfterFailedLoad(t *testing.T) {
	m := initFilterModel([]treeFilterNode{
		{
			name:      "repo-a",
			rootPaths: []string{"/path/to/repo-a"},
			count:     5,

			expanded: true,
			loading:  false,
		},
	})
	m.filterSelectedIdx = 1

	m2, cmd := pressSpecial(m, tea.KeyRight)

	assert.True(t, m2.filterTree[0].loading)
	assert.NotNil(t, cmd)
}

func TestTUIWindowResizeLoadsMoreWhenMultiRepoFiltered(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())

	m.jobs = []storage.ReviewJob{makeJob(1, withRepoPath("/repo1"))}
	m.hasMore = true
	m.loadingMore = false
	m.loadingJobs = false
	m.activeRepoFilter = []string{"/repo1", "/repo2"}
	m.currentView = viewQueue
	m.height = 10
	m.heightDetected = false

	m2, cmd := updateModel(t, m, tea.WindowSizeMsg{Width: 120, Height: 50})

	assert.False(t, m2.loadingMore)
	assert.True(t, m2.loadingJobs)
	assert.Equal(t, 50, m2.height)
	assert.True(t, m2.heightDetected)
	assert.NotNil(t, cmd)
}

func TestTUIBKeyFallsBackToFirstRepo(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.currentView = viewFilter
	m.filterBranchMode = true

	repos := []repoFilterItem{
		{name: "repo-a", rootPaths: []string{"/path/to/repo-a"}, count: 3},
		{name: "repo-b", rootPaths: []string{"/path/to/repo-b"}, count: 2},
	}
	msg := reposMsg{repos: repos}

	m2, cmd := updateModel(t, m, msg)

	assert.True(t, m2.filterTree[0].loading)
	assert.NotNil(t, cmd)
}

func TestTUIBKeyUsesActiveRepoFilter(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.currentView = viewFilter
	m.filterBranchMode = true
	m.activeRepoFilter = []string{"/path/to/repo-b"}
	m.cwdRepoRoot = "/path/to/repo-a"

	repos := []repoFilterItem{
		{name: "repo-a", rootPaths: []string{"/path/to/repo-a"}, count: 3},
		{name: "repo-b", rootPaths: []string{"/path/to/repo-b"}, count: 2},
	}
	msg := reposMsg{repos: repos}

	m2, cmd := updateModel(t, m, msg)

	assert.True(t, m2.filterTree[1].loading)
	assert.Equal(t, "repo-b", m2.filterTree[1].name)
	assert.NotNil(t, cmd)
}

func TestTUIBKeyUsesMultiPathActiveRepoFilter(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.currentView = viewFilter
	m.filterBranchMode = true
	m.activeRepoFilter = []string{"/path/a", "/path/b"}
	m.cwdRepoRoot = "/path/to/other"

	repos := []repoFilterItem{
		{name: "other", rootPaths: []string{"/path/to/other"}, count: 1},
		{name: "multi-root", rootPaths: []string{"/path/a", "/path/b"}, count: 5},
	}
	msg := reposMsg{repos: repos}

	m2, cmd := updateModel(t, m, msg)

	assert.True(t, m2.filterTree[1].loading)
	assert.Equal(t, "multi-root", m2.filterTree[1].name)
	assert.NotNil(t, cmd)
}

func TestTUIFilterOpenSkipsBackfillWhenDone(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.currentView = viewQueue
	m.branchBackfillDone = true
	m.jobs = []storage.ReviewJob{makeJob(1)}
	m.selectedIdx = 0

	m2, cmd := pressKey(m, 'f')

	assert.Equal(t, viewFilter, m2.currentView)
	assert.NotNil(t, cmd)
}
