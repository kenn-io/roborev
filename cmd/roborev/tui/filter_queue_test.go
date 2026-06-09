package tui

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
)

func TestTUIFilterNavigation(t *testing.T) {
	cases := []struct {
		startIdx    int
		key         rune
		expectedIdx int
		description string
	}{
		{0, tea.KeyDown, 1, "Navigate down from 0"},
		{1, tea.KeyDown, 2, "Navigate down from 1"},
		{2, tea.KeyDown, 2, "Navigate down at boundary"},
		{2, tea.KeyUp, 1, "Navigate up from 2"},
	}

	for _, tc := range cases {
		t.Run(tc.description, func(t *testing.T) {
			m := initFilterModel([]treeFilterNode{
				makeNode("repo-a", 5),
				makeNode("repo-b", 3),
			})
			m.filterSelectedIdx = tc.startIdx

			m2, _ := pressSpecial(m, tc.key)
			assert.Equal(t, tc.expectedIdx, m2.filterSelectedIdx)
		})
	}
}

func TestTUIFilterNavigationSequential(t *testing.T) {
	m := initFilterModel([]treeFilterNode{
		makeNode("repo-a", 1),
		makeNode("repo-b", 1),
		makeNode("repo-c", 1),
	})

	keys := []rune{tea.KeyDown, tea.KeyDown, tea.KeyDown, tea.KeyUp}

	m2 := m
	for _, k := range keys {
		m2, _ = pressSpecial(m2, k)
	}

	assert.Equal(t, 2, m2.filterSelectedIdx)
}

func TestTUIFilterToZeroVisibleJobs(t *testing.T) {
	m := initFilterModel([]treeFilterNode{
		makeNode("repo-a", 2),
		makeNode("repo-b", 0),
	})

	m.jobs = []storage.ReviewJob{
		makeJob(1, withRepoName("repo-a"), withRepoPath("/path/to/repo-a")),
		makeJob(2, withRepoName("repo-a"), withRepoPath("/path/to/repo-a")),
	}
	m.selectedIdx = 0
	m.selectedJobID = 1

	m.filterSelectedIdx = 2

	m2, cmd := pressSpecial(m, tea.KeyEnter)

	assert.False(t, len(m2.activeRepoFilter) != 1 || m2.activeRepoFilter[0] != "/path/to/repo-b")
	assert.NotNil(t, cmd)

	assert.Equal(t, -1, m2.selectedIdx)
	assert.EqualValues(t, 0, m2.selectedJobID)

	m3, _ := updateModel(t, m2, jobsMsg{jobs: []storage.ReviewJob{}})

	assert.Equal(t, -1, m3.selectedIdx)
	assert.EqualValues(t, 0, m3.selectedJobID)
}

func TestTUIFilterChangeFetchesFirstPageOnly(t *testing.T) {
	var gotLimit string
	var gotRepo string
	_, m := mockServerModel(t, func(w http.ResponseWriter, r *http.Request) {
		gotLimit = r.URL.Query().Get("limit")
		gotRepo = r.URL.Query().Get("repo")
		assert.NoError(t, json.NewEncoder(w).Encode(jobsPageResult{}))
	})
	m.currentView = viewFilter
	m.heightDetected = true
	m.height = 40
	setupFilterTree(&m, []treeFilterNode{
		{
			name:      "kata",
			rootPaths: []string{"/workspace/kata"},
			count:     1429,
		},
	})
	m.filterSelectedIdx = 1
	m.jobs = make([]storage.ReviewJob, 1429)
	m.loadingJobs = false

	m2, cmd := pressSpecial(m, tea.KeyEnter)
	require.NotNil(t, cmd)
	msg := cmd()
	_, ok := msg.(jobsMsg)
	require.True(t, ok)

	assert.Empty(t, m2.jobs)
	assert.Equal(t, "/workspace/kata", gotRepo)
	assert.Equal(t, strconv.Itoa(m2.queueVisibleRows()+queuePrefetchBuffer), gotLimit)
}

