package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"

	"go.kenn.io/roborev/internal/storage"
)

func TestTUIFilterClearWithEsc(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())

	m.jobs = []storage.ReviewJob{
		makeJob(1, withRepoName("repo-a"), withRepoPath("/path/to/repo-a")),
		makeJob(2, withRepoName("repo-b"), withRepoPath("/path/to/repo-b")),
	}
	m.selectedIdx = 0
	m.selectedJobID = 1
	m.currentView = viewQueue
	m.activeRepoFilter = []string{"/path/to/repo-a"}
	m.filterStack = []string{"repo"}

	m2, _ := pressSpecial(m, tea.KeyEscape)

	assert.Empty(t, m2.activeRepoFilter)

	assert.Equal(t, -1, m2.selectedIdx)
}

func TestTUIFilterClearWithEscLayered(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())

	m.jobs = []storage.ReviewJob{
		makeJob(1, withRepoName("repo-a"), withRepoPath("/path/to/repo-a")),
		makeJob(2, withRepoName("repo-b"), withRepoPath("/path/to/repo-b")),
	}
	m.selectedIdx = 0
	m.selectedJobID = 1
	m.currentView = viewQueue
	m.activeRepoFilter = []string{"/path/to/repo-a"}
	m.filterStack = []string{"repo"}
	m.hideClosed = true

	m2, _ := pressSpecial(m, tea.KeyEscape)

	assert.Empty(t, m2.activeRepoFilter)
	assert.True(t, m2.hideClosed)

	m3, _ := pressSpecial(m2, tea.KeyEscape)

	assert.False(t, m3.hideClosed)
}

func TestTUIFilterClearHideClosedOnly(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())

	m.jobs = []storage.ReviewJob{
		makeJob(1, withRepoName("repo-a")),
	}
	m.selectedIdx = 0
	m.selectedJobID = 1
	m.currentView = viewQueue
	m.hideClosed = true

	m2, _ := pressSpecial(m, tea.KeyEscape)

	assert.False(t, m2.hideClosed)
	assert.Equal(t, -1, m2.selectedIdx)
}

func TestTUIFilterEscapeWhileLoadingFiresNewFetch(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())

	m.jobs = []storage.ReviewJob{
		makeJob(1, withRepoName("repo-a"), withRepoPath("/path/to/repo-a")),
	}
	m.selectedIdx = 0
	m.selectedJobID = 1
	m.currentView = viewQueue
	m.activeRepoFilter = []string{"/path/to/repo-a"}
	m.filterStack = []string{"repo"}
	m.loadingJobs = true
	oldSeq := m.fetchSeq

	m2, cmd := pressSpecial(m, tea.KeyEscape)

	assert.Empty(t, m2.activeRepoFilter)
	assert.Greater(t, m2.fetchSeq, oldSeq)
	assert.NotNil(t, cmd)

	m3, _ := updateModel(t, m2, jobsMsg{jobs: []storage.ReviewJob{makeJob(2)}, hasMore: false, seq: oldSeq})

	assert.True(t, m3.loadingJobs)
}

func TestTUIFilterEscapeWhilePaginationDiscardsAppend(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())

	m.jobs = []storage.ReviewJob{
		makeJob(1, withRepoName("repo-a"), withRepoPath("/path/to/repo-a")),
	}
	m.selectedIdx = 0
	m.selectedJobID = 1
	m.currentView = viewQueue
	m.activeRepoFilter = []string{"/path/to/repo-a"}
	m.filterStack = []string{"repo"}
	m.loadingMore = true
	m.loadingJobs = false
	oldSeq := m.fetchSeq

	m2, cmd := pressSpecial(m, tea.KeyEscape)

	assert.Empty(t, m2.activeRepoFilter)
	assert.Greater(t, m2.fetchSeq, oldSeq)
	assert.NotNil(t, cmd)

	m3, _ := updateModel(t, m2, jobsMsg{
		jobs:    []storage.ReviewJob{makeJob(99, withRepoName("stale"))},
		hasMore: true,
		append:  true,
		seq:     oldSeq,
	})

	for _, job := range m3.jobs {
		assert.NotEqual(t, 99, job.ID)
	}
}

func TestTUIFilterEscapeCloses(t *testing.T) {
	m := initFilterModel([]treeFilterNode{
		makeNode("repo-a", 1),
	})
	m.filterSearch = "test"

	m2, _ := pressSpecial(m, tea.KeyEscape)

	assert.Equal(t, viewQueue, m2.currentView)
	assert.Empty(t, m2.filterSearch)
}

