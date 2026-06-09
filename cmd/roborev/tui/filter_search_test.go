package tui

import (
	"errors"
	"fmt"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
)

func TestTUIFilterSearch(t *testing.T) {
	testNodes := []treeFilterNode{
		{name: "repo-alpha", count: 5},
		{name: "repo-beta", count: 3},
		{name: "something-else", count: 2},
	}

	cases := []struct {
		query         string
		expectedCount int
		description   string
	}{
		{"", 4, "No search (all visible)"},
		{"repo", 2, "Search 'repo'"},
		{"alpha", 1, "Search 'alpha'"},
		{"xyz", 0, "No matches"},
		{"all", 1, "Search 'all'"},
	}

	for _, tc := range cases {
		t.Run(tc.description, func(t *testing.T) {
			m := initFilterModel(testNodes)
			m.filterSearch = tc.query
			m.rebuildFilterFlatList()
			assert.Len(t, m.filterFlatList, tc.expectedCount)
			if tc.query == "all" && len(m.filterFlatList) > 0 {
				assert.Equal(t, -1, m.filterFlatList[0].repoIdx)
			}
		})
	}
}

func TestTUIFilterSearchSequential(t *testing.T) {
	m := initFilterModel([]treeFilterNode{
		makeNode("repo-alpha", 5),
		makeNode("repo-beta", 3),
		makeNode("other", 2),
	})

	assert.Len(t, m.filterFlatList, 4)

	m.filterSearch = "repo"
	m.rebuildFilterFlatList()
	assert.Len(t, m.filterFlatList, 2)

	m.filterSearch = "alpha"
	m.rebuildFilterFlatList()
	assert.Len(t, m.filterFlatList, 1)
	assert.Len(t, m.filterFlatList, 1, "expected one match")
	entry := m.filterFlatList[0]
	assert.NotEqual(t, -1, entry.repoIdx, "expected valid repo index")
	assert.Equal(t, "repo-alpha", m.filterTree[entry.repoIdx].name)

	m.filterSearch = ""
	m.rebuildFilterFlatList()
	assert.Len(t, m.filterFlatList, 4)
}

func TestTUIFilterTypingSearch(t *testing.T) {
	m := initTestModel(
		withCurrentView(viewFilter),
		withFilterTree([]treeFilterNode{makeNode("repo-a", 5)}),
		withFilterSelectedIdx(1),
	)

	steps := []struct {
		action func(m model) (model, tea.Cmd)
		assert func(t *testing.T, m model)
	}{
		{
			action: func(m model) (model, tea.Cmd) { return pressKey(m, 'a') },
			assert: func(t *testing.T, m model) {
				assertSearch(t, m, "a")
				assert.Equal(t, 0, m.filterSelectedIdx)
			},
		},
		{
			action: func(m model) (model, tea.Cmd) { return pressKey(m, 'b') },
			assert: func(t *testing.T, m model) { assertSearch(t, m, "ab") },
		},
		{
			action: func(m model) (model, tea.Cmd) { return pressSpecial(m, tea.KeyBackspace) },
			assert: func(t *testing.T, m model) { assertSearch(t, m, "a") },
		},
	}

	for _, step := range steps {
		m, _ = step.action(m)
		step.assert(t, m)
	}
}

func TestTUIFilterTypingHAndL(t *testing.T) {
	m := initFilterModel([]treeFilterNode{
		{
			name:      "highlight",
			rootPaths: []string{"/path/to/highlight"},
			count:     3,
			children:  []branchFilterItem{{name: "main", count: 3}},
		},
	})
	m.filterSelectedIdx = 1

	m2, _ := pressKey(m, 'h')
	assert.Equal(t, "h", m2.filterSearch)

	m3, _ := pressKey(m2, 'l')
	assert.Equal(t, "hl", m3.filterSearch)
}