func TestTUIMultiPathFilterStatusCounts(t *testing.T) {
	// A display name spanning multiple repos is scoped server-side via an IN
	// clause and paginated, so the status counts come from the server
	// aggregate (jobStats), not from counting the loaded page. The loaded
	// page below is deliberately smaller and would yield different counts if
	// the line still tallied client-side.
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.height = 20
	m.daemonVersion = "test"

	addrTrue := true
	addrFalse := false

	m.jobs = []storage.ReviewJob{
		{ID: 1, RepoPath: "/path/to/backend-dev", Status: storage.JobStatusDone, Closed: &addrTrue},
		{ID: 2, RepoPath: "/path/to/backend-prod", Status: storage.JobStatusDone, Closed: &addrFalse},
	}
	m.jobStats = storage.JobStats{Done: 3, Closed: 1, Open: 2}
	m.activeRepoFilter = []string{"/path/to/backend-dev", "/path/to/backend-prod"}

	output := m.renderQueueView()

	assert.Contains(t, output, "Completed: 3")
	assert.Contains(t, output, "Closed: 1")
	assert.Contains(t, output, "Open: 2")
}

func TestTUIBranchFilterApplied(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())

	m.jobs = []storage.ReviewJob{
		makeJob(1, withRepoName("repo-a"), withBranch("main")),
		makeJob(2, withRepoName("repo-a"), withBranch("feature")),
		makeJob(3, withRepoName("repo-b"), withBranch("main")),
		{ID: 4, RepoName: "repo-b", Branch: ""},
	}
	m.selectedIdx = 0
	m.selectedJobID = 1
	m.currentView = viewQueue

	m.activeBranchFilter = "main"

	visible := m.getVisibleJobs()
	assert.Len(t, visible, 2)
	for _, job := range visible {
		assert.Equal(t, "main", job.Branch)
	}
}

func TestTUIBranchFilterNone(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())

	m.jobs = []storage.ReviewJob{
		makeJob(1, withRepoName("repo-a"), withBranch("main")),
		{ID: 2, RepoName: "repo-a", Branch: ""},
		{ID: 3, RepoName: "repo-b", Branch: ""},
	}
	m.selectedIdx = 0
	m.selectedJobID = 1
	m.currentView = viewQueue

	m.activeBranchFilter = "(none)"

	visible := m.getVisibleJobs()
	assert.Len(t, visible, 2)
	for _, job := range visible {
		assert.Empty(t, job.Branch)
	}
}

func TestTUIBranchFilterCombinedWithRepoFilter(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())

	m.jobs = []storage.ReviewJob{
		makeJob(1, withRepoName("repo-a"), withRepoPath("/path/to/repo-a"), withBranch("main")),
		makeJob(2, withRepoName("repo-a"), withRepoPath("/path/to/repo-a"), withBranch("feature")),
		makeJob(3, withRepoName("repo-b"), withRepoPath("/path/to/repo-b"), withBranch("main")),
		makeJob(4, withRepoName("repo-b"), withRepoPath("/path/to/repo-b"), withBranch("feature")),
	}
	m.selectedIdx = 0
	m.selectedJobID = 1
	m.currentView = viewQueue

	m.activeRepoFilter = []string{"/path/to/repo-a"}
	m.activeBranchFilter = "main"

	visible := m.getVisibleJobs()
	assert.Len(t, visible, 1)
	assert.False(t, len(visible) > 0 && (visible[0].RepoPath != "/path/to/repo-a" || visible[0].Branch != "main"))
}

func TestTUINavigateDownLoadsMoreWhenBranchFiltered(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())

	m.jobs = []storage.ReviewJob{makeJob(1, withBranch("feature"))}
	m.selectedIdx = 0
	m.selectedJobID = 1
	m.hasMore = true
	m.loadingMore = false
	m.loadingJobs = false
	m.activeBranchFilter = "feature"
	m.currentView = viewQueue

	m2, cmd := pressSpecial(m, tea.KeyDown)

	assert.True(t, m2.loadingMore)
	assert.NotNil(t, cmd)
}