func TestTUIFilterStackPush(t *testing.T) {
	m := initFilterModel(nil)

	m.pushFilter("repo")
	assert.False(t, len(m.filterStack) != 1 || m.filterStack[0] != "repo")

	m.pushFilter("branch")
	assert.False(t, len(m.filterStack) != 2 || m.filterStack[0] != "repo" || m.filterStack[1] != "branch")
}

func TestTUIFilterStackPushMovesDuplicate(t *testing.T) {
	m := initFilterModel(nil)

	m.pushFilter("repo")
	m.pushFilter("branch")

	m.pushFilter("repo")
	assert.False(t, len(m.filterStack) != 2 || m.filterStack[0] != "branch" || m.filterStack[1] != "repo")
}

func TestTUIFilterStackPopClearsValue(t *testing.T) {
	m := initFilterModel(nil)

	m.activeRepoFilter = []string{"/path/to/repo"}
	m.activeBranchFilter = "main"
	m.filterStack = []string{"repo", "branch"}

	popped := m.popFilter()
	assert.Equal(t, "branch", popped)
	assert.Empty(t, m.activeBranchFilter)
	assert.Len(t, m.activeRepoFilter, 1)

	popped = m.popFilter()
	assert.Equal(t, "repo", popped)
	assert.Empty(t, m.activeRepoFilter)

	popped = m.popFilter()
	assert.Empty(t, popped)
}

func TestTUIFilterStackEscapeOrder(t *testing.T) {
	m := initTestModel(
		withTestJobs(makeJob(1, withRepoName("repo-a"), withRepoPath("/path/to/repo-a"), withBranch("main"))),
		withSelection(0, 1),
		withCurrentView(viewQueue),
		withActiveRepoFilter([]string{"/path/to/repo-a"}),
		withActiveBranchFilter("main"),
		withFilterStack("repo", "branch"),
	)

	steps := []struct {
		action func(m model) (model, tea.Cmd)
		assert func(t *testing.T, m model)
	}{
		{
			action: func(m model) (model, tea.Cmd) { return pressSpecial(m, tea.KeyEscape) },
			assert: func(t *testing.T, m model) {
				assert.Empty(t, m.activeBranchFilter)
				assert.NotEmpty(t, m.activeRepoFilter)
				assert.False(t, len(m.filterStack) != 1 || m.filterStack[0] != "repo")
			},
		},
		{
			action: func(m model) (model, tea.Cmd) { return pressSpecial(m, tea.KeyEscape) },
			assert: func(t *testing.T, m model) {
				assert.Empty(t, m.activeRepoFilter)
				assert.Empty(t, m.filterStack)
			},
		},
	}

	for _, step := range steps {
		m, _ = step.action(m)
		step.assert(t, m)
	}
}

func TestTUIFilterStackTitleBarOrder(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())

	m.jobs = []storage.ReviewJob{
		makeJob(1, withRepoName("myrepo"), withRepoPath("/path/to/myrepo"), withBranch("feature")),
	}
	m.currentView = viewQueue

	m.activeBranchFilter = "feature"
	m.filterStack = []string{"branch"}
	m.activeRepoFilter = []string{"/path/to/myrepo"}
	m.filterStack = append(m.filterStack, "repo")

	output := m.View().Content

	assert.Contains(t, output, "[b: feature]")
	assert.Contains(t, output, "[f: myrepo]")

	bIdx := strings.Index(output, "[b: feature]")
	fIdx := strings.Index(output, "[f: myrepo]")
	assert.LessOrEqual(t, bIdx, fIdx)
}

func TestTUIFilterStackTitleUsesRepoDisplayName(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.currentView = viewQueue
	m.activeRepoFilter = []string{"/workspace/old-service"}
	m.filterStack = []string{"repo"}
	m.repoNames = map[string][]string{
		"new-service": {"/workspace/old-service"},
	}

	output := m.View().Content

	assert.Contains(t, output, "[f: new-service]")
	assert.NotContains(t, output, "[f: old-service]")
}

func TestTUIFilterStackReverseOrder(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())

	m.jobs = []storage.ReviewJob{
		makeJob(1, withRepoName("myrepo"), withRepoPath("/path/to/myrepo"), withBranch("develop")),
	}
	m.currentView = viewQueue

	m.activeRepoFilter = []string{"/path/to/myrepo"}
	m.filterStack = []string{"repo"}
	m.activeBranchFilter = "develop"
	m.filterStack = append(m.filterStack, "branch")

	output := m.View().Content

	fIdx := strings.Index(output, "[f: myrepo]")
	bIdx := strings.Index(output, "[b: develop]")
	assert.LessOrEqual(t, fIdx, bIdx)
}