// Regression test for issue #654: j, k, and q were swallowed by
// vim-style navigation/quit shortcuts instead of being appended to
// the search text.
func TestTUIFilterTypingJKQ(t *testing.T) {
	for _, r := range []rune{'j', 'k', 'q'} {
		t.Run(string(r), func(t *testing.T) {
			m := initFilterModel([]treeFilterNode{
				makeNode("kubernetes", 3),
				makeNode("jenkins", 2),
				makeNode("queue-worker", 1),
			})
			m.filterSelectedIdx = 1

			m2, _ := pressKey(m, r)
			assert.Equal(t, string(r), m2.filterSearch,
				"key %q should be appended to search", r)
			assert.Equal(t, viewFilter, m2.currentView,
				"key %q must not close the filter modal", r)
		})
	}
}

func TestTUIFilterSearchByRepoPath(t *testing.T) {
	m := initFilterModel([]treeFilterNode{
		{name: "backend", rootPaths: []string{"/path/to/backend-dev", "/path/to/backend-prod"}, count: 2},
		{name: "frontend", rootPaths: []string{"/path/to/frontend"}, count: 1},
	})

	m.filterSearch = "backend-dev"
	m.rebuildFilterFlatList()

	assert.Len(t, m.filterFlatList, 1)
	if len(m.filterFlatList) > 0 && m.filterFlatList[0].repoIdx != 0 {
		assert.Equal(t, 0, m.filterFlatList[0].repoIdx, "Expected to find 'backend' group at repoIdx 0")
	}
}

func TestTUIFilterSearchByDisplayName(t *testing.T) {
	m := initFilterModel([]treeFilterNode{
		{name: "My Project", rootPaths: []string{"/home/user/my-project-repo"}, count: 2},

		{name: "frontend", rootPaths: []string{"/path/to/frontend"}, count: 1},

		{name: "Backend Services", rootPaths: []string{"/srv/api-server", "/srv/worker-daemon"}, count: 2},
	})

	m.filterSearch = "my project"
	m.rebuildFilterFlatList()
	assert.Len(t, m.filterFlatList, 1)

	m.filterSearch = "my-project-repo"
	m.rebuildFilterFlatList()
	assert.Len(t, m.filterFlatList, 1)

	m.filterSearch = "backend"
	m.rebuildFilterFlatList()
	assert.Len(t, m.filterFlatList, 1)

	m.filterSearch = "api-server"
	m.rebuildFilterFlatList()
	assert.Len(t, m.filterFlatList, 1)
}

func TestTUIFilterBackspaceMultiByte(t *testing.T) {
	m := initTestModel(
		withCurrentView(viewFilter),
		withFilterTree([]treeFilterNode{makeNode("repo-a", 10)}),
	)

	steps := []struct {
		action func(m model) (model, tea.Cmd)
		assert func(t *testing.T, m model)
	}{
		{
			action: func(m model) (model, tea.Cmd) {
				m, _ = pressKey(m, 'a')
				m, _ = pressKeys(m, []rune("\xf0\x9f\x98\x8a"))
				return pressKey(m, 'b')
			},
			assert: func(t *testing.T, m model) { assertSearch(t, m, "a\xf0\x9f\x98\x8ab") },
		},
		{
			action: func(m model) (model, tea.Cmd) { return pressSpecial(m, tea.KeyBackspace) },
			assert: func(t *testing.T, m model) { assertSearch(t, m, "a\xf0\x9f\x98\x8a") },
		},
		{
			action: func(m model) (model, tea.Cmd) { return pressSpecial(m, tea.KeyBackspace) },
			assert: func(t *testing.T, m model) { assertSearch(t, m, "a") },
		},
	}

	for _, step := range steps {
		m, _ = step.action(m)
		step.assert(t, m)
	}
}

func TestTUIRightArrowDuringSearchLoad(t *testing.T) {
	m := initFilterModel([]treeFilterNode{
		{
			name:      "repo-a",
			rootPaths: []string{"/path/to/repo-a"},
			count:     5,
			loading:   true,
		},
	})
	m.filterSelectedIdx = 1

	m2, _ := pressSpecial(m, tea.KeyRight)

	assert.True(t, m2.filterTree[0].expanded)

	m3, _ := updateModel(t, m2, repoBranchesMsg{
		repoIdx:      0,
		rootPaths:    []string{"/path/to/repo-a"},
		branches:     []branchFilterItem{{name: "main", count: 3}},
		expandOnLoad: false,
	})

	assert.True(t, m3.filterTree[0].expanded)

	assert.Len(t, m3.filterFlatList, 3)
}