func TestTUINavigateJKeyLoadsMoreWhenBranchFiltered(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())

	m.jobs = []storage.ReviewJob{makeJob(1, withBranch("feature"))}
	m.selectedIdx = 0
	m.selectedJobID = 1
	m.hasMore = true
	m.loadingMore = false
	m.loadingJobs = false
	m.activeBranchFilter = "feature"
	m.currentView = viewQueue

	m2, cmd := pressKey(m, 'j')

	assert.True(t, m2.loadingMore)
	assert.NotNil(t, cmd)
}

func TestTUIPageDownLoadsMoreWhenBranchFiltered(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())

	m.jobs = []storage.ReviewJob{makeJob(1, withBranch("feature"))}
	m.selectedIdx = 0
	m.selectedJobID = 1
	m.hasMore = true
	m.loadingMore = false
	m.loadingJobs = false
	m.activeBranchFilter = "feature"
	m.currentView = viewQueue
	m.height = 20

	m2, cmd := pressSpecial(m, tea.KeyPgDown)

	assert.True(t, m2.loadingMore)
	assert.NotNil(t, cmd)
}

func TestTUIBranchFilterClearTriggersRefetch(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())

	m.currentView = viewQueue
	m.activeBranchFilter = "feature"
	m.filterStack = []string{"branch"}
	m.jobs = []storage.ReviewJob{makeJob(1, withBranch("feature"))}
	m.loadingJobs = false

	m2, cmd := pressSpecial(m, tea.KeyEscape)

	assert.Empty(t, m2.activeBranchFilter)
	assert.True(t, m2.loadingJobs)
	assert.NotNil(t, cmd)
}

func TestTUIQueueNavigationWithFilter(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())

	m.jobs = []storage.ReviewJob{
		makeJob(1, withRepoName("repo-a"), withRepoPath("/path/to/repo-a")),
		makeJob(2, withRepoName("repo-b"), withRepoPath("/path/to/repo-b")),
		makeJob(3, withRepoName("repo-a"), withRepoPath("/path/to/repo-a")),
		makeJob(4, withRepoName("repo-b"), withRepoPath("/path/to/repo-b")),
		makeJob(5, withRepoName("repo-a"), withRepoPath("/path/to/repo-a")),
	}
	m.selectedIdx = 0
	m.selectedJobID = 1
	m.currentView = viewQueue
	m.activeRepoFilter = []string{"/path/to/repo-a"}

	m2, _ := pressKey(m, 'j')

	assert.Equal(t, 2, m2.selectedIdx)
	assert.EqualValues(t, 3, m2.selectedJobID)

	m3, _ := pressKey(m2, 'j')

	assert.Equal(t, 4, m3.selectedIdx)
	assert.EqualValues(t, 5, m3.selectedJobID)

	m4, _ := pressKey(m3, 'k')

	assert.Equal(t, 2, m4.selectedIdx)
}

func TestTUIJobsRefreshWithFilter(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())

	m.jobs = []storage.ReviewJob{
		makeJob(1, withRepoName("repo-a"), withRepoPath("/path/to/repo-a")),
		makeJob(2, withRepoName("repo-b"), withRepoPath("/path/to/repo-b")),
		makeJob(3, withRepoName("repo-a"), withRepoPath("/path/to/repo-a")),
	}
	m.selectedIdx = 2
	m.selectedJobID = 3
	m.activeRepoFilter = []string{"/path/to/repo-a"}

	newJobs := jobsMsg{jobs: []storage.ReviewJob{
		makeJob(1, withRepoName("repo-a"), withRepoPath("/path/to/repo-a")),
		makeJob(2, withRepoName("repo-b"), withRepoPath("/path/to/repo-b")),
		makeJob(3, withRepoName("repo-a"), withRepoPath("/path/to/repo-a")),
	}}

	m2, _ := updateModel(t, m, newJobs)

	assert.Equal(t, 2, m2.selectedIdx)
	assert.EqualValues(t, 3, m2.selectedJobID)

	newJobs = jobsMsg{jobs: []storage.ReviewJob{
		makeJob(1, withRepoName("repo-a"), withRepoPath("/path/to/repo-a")),
		makeJob(2, withRepoName("repo-b"), withRepoPath("/path/to/repo-b")),
	}}

	m3, _ := updateModel(t, m2, newJobs)

	assert.Equal(t, 0, m3.selectedIdx)
	assert.EqualValues(t, 1, m3.selectedJobID)
}