func TestTUIRemoveFilterFromStack(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())

	m.filterStack = []string{"repo", "branch", "other"}

	m.removeFilterFromStack("branch")
	assert.False(t, len(m.filterStack) != 2 || m.filterStack[0] != "repo" || m.filterStack[1] != "other")

	m.removeFilterFromStack("nonexistent")
	assert.Len(t, m.filterStack, 2)
}

// TestTUIReconnectClearsFetchFailed verifies that a successful
// daemon reconnect clears fetchFailed and retriggers branch
// fetches when search is active.
func TestTUIReconnectClearsFetchFailed(t *testing.T) {
	m := newModel(testEndpointFromURL("http://localhost:7373"), withExternalIODisabled())
	m.currentView = viewFilter
	setupFilterTree(&m, []treeFilterNode{
		{name: "repo-a", rootPaths: []string{"/a"}, count: 3},
	})

	m.filterSearch = "test"
	m.filterTree[0].fetchFailed = true

	m2, cmd := updateModel(t, m, reconnectMsg{
		endpoint: testEndpointFromURL("http://localhost:7374"),
	})

	assert.False(t, m2.filterTree[0].fetchFailed)
	assert.NotNil(t, cmd, "Expected commands after reconnect")

	assert.True(t, m2.filterTree[0].loading)

	m3 := newModel(testEndpointFromURL("http://localhost:7373"), withExternalIODisabled())
	m3.currentView = viewFilter
	setupFilterTree(&m3, []treeFilterNode{
		{name: "repo-b", rootPaths: []string{"/b"}, count: 2},
	})
	m3.filterTree[0].fetchFailed = true

	m4, _ := updateModel(t, m3, reconnectMsg{
		endpoint: testEndpointFromURL("http://localhost:7374"),
	})

	assert.False(t, m4.filterTree[0].fetchFailed)

	assert.False(t, m4.filterTree[0].loading)
}

func TestTUILockedFilterModalBlocksAll(t *testing.T) {
	nodes := []treeFilterNode{
		makeNode("repo-a", 3),
	}
	m := initFilterModel(nodes)
	m.activeRepoFilter = []string{"/locked/repo"}
	m.activeBranchFilter = "locked-branch"
	m.filterStack = []string{"repo", "branch"}
	m.lockedRepoFilter = true
	m.lockedBranchFilter = true

	m.filterSelectedIdx = 0
	m2, _ := pressKey(m, '\r')

	assert.NotNil(t, m2.activeRepoFilter)
	assert.NotEmpty(t, m2.activeBranchFilter)
}

func TestTUIPopFilterSkipsLockedWalksBack(t *testing.T) {
	m := initFilterModel(nil)
	m.activeRepoFilter = []string{"/unlocked/repo"}
	m.activeBranchFilter = "locked-branch"
	m.filterStack = []string{"repo", "branch"}
	m.lockedBranchFilter = true

	popped := m.popFilter()
	assert.Equal(t, "repo", popped)
	assert.Nil(t, m.activeRepoFilter)
	assert.Equal(t, "locked-branch", m.activeBranchFilter)

	assert.False(t, len(m.filterStack) != 1 || m.filterStack[0] != "branch")
}

func TestTUIPopFilterAllLocked(t *testing.T) {
	m := initFilterModel(nil)
	m.activeRepoFilter = []string{"/locked/repo"}
	m.activeBranchFilter = "locked-branch"
	m.filterStack = []string{"repo", "branch"}
	m.lockedRepoFilter = true
	m.lockedBranchFilter = true

	popped := m.popFilter()
	assert.Empty(t, popped)
	assert.Len(t, m.filterStack, 2)
}

func TestTUIEscapeWithLockedFilters(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.currentView = viewQueue
	m.jobs = []storage.ReviewJob{
		makeJob(1, withRepoName("r"), withRepoPath("/r"), withBranch("b")),
	}
	m.selectedIdx = 0
	m.selectedJobID = 1
	m.activeRepoFilter = []string{"/r"}
	m.activeBranchFilter = "b"
	m.filterStack = []string{"repo", "branch"}
	m.lockedBranchFilter = true

	m2, _ := pressSpecial(m, tea.KeyEscape)
	assert.Equal(t, "b", m2.activeBranchFilter)
	assert.Nil(t, m2.activeRepoFilter)

	m3, _ := pressSpecial(m2, tea.KeyEscape)
	assert.Equal(t, "b", m3.activeBranchFilter)
}