func TestTUISearchFetchProgressiveLoading(t *testing.T) {
	m := initFilterModel(nil)

	nodes := make([]treeFilterNode, 8)
	for i := range nodes {
		nodes[i] = treeFilterNode{
			name:      fmt.Sprintf("repo-%d", i),
			rootPaths: []string{fmt.Sprintf("/path/repo-%d", i)},
			count:     1,
		}
	}
	setupFilterTree(&m, nodes)
	m.filterSearch = "test"

	m.fetchUnloadedBranches()
	assert.Equal(t, maxSearchBranchFetches, countLoading(&m))

	for i := range 5 {
		assert.True(t, m.filterTree[i].loading)
	}
	for i := 5; i < 8; i++ {
		assert.False(t, m.filterTree[i].loading)
	}

	m2, cmd := updateModel(t, m, repoBranchesMsg{
		repoIdx:   0,
		rootPaths: []string{"/path/repo-0"},
		branches:  []branchFilterItem{{name: "main", count: 1}},
	})
	assert.NotNil(t, cmd)
	assert.Equal(t, maxSearchBranchFetches, countLoading(&m2))
	assert.True(t, m2.filterTree[5].loading)

	for i := 1; i < 8; i++ {
		if m2.filterTree[i].loading {
			m2, _ = updateModel(t, m2, repoBranchesMsg{
				repoIdx:   i,
				rootPaths: []string{fmt.Sprintf("/path/repo-%d", i)},
				branches:  []branchFilterItem{{name: "main", count: 1}},
			})
		}
	}
	assert.Equal(t, 8, countLoaded(&m2))
	assert.Equal(t, 0, countLoading(&m2))
}

func TestTUISearchFetchErrorNoRetryLoop(t *testing.T) {
	m := initFilterModel(nil)

	nodes := make([]treeFilterNode, 3)
	for i := range nodes {
		nodes[i] = treeFilterNode{
			name:      fmt.Sprintf("repo-%d", i),
			rootPaths: []string{fmt.Sprintf("/path/repo-%d", i)},
			count:     1,
		}
	}
	setupFilterTree(&m, nodes)
	m.filterSearch = "test"

	m.fetchUnloadedBranches()
	assert.Equal(t, 3, countLoading(&m))

	m2, cmd := updateModel(t, m, repoBranchesMsg{
		repoIdx:   0,
		rootPaths: []string{"/path/repo-0"},
		err:       errors.New("server error"),
	})

	assert.False(t, m2.filterTree[0].loading)
	assert.True(t, m2.filterTree[0].fetchFailed)

	assert.Nil(t, cmd)

	cmd2 := m2.fetchUnloadedBranches()
	assert.Nil(t, cmd2)

	m2.filterSelectedIdx = 1
	m3, cmd3 := pressSpecial(m2, tea.KeyRight)
	assert.True(t, m3.filterTree[0].loading)
	assert.False(t, m3.filterTree[0].fetchFailed)
	assert.NotNil(t, cmd3)
}

func TestTUIFetchFailedResetsOnSearchClear(t *testing.T) {
	m := initFilterModel([]treeFilterNode{
		{
			name:      "repo-a",
			rootPaths: []string{"/path/repo-a"},
			count:     5,
		},
	})

	m.filterTree[0].fetchFailed = true

	m.filterSearch = "test"
	cmd := m.fetchUnloadedBranches()
	assert.Nil(t, cmd)

	m.filterSearch = ""
	m.rebuildFilterFlatList()
	assert.False(t, m.filterTree[0].fetchFailed)

	m.filterSearch = "test"
	cmd2 := m.fetchUnloadedBranches()
	assert.NotNil(t, cmd2)
}