func TestTUIRefreshWithZeroVisibleJobs(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())

	m.jobs = []storage.ReviewJob{
		makeJob(1, withRepoName("repo-a"), withRepoPath("/path/to/repo-a")),
	}
	m.activeRepoFilter = []string{"/path/to/repo-b"}
	m.selectedIdx = 0
	m.selectedJobID = 1

	newJobs := []storage.ReviewJob{
		makeJob(1, withRepoName("repo-a"), withRepoPath("/path/to/repo-a")),
		makeJob(2, withRepoName("repo-a"), withRepoPath("/path/to/repo-a")),
	}
	m2, _ := updateModel(t, m, jobsMsg{jobs: newJobs})

	assert.Equal(t, -1, m2.selectedIdx)
	assert.EqualValues(t, 0, m2.selectedJobID)
}

func TestTUIActionsNoOpWithZeroVisibleJobs(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())

	m.jobs = []storage.ReviewJob{
		makeJob(1, withRepoName("repo-a"), withRepoPath("/path/to/repo-a")),
	}
	m.activeRepoFilter = []string{"/path/to/repo-b"}
	m.selectedIdx = -1
	m.selectedJobID = 0
	m.currentView = viewQueue

	m2, cmd := pressSpecial(m, tea.KeyEnter)
	assert.Nil(t, cmd)
	assert.Equal(t, viewQueue, m2.currentView)

	_, cmd = pressKey(m, 'x')
	assert.Nil(t, cmd)

	_, cmd = pressKey(m, 'a')
	assert.Nil(t, cmd)
}

func TestTUIBKeyOpensBranchFilter(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.currentView = viewQueue
	m.jobs = []storage.ReviewJob{makeJob(1, withRepoName("repo-a"))}
	m.selectedIdx = 0

	m2, cmd := pressKey(m, 'b')

	assert.Equal(t, viewFilter, m2.currentView)
	assert.True(t, m2.filterBranchMode)
	assert.NotNil(t, cmd)
}

func TestTUIFilterOpenBatchesBackfill(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.currentView = viewQueue
	m.branchBackfillDone = false
	m.jobs = []storage.ReviewJob{makeJob(1)}
	m.selectedIdx = 0

	m2, cmd := pressKey(m, 'f')

	assert.Equal(t, viewFilter, m2.currentView)
	assert.NotNil(t, cmd)
}

func TestTUIFilterCwdRepoSortsFirst(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.currentView = viewFilter
	m.cwdRepoRoot = "/path/to/repo-b"

	repos := []repoFilterItem{
		{name: "repo-a", rootPaths: []string{"/path/to/repo-a"}, count: 3},
		{name: "repo-b", rootPaths: []string{"/path/to/repo-b"}, count: 2},
		{name: "repo-c", rootPaths: []string{"/path/to/repo-c"}, count: 1},
	}
	msg := reposMsg{repos: repos}

	m2, _ := updateModel(t, m, msg)

	assert.Len(t, m2.filterTree, 3)
	assert.Equal(t, "repo-b", m2.filterTree[0].name)
	assert.Equal(t, "repo-a", m2.filterTree[1].name)
	assert.Equal(t, "repo-c", m2.filterTree[2].name)
}

func TestTUIFilterNoCwdNoReorder(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.currentView = viewFilter

	repos := []repoFilterItem{
		{name: "repo-a", rootPaths: []string{"/path/to/repo-a"}, count: 3},
		{name: "repo-b", rootPaths: []string{"/path/to/repo-b"}, count: 2},
		{name: "repo-c", rootPaths: []string{"/path/to/repo-c"}, count: 1},
	}
	msg := reposMsg{repos: repos}

	m2, _ := updateModel(t, m, msg)

	assert.Equal(t, "repo-a", m2.filterTree[0].name)
	assert.Equal(t, "repo-b", m2.filterTree[1].name)
	assert.Equal(t, "repo-c", m2.filterTree[2].name)
}

func TestTUIAutoRepoFilterUsesRenamedRepoDisplayName(t *testing.T) {
	oldRoot := "/workspace/old-service"
	newRoot := "/workspace/new-service"
	m := newModel(localhostEndpoint, withExternalIODisabled(), withAutoFilterRepo(newRoot))
	m.currentView = viewQueue
	m.loadingJobs = false

	m2, cmd := updateModel(t, m, repoNamesMsg{
		names: map[string][]string{
			"new-service": {oldRoot},
		},
	})

	assert.Equal(t, []string{oldRoot}, m2.activeRepoFilter)
	assert.NotNil(t, cmd)
}

func TestTUIAutoRepoFilterFallsBackToIdentity(t *testing.T) {
	oldRoot := "/workspace/old-service"
	newRoot := "/workspace/cool-rebrand"
	identity := "https://github.com/test/service.git"
	m := newModel(
		localhostEndpoint,
		withExternalIODisabled(),
		withAutoFilterRepo(newRoot),
		withCwdRepoIdentity(identity),
	)
	m.currentView = viewQueue
	m.loadingJobs = false

	m2, cmd := updateModel(t, m, repoNamesMsg{
		names: map[string][]string{
			"old-service": {oldRoot},
		},
		identities: map[string][]string{
			identity: {oldRoot},
		},
	})

	assert.Equal(t, []string{oldRoot}, m2.activeRepoFilter)
	assert.NotNil(t, cmd)
}

func TestTUIAutoRepoFilterPrefersIdentityOverDisplayName(t *testing.T) {
	expectedRoot := "/workspace/team-a/service"
	wrongRoot := "/workspace/team-b/service"
	newRoot := "/workspace/team-c/service"
	identity := "https://github.com/test/team-a-service.git"
	m := newModel(
		localhostEndpoint,
		withExternalIODisabled(),
		withAutoFilterRepo(newRoot),
		withCwdRepoIdentity(identity),
	)
	m.currentView = viewQueue
	m.loadingJobs = false

	m2, cmd := updateModel(t, m, repoNamesMsg{
		names: map[string][]string{
			"service": {wrongRoot},
		},
		identities: map[string][]string{
			identity: {expectedRoot},
		},
	})

	assert.Equal(t, []string{expectedRoot}, m2.activeRepoFilter)
	assert.NotNil(t, cmd)
}

func TestTUIAutoRepoFilterKeepsTrackedCloneWithSharedIdentity(t *testing.T) {
	currentRoot := "/workspace/team-a/service"
	otherRoot := "/workspace/team-b/service"
	identity := "https://github.com/test/service.git"
	m := newModel(
		localhostEndpoint,
		withExternalIODisabled(),
		withAutoFilterRepo(currentRoot),
		withCwdRepoIdentity(identity),
	)
	m.currentView = viewQueue
	m.loadingJobs = false

	m2, cmd := updateModel(t, m, repoNamesMsg{
		names: map[string][]string{
			"service": {currentRoot, otherRoot},
		},
		identities: map[string][]string{
			identity: {currentRoot, otherRoot},
		},
	})

	assert.Equal(t, []string{currentRoot}, m2.activeRepoFilter)
	assert.Nil(t, cmd)
	assert.False(t, m2.loadingJobs)
}

func TestTUIBKeyNoOpOutsideQueue(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.currentView = viewReview

	m2, cmd := pressKey(m, 'b')

	assert.Equal(t, viewReview, m2.currentView)
	assert.False(t, m2.filterBranchMode)
	assert.Nil(t, cmd)
}