func TestTUILateErrorAfterSearchClear(t *testing.T) {
	m := initFilterModel([]treeFilterNode{
		{
			name:      "repo-a",
			rootPaths: []string{"/path/repo-a"},
			count:     5,
			loading:   true,
		},
	})

	m.filterSearch = ""
	m.rebuildFilterFlatList()

	m2, _ := updateModel(t, m, repoBranchesMsg{
		repoIdx:   0,
		rootPaths: []string{"/path/repo-a"},
		err:       errors.New("timeout"),
	})

	assert.False(t, m2.filterTree[0].fetchFailed)

	m2.filterSearch = "test"
	cmd := m2.fetchUnloadedBranches()
	assert.NotNil(t, cmd)
}

// TestTUIStaleSearchErrorIgnored verifies that an error from a
// previous search session does not set fetchFailed in the current
// session. Scenario: type "f" → clear → type "m" → old error
// arrives with stale searchSeq.
func TestTUIStaleSearchErrorIgnored(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.currentView = viewFilter
	setupFilterTree(&m, []treeFilterNode{
		{name: "repo-a", rootPaths: []string{"/a"}, count: 3},
		{name: "repo-b", rootPaths: []string{"/b"}, count: 2},
	})

	m, _ = pressKey(m, 'f')
	staleSeq := m.filterSearchSeq
	assert.Equal(t, 1, staleSeq)

	m.filterTree[0].loading = true

	m, _ = pressSpecial(m, tea.KeyBackspace)
	m, _ = pressKey(m, 'm')
	newSeq := m.filterSearchSeq
	assert.Equal(t, 3, newSeq)

	m, cmd := updateModel(t, m, repoBranchesMsg{
		repoIdx:   0,
		rootPaths: []string{"/a"},
		err:       fmt.Errorf("connection refused"),
		searchSeq: staleSeq,
	})

	assert.False(t, m.filterTree[0].fetchFailed, "fetchFailed should not be set by a stale search error")

	assert.NotNil(t, cmd)
}

// TestTUISearchBeforeReposLoad verifies that when the user types
// search text before repos have loaded, fetchUnloadedBranches is
// triggered once repos arrive via reposMsg.
func TestTUISearchBeforeReposLoad(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.currentView = viewFilter

	m, _ = pressKey(m, 'f')
	assert.Equal(t, "f", m.filterSearch)

	m2, cmd := updateModel(t, m, reposMsg{
		repos: []repoFilterItem{
			{name: "repo-a", rootPaths: []string{"/a"}, count: 3},
			{name: "repo-b", rootPaths: []string{"/b"}, count: 2},
		},
	})

	assert.Len(t, m2.filterTree, 2)

	assert.NotNil(t, cmd, "Expected fetchUnloadedBranches cmd after repos load with active search")

	anyLoading := false
	for _, node := range m2.filterTree {
		if node.loading {
			anyLoading = true
			break
		}
	}
	assert.True(t, anyLoading)
}

// TestTUISearchEditClearsFetchFailed verifies that changing search
// text (non-empty → non-empty) clears fetchFailed so previously
// failed repos are retried with the new search.
func TestTUISearchEditClearsFetchFailed(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.currentView = viewFilter
	setupFilterTree(&m, []treeFilterNode{
		{name: "repo-a", rootPaths: []string{"/a"}, count: 3},
		{name: "repo-b", rootPaths: []string{"/b"}, count: 2},
	})

	m, _ = pressKey(m, 'a')
	seqA := m.filterSearchSeq

	m, _ = updateModel(t, m, repoBranchesMsg{
		repoIdx:   0,
		rootPaths: []string{"/a"},
		err:       fmt.Errorf("timeout"),
		searchSeq: seqA,
	})
	assert.True(t, m.filterTree[0].fetchFailed, "Expected fetchFailed=true after error in current session")

	m, cmd := pressKey(m, 'b')

	assert.False(t, m.filterTree[0].fetchFailed)

	assert.NotNil(t, cmd)
}
