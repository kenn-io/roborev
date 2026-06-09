package tui

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/version"
)

// testServerAddr is a placeholder address used in tests that don't make real HTTP calls.
// Tests that need actual HTTP should use httptest.NewServer and pass ts.URL.
const testServerAddr = "http://test.invalid:9999"

// testEndpoint is a DaemonEndpoint parsed from testServerAddr.
var testEndpoint = testEndpointFromURL(testServerAddr)

// localhostEndpoint is a convenience DaemonEndpoint for tests using "http://localhost".
var localhostEndpoint = daemon.DaemonEndpoint{Network: "tcp", Address: "localhost"}

// testEndpointFromURL parses a URL string into a DaemonEndpoint for tests.
// It strips the http:// prefix and uses the host:port as the TCP address.
func testEndpointFromURL(rawURL string) daemon.DaemonEndpoint {
	ep, err := daemon.ParseEndpoint(rawURL)
	if err != nil {
		// Fallback for test URLs that ParseEndpoint may reject
		// (e.g. non-loopback hosts like test.invalid)
		addr := strings.TrimPrefix(rawURL, "https://")
		addr = strings.TrimPrefix(addr, "http://")
		return daemon.DaemonEndpoint{Network: "tcp", Address: addr}
	}
	return ep
}

// setupTuiTestEnv isolates the test from the production roborev environment
// by setting ROBOREV_DATA_DIR to a temp directory. This prevents tests from
// reading the production daemon runtime store or affecting production state.
func setupTuiTestEnv(t *testing.T) string {
	t.Helper()
	tmpDir := t.TempDir()
	origDataDir := os.Getenv("ROBOREV_DATA_DIR")
	os.Setenv("ROBOREV_DATA_DIR", tmpDir)
	t.Cleanup(func() {
		if origDataDir != "" {
			os.Setenv("ROBOREV_DATA_DIR", origDataDir)
		} else {
			os.Unsetenv("ROBOREV_DATA_DIR")
		}
	})
	return tmpDir
}

// mockConnError creates a connection error (url.Error) for testing
func mockConnError(msg string) error {
	return &url.Error{Op: "Get", URL: testServerAddr, Err: errors.New(msg)}
}

// stripANSI removes ANSI escape sequences from a string
var ansiRegex = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

func stripANSI(s string) string {
	return ansiRegex.ReplaceAllString(s, "")
}

// mockServerModel creates an httptest.Server and a model pointed at it.
func mockServerModel(t *testing.T, handler http.HandlerFunc) (*httptest.Server, model) {
	t.Helper()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return ts, newModel(testEndpointFromURL(ts.URL), withExternalIODisabled())
}

// pressKey simulates pressing a rune key and returns the updated model.
func pressKey(m model, r rune) (model, tea.Cmd) {
	updated, cmd := m.Update(keyPressMsg(r))
	return updated.(model), cmd
}

// pressKeys simulates pressing multiple rune keys.
func pressKeys(m model, runes []rune) (model, tea.Cmd) {
	updated, cmd := m.Update(keyPressTextMsg(string(runes)))
	return updated.(model), cmd
}

// pressSpecial simulates pressing a special key (Enter, Escape, etc.).
func pressSpecial(m model, key rune) (model, tea.Cmd) {
	updated, cmd := m.Update(keySpecialMsg(key))
	return updated.(model), cmd
}

func pressCtrl(m model, key rune) (model, tea.Cmd) {
	updated, cmd := m.Update(ctrlKeyMsg(key))
	return updated.(model), cmd
}

func keyPressMsg(r rune) tea.KeyPressMsg {
	return keyPressTextMsg(string(r))
}

func keyPressTextMsg(text string) tea.KeyPressMsg {
	key := tea.Key{Text: text}
	runes := []rune(text)
	if len(runes) == 1 {
		key.Code = runes[0]
		if key.Code == ' ' {
			key.Code = tea.KeySpace
		}
	} else {
		key.Code = tea.KeyExtended
	}
	return tea.KeyPressMsg(key)
}

func keySpecialMsg(key rune) tea.KeyPressMsg {
	return tea.KeyPressMsg(tea.Key{Code: key})
}

func ctrlKeyMsg(key rune) tea.KeyPressMsg {
	return tea.KeyPressMsg(tea.Key{Code: key, Mod: tea.ModCtrl})
}

// updateModel sends a message to the model and returns the updated model.
func updateModel(t *testing.T, m model, msg tea.Msg) (model, tea.Cmd) {
	t.Helper()
	updated, cmd := m.Update(msg)
	newModel, ok := updated.(model)
	if !ok {
		require.Condition(t, func() bool {
			return false
		}, "Model type assertion failed: got %T", updated)
	}
	return newModel, cmd
}

// makeJob creates a storage.ReviewJob with the given ID and sensible defaults.
// Optional functional options can override specific fields.
func makeJob(id int64, opts ...func(*storage.ReviewJob)) storage.ReviewJob {
	j := storage.ReviewJob{ID: id, Status: storage.JobStatusDone}
	for _, opt := range opts {
		opt(&j)
	}
	return j
}

func withStatus(s storage.JobStatus) func(*storage.ReviewJob) {
	return func(j *storage.ReviewJob) { j.Status = s }
}

func withRef(ref string) func(*storage.ReviewJob) {
	return func(j *storage.ReviewJob) { j.GitRef = ref }
}

func withAgent(agent string) func(*storage.ReviewJob) {
	return func(j *storage.ReviewJob) { j.Agent = agent }
}

func withBranch(branch string) func(*storage.ReviewJob) {
	return func(j *storage.ReviewJob) { j.Branch = branch }
}

func withClosed(b *bool) func(*storage.ReviewJob) {
	return func(j *storage.ReviewJob) { j.Closed = b }
}

func withRepoPath(path string) func(*storage.ReviewJob) {
	return func(j *storage.ReviewJob) { j.RepoPath = path }
}

func withRepoName(name string) func(*storage.ReviewJob) {
	return func(j *storage.ReviewJob) { j.RepoName = name }
}

func withEnqueuedAt(t time.Time) func(*storage.ReviewJob) {
	return func(j *storage.ReviewJob) { j.EnqueuedAt = t }
}

func withError(err string) func(*storage.ReviewJob) {
	return func(j *storage.ReviewJob) { j.Error = err }
}

func withReviewType(rt string) func(*storage.ReviewJob) {
	return func(j *storage.ReviewJob) { j.ReviewType = rt }
}

func withSynthesis(runUUID string, summary storage.PanelSummary) func(*storage.ReviewJob) {
	return func(j *storage.ReviewJob) {
		j.JobType = storage.JobTypeSynthesis
		j.PanelRole = storage.PanelRoleSynthesis
		j.PanelRunUUID = runUUID
		s := summary
		j.PanelSummary = &s
	}
}

func withPanelMember(runUUID, memberName string, index int) func(*storage.ReviewJob) {
	return func(j *storage.ReviewJob) {
		j.PanelRole = storage.PanelRoleMember
		j.PanelRunUUID = runUUID
		j.PanelMemberName = memberName
		j.PanelMemberIndex = index
	}
}

func withVerdict(v string) func(*storage.ReviewJob) {
	return func(j *storage.ReviewJob) { verdict := v; j.Verdict = &verdict }
}

// makeReview creates a storage.Review linked to the given job.
func makeReview(id int64, job *storage.ReviewJob, opts ...func(*storage.Review)) *storage.Review {
	r := &storage.Review{
		ID:    id,
		JobID: job.ID,
		Job:   job,
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

func withReviewOutput(output string) func(*storage.Review) {
	return func(r *storage.Review) { r.Output = output }
}

func withReviewAgent(agent string) func(*storage.Review) {
	return func(r *storage.Review) { r.Agent = agent }
}

func TestTUIFetchJobsSuccess(t *testing.T) {
	_, m := mockServerModel(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/jobs" {
			assert.Condition(t, func() bool {
				return false
			}, "Expected /api/jobs, got %s", r.URL.Path)
		}
		jobs := []storage.ReviewJob{{ID: 1, GitRef: "abc123", Agent: "test"}}
		json.NewEncoder(w).Encode(map[string]any{"jobs": jobs})
	})
	cmd := m.fetchJobs()
	msg := cmd()

	jobs, ok := msg.(jobsMsg)
	if !ok {
		require.Condition(t, func() bool {
			return false
		}, "Expected jobsMsg, got %T: %v", msg, msg)
	}
	if len(jobs.jobs) != 1 || jobs.jobs[0].ID != 1 {
		assert.Condition(t, func() bool {
			return false
		}, "Unexpected jobs: %+v", jobs.jobs)
	}
}

func TestTUIFetchJobsError(t *testing.T) {
	_, m := mockServerModel(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	cmd := m.fetchJobs()
	msg := cmd()

	_, ok := msg.(jobsErrMsg)
	if !ok {
		require.Condition(t, func() bool {
			return false
		}, "Expected jobsErrMsg for 500, got %T: %v", msg, msg)
	}
}

func TestTUIHTTPTimeout(t *testing.T) {
	_, m := mockServerModel(t, func(w http.ResponseWriter, r *http.Request) {
		// Delay much longer than client timeout to avoid flaky timing on fast machines
		time.Sleep(500 * time.Millisecond)
		json.NewEncoder(w).Encode(map[string]any{"jobs": []storage.ReviewJob{}})
	})
	// Override with short timeout for test (10x shorter than server delay)
	m.client.Timeout = 50 * time.Millisecond

	cmd := m.fetchJobs()
	msg := cmd()

	_, ok := msg.(jobsErrMsg)
	if !ok {
		require.Condition(t, func() bool {
			return false
		}, "Expected jobsErrMsg for timeout, got %T: %v", msg, msg)
	}
}

func TestTUIGetVisibleJobs(t *testing.T) {
	m := newModel(testEndpoint, withExternalIODisabled())

	m.jobs = []storage.ReviewJob{
		makeJob(1, withRepoName("repo-a"), withRepoPath("/path/to/repo-a")),
		makeJob(2, withRepoName("repo-b"), withRepoPath("/path/to/repo-b")),
		makeJob(3, withRepoName("repo-a"), withRepoPath("/path/to/repo-a")),
	}

	// No filter - all jobs visible
	visible := m.getVisibleJobs()
	if len(visible) != 3 {
		assert.Condition(t, func() bool {
			return false
		}, "No filter: expected 3 visible, got %d", len(visible))
	}

	// Filter to repo-a
	m.activeRepoFilter = []string{"/path/to/repo-a"}
	visible = m.getVisibleJobs()
	if len(visible) != 2 {
		assert.Condition(t, func() bool {
			return false
		}, "Filter repo-a: expected 2 visible, got %d", len(visible))
	}
	if visible[0].ID != 1 || visible[1].ID != 3 {
		assert.Condition(t, func() bool {
			return false
		}, "Expected IDs 1 and 3, got %d and %d", visible[0].ID, visible[1].ID)
	}

	// Filter to non-existent repo
	m.activeRepoFilter = []string{"/path/to/repo-xyz"}
	visible = m.getVisibleJobs()
	if len(visible) != 0 {
		assert.Condition(t, func() bool {
			return false
		}, "Filter repo-xyz: expected 0 visible, got %d", len(visible))
	}
}

func TestTUIGetVisibleSelectedIdx(t *testing.T) {
	// Shared setup
	jobs := []storage.ReviewJob{
		makeJob(1, withRepoName("repo-a"), withRepoPath("/path/to/repo-a")),
		makeJob(2, withRepoName("repo-b"), withRepoPath("/path/to/repo-b")),
		makeJob(3, withRepoName("repo-a"), withRepoPath("/path/to/repo-a")),
	}

	tests := []struct {
		name        string
		selectedIdx int
		filter      []string
		want        int
	}{
		{"No filter, valid selection", 1, nil, 1},
		{"No filter, no selection", -1, nil, -1},
		{"Filter active, valid visible selection", 2, []string{"/path/to/repo-a"}, 1}, // index 2 is job ID 3, which is 2nd in filtered list (index 1)
		{"Filter active, selection hidden", 1, []string{"/path/to/repo-a"}, -1},       // index 1 is job ID 2, not in repo-a
		{"Filter active, selectedIdx -1", -1, []string{"/path/to/repo-a"}, -1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newModel(testEndpoint, withExternalIODisabled())
			m.jobs = jobs
			m.selectedIdx = tt.selectedIdx
			if tt.filter != nil {
				m.activeRepoFilter = tt.filter
			}

			if got := m.getVisibleSelectedIdx(); got != tt.want {
				assert.Condition(t, func() bool {
					return false
				}, "getVisibleSelectedIdx() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestTUITickNoRefreshWhileLoadingJobs(t *testing.T) {
	m := newModel(testEndpoint, withExternalIODisabled())

	// Set up with loadingJobs true
	m.jobs = []storage.ReviewJob{makeJob(1), makeJob(2), makeJob(3)}
	m.loadingJobs = true

	// Simulate tick
	m2, _ := updateModel(t, m, tickMsg(time.Now()))

	// loadingJobs should still be true (not reset by tick)
	if !m2.loadingJobs {
		assert.Condition(t, func() bool {
			return false
		}, "loadingJobs should remain true when tick skips refresh")
	}
}

func TestTUIDisplayTickDoesNotTriggerRefresh(t *testing.T) {
	m := newModel(testEndpointFromURL("http://localhost"))
	m.loadingJobs = false
	m.loadingMore = false

	updated, cmd := m.Update(displayTickMsg{})
	m2 := updated.(model)

	if m2.loadingJobs {
		t.Error("display tick should not mark jobs as loading")
	}
	if m2.loadingMore {
		t.Error("display tick should not start pagination loads")
	}
	if cmd == nil {
		t.Error("display tick should reschedule itself")
	}
}

func TestTUITickInterval(t *testing.T) {
	// tickInterval returns a constant fallback interval now that SSE
	// handles real-time updates. Verify it doesn't vary with queue state.
	tests := []struct {
		name        string
		runningJobs int
	}{
		{"idle queue", 0},
		{"active queue", 3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newModel(testEndpoint, withExternalIODisabled())
			m.status.RunningJobs = tt.runningJobs

			assert.Equal(t, tickIntervalFallback, m.tickInterval())
		})
	}
}

func TestTUIJobsMsgClearsLoadingJobs(t *testing.T) {
	m := newModel(testEndpoint, withExternalIODisabled())

	// Set up with loadingJobs true
	m.loadingJobs = true

	// Simulate jobs response (not append)
	m2, _ := updateModel(t, m, jobsMsg{
		jobs:    []storage.ReviewJob{makeJob(1)},
		hasMore: false,
		append:  false,
	})

	// loadingJobs should be cleared
	if m2.loadingJobs {
		assert.Condition(t, func() bool {
			return false
		}, "loadingJobs should be false after non-append JobsMsg")
	}
}

func TestTUIJobsMsgAppendKeepsLoadingJobs(t *testing.T) {
	m := newModel(testEndpoint, withExternalIODisabled())

	// Set up with loadingJobs true (shouldn't normally happen with append, but test the logic)
	m.jobs = []storage.ReviewJob{makeJob(1)}
	m.loadingJobs = true

	// Simulate jobs response (append mode - pagination)
	m2, _ := updateModel(t, m, jobsMsg{
		jobs:    []storage.ReviewJob{makeJob(2)},
		hasMore: false,
		append:  true,
	})

	// loadingJobs should NOT be cleared by append (it's for pagination, not full refresh)
	if !m2.loadingJobs {
		assert.Condition(t, func() bool {
			return false
		}, "loadingJobs should remain true after append JobsMsg")
	}
}

func TestTUINewModelLoadingJobsTrue(t *testing.T) {
	// newModel should initialize loadingJobs to true since Init() calls fetchJobs
	m := newModel(testEndpoint, withExternalIODisabled())
	if !m.loadingJobs {
		assert.Condition(t, func() bool {
			return false
		}, "loadingJobs should be true in new model")
	}
}

func TestTUIJobsErrMsgClearsLoadingJobs(t *testing.T) {
	m := newModel(testEndpoint, withExternalIODisabled())
	m.loadingJobs = true

	// Simulate job fetch error
	m2, _ := updateModel(t, m, jobsErrMsg{err: fmt.Errorf("connection refused")})

	if m2.loadingJobs {
		assert.Condition(t, func() bool {
			return false
		}, "loadingJobs should be cleared on job fetch error")
	}
	if m2.err == nil {
		assert.Condition(t, func() bool {
			return false
		}, "err should be set on job fetch error")
	}
}

func TestTUITickInFlightGuards(t *testing.T) {
	tests := []struct {
		name           string
		loadingStatus  bool
		loadingFixJobs bool
		tasksEnabled   bool
		view           viewKind
		wantStatus     bool // loadingStatus after tick
		wantFixJobs    bool // loadingFixJobs after tick
	}{
		{
			name:          "skips status fetch when already loading",
			loadingStatus: true,
			tasksEnabled:  false,
			wantStatus:    true, // stays true, no new fetch dispatched
			wantFixJobs:   false,
		},
		{
			name:          "dispatches status fetch when idle",
			loadingStatus: false,
			tasksEnabled:  false,
			wantStatus:    true, // set to true by dispatch
			wantFixJobs:   false,
		},
		{
			name:           "skips fix-jobs fetch when already loading",
			loadingFixJobs: true,
			tasksEnabled:   true,
			view:           viewTasks,
			wantStatus:     true,
			wantFixJobs:    true, // stays true, no new fetch dispatched
		},
		{
			name:           "dispatches fix-jobs fetch when idle on tasks view",
			loadingFixJobs: false,
			tasksEnabled:   true,
			view:           viewTasks,
			wantStatus:     true,
			wantFixJobs:    true, // set to true by dispatch
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			m := newModel(testEndpoint, withExternalIODisabled())
			m.loadingJobs = false
			m.loadingMore = false
			m.loadingStatus = tt.loadingStatus
			m.loadingFixJobs = tt.loadingFixJobs
			m.tasksEnabled = tt.tasksEnabled
			if tt.view != 0 {
				m.currentView = tt.view
			}

			m2, cmd := updateModel(t, m, tickMsg(time.Now()))

			assert.NotNil(cmd, "tick should always return commands")
			assert.Equal(tt.wantStatus, m2.loadingStatus)
			assert.Equal(tt.wantFixJobs, m2.loadingFixJobs)
		})
	}
}

func TestTUIStatusResponseClearsLoadingFlag(t *testing.T) {
	assert := assert.New(t)
	m := newModel(testEndpoint, withExternalIODisabled())
	m.loadingStatus = true

	m2, _ := updateModel(t, m, statusMsg{})
	assert.False(m2.loadingStatus, "statusMsg should clear loadingStatus")

	// Error path should also clear the flag.
	m.loadingStatus = true
	m3, _ := updateModel(t, m, statusErrMsg{err: fmt.Errorf("connection refused")})
	assert.False(m3.loadingStatus, "statusErrMsg should clear loadingStatus")
}

func TestTUIFixJobsResponseClearsLoadingFlag(t *testing.T) {
	assert := assert.New(t)
	m := newModel(testEndpoint, withExternalIODisabled())
	m.loadingFixJobs = true

	m2, _ := updateModel(t, m, fixJobsMsg{jobs: []storage.ReviewJob{makeJob(1)}})
	assert.False(m2.loadingFixJobs, "fixJobsMsg should clear loadingFixJobs")

	// Error path should also clear the flag.
	m.loadingFixJobs = true
	m3, _ := updateModel(t, m, fixJobsMsg{err: fmt.Errorf("connection refused")})
	assert.False(m3.loadingFixJobs, "fixJobsMsg with error should clear loadingFixJobs")
}

func TestTUIFixJobsStaleFlagTriggersFollowUp(t *testing.T) {
	assert := assert.New(t)
	m := newModel(testEndpoint, withExternalIODisabled())

	// Simulate: a fetch is in flight and a mutation marks data as stale.
	m.loadingFixJobs = true
	m.fixJobsStale = true

	// When the in-flight fetch returns, it should dispatch a follow-up.
	m2, cmd := updateModel(t, m, fixJobsMsg{jobs: []storage.ReviewJob{makeJob(1)}})
	assert.True(m2.loadingFixJobs, "should re-dispatch fetch when stale")
	assert.False(m2.fixJobsStale, "stale flag should be cleared")
	assert.NotNil(cmd, "should return a follow-up fetch command")

	// Without the stale flag, no follow-up.
	m3, cmd := updateModel(t, m2, fixJobsMsg{jobs: []storage.ReviewJob{makeJob(1)}})
	assert.False(m3.loadingFixJobs, "should not re-dispatch without stale flag")
	assert.Nil(cmd, "should return no command without stale flag")

	// Error path should also honor the stale flag.
	m.loadingFixJobs = true
	m.fixJobsStale = true
	m4, cmd := updateModel(t, m, fixJobsMsg{err: fmt.Errorf("connection refused")})
	assert.True(m4.loadingFixJobs, "should re-dispatch on error when stale")
	assert.False(m4.fixJobsStale, "stale flag should be cleared on error path")
	assert.NotNil(cmd, "should return follow-up command on error path")
}

func TestTUIStatusStaleFlagTriggersFollowUp(t *testing.T) {
	assert := assert.New(t)
	m := newModel(testEndpoint, withExternalIODisabled())

	// Simulate: a fetch is in flight and an SSE event marks data as stale.
	m.loadingStatus = true
	m.statusStale = true

	// When the in-flight fetch returns, it should dispatch a follow-up.
	m2, cmd := updateModel(t, m, statusMsg{})
	assert.True(m2.loadingStatus, "should re-dispatch fetch when stale")
	assert.False(m2.statusStale, "stale flag should be cleared")
	assert.NotNil(cmd, "should return a follow-up fetch command")

	// Without the stale flag, no follow-up.
	m3, cmd := updateModel(t, m2, statusMsg{})
	assert.False(m3.loadingStatus, "should not re-dispatch without stale flag")
	assert.Nil(cmd, "should return no command without stale flag")

	// Error path should also honor the stale flag.
	m.loadingStatus = true
	m.statusStale = true
	m4, cmd := updateModel(t, m, statusErrMsg{err: fmt.Errorf("connection refused")})
	assert.True(m4.loadingStatus, "should re-dispatch on error when stale")
	assert.False(m4.statusStale, "stale flag should be cleared on error path")
	assert.NotNil(cmd, "should return follow-up command on error path")
}

func TestTUIHideClosedMalformedConfigNotOverwritten(t *testing.T) {
	tmpDir := setupTuiTestEnv(t)

	// Write malformed TOML that LoadGlobal will fail to parse
	malformed := []byte(`this is not valid toml {{{`)
	configPath := filepath.Join(tmpDir, "config.toml")
	if err := os.WriteFile(configPath, malformed, 0o644); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "write malformed config: %v", err)
	}

	m := newModel(testEndpoint)
	m.currentView = viewQueue

	// Toggle hide closed ON
	m2, _ := pressKey(m, 'h')

	// In-session toggle should still work
	if !m2.hideClosed {
		assert.Condition(t, func() bool {
			return false
		}, "hideClosed should be true after pressing 'h'")
	}

	// Malformed config file must not have been overwritten
	got, err := os.ReadFile(configPath)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "read config: %v", err)
	}
	if string(got) != string(malformed) {
		assert.Condition(t, func() bool {
			return false
		}, "malformed config was overwritten:\n  before: %q\n  after:  %q", malformed, got)
	}

	// Toggle back OFF — still works in-session
	m3, _ := pressKey(m2, 'h')
	if m3.hideClosed {
		assert.Condition(t, func() bool {
			return false
		}, "hideClosed should be false after pressing 'h' again")
	}
}

func TestTUIIsJobVisibleRespectsPendingClosed(t *testing.T) {
	m := newModel(testEndpoint, withExternalIODisabled())
	m.hideClosed = true

	// Job with Closed=false but pendingClosed=true should be hidden
	m.jobs = []storage.ReviewJob{
		makeJob(1, withClosed(new(false))),
	}
	m.pendingClosed[1] = pendingState{newState: true, seq: 1}

	if m.isJobVisible(m.jobs[0]) {
		assert.Condition(t, func() bool {
			return false
		}, "Job with pendingClosed=true should be hidden when hideClosed is active")
	}

	// Job with Closed=true but pendingClosed=false should be visible
	m.jobs = []storage.ReviewJob{
		makeJob(2, withClosed(new(true))),
	}
	m.pendingClosed[2] = pendingState{newState: false, seq: 1}

	if !m.isJobVisible(m.jobs[0]) {
		assert.Condition(t, func() bool {
			return false
		}, "Job with pendingClosed=false should be visible even if job.Closed is true")
	}

	// Job with no pendingClosed entry falls back to job.Closed
	m.jobs = []storage.ReviewJob{
		makeJob(3, withClosed(new(true))),
	}
	delete(m.pendingClosed, 3)

	if m.isJobVisible(m.jobs[0]) {
		assert.Condition(t, func() bool {
			return false
		}, "Job with Closed=true and no pending entry should be hidden")
	}
}

func TestTUIUpdateNotificationInQueueView(t *testing.T) {
	m := newModel(testEndpoint, withExternalIODisabled())
	m.currentView = viewQueue
	m.width = 80
	m.height = 24
	m.updateAvailable = "1.2.3"

	output := m.renderQueueView()
	if !strings.Contains(output, "Update available: 1.2.3") {
		assert.Condition(t, func() bool {
			return false
		}, "Expected update notification in queue view")
	}
	if !strings.Contains(output, "run 'roborev update'") {
		assert.Condition(t, func() bool {
			return false
		}, "Expected update instructions in queue view")
	}

	// Verify update notification appears on line 3 (index 2) - above the table
	// Layout: line 0 = title, line 1 = status, line 2 = update notification
	lines := strings.Split(output, "\n")
	if len(lines) < 3 {
		require.Condition(t, func() bool {
			return false
		}, "Expected at least 3 lines, got %d", len(lines))
	}
	// Line 2 (third line) should contain the update notification
	if !strings.Contains(lines[2], "Update available") {
		assert.Condition(t, func() bool {
			return false
		}, "Expected update notification on line 3 (index 2), got: %q", lines[2])
	}
}

func TestTUIUpdateNotificationDevBuild(t *testing.T) {
	m := newModel(testEndpoint, withExternalIODisabled())
	m.currentView = viewQueue
	m.width = 80
	m.height = 24
	m.updateAvailable = "1.2.3"
	m.updateIsDevBuild = true

	output := m.renderQueueView()
	if !strings.Contains(output, "Dev build") {
		assert.Condition(t, func() bool {
			return false
		}, "Expected 'Dev build' in notification for dev builds")
	}
	if !strings.Contains(output, "roborev update --force") {
		assert.Condition(t, func() bool {
			return false
		}, "Expected --force flag in update instructions for dev builds")
	}
}

func TestTUIUpdateNotificationNotInReviewView(t *testing.T) {
	m := newModel(testEndpoint, withExternalIODisabled())
	m.currentView = viewReview
	m.width = 80
	m.height = 24
	m.updateAvailable = "1.2.3"
	m.currentReview = makeReview(1, &storage.ReviewJob{}, withReviewOutput("Test review content"))

	output := m.renderReviewView()
	if strings.Contains(output, "Update available") {
		assert.Condition(t, func() bool {
			return false
		}, "Update notification should not appear in review view")
	}
}

func TestTUIVersionMismatchDetection(t *testing.T) {
	_ = setupTuiTestEnv(t)

	t.Run("detects version mismatch", func(t *testing.T) {
		m := newModel(testEndpoint, withExternalIODisabled())

		// Simulate receiving status with different version
		status := statusMsg{status: storage.DaemonStatus{
			Version: "different-version",
		}}

		m2, _ := updateModel(t, m, status)

		if !m2.versionMismatch {
			assert.Condition(t, func() bool {
				return false
			}, "Expected versionMismatch=true when daemon version differs")
		}
		if m2.daemonVersion != "different-version" {
			assert.Condition(t, func() bool {
				return false
			}, "Expected daemonVersion='different-version', got %q", m2.daemonVersion)
		}
	})

	t.Run("no mismatch when versions match", func(t *testing.T) {
		m := newModel(testEndpoint, withExternalIODisabled())

		// Simulate receiving status with same version as TUI
		status := statusMsg{status: storage.DaemonStatus{
			Version: version.Version,
		}}

		m2, _ := updateModel(t, m, status)

		if m2.versionMismatch {
			assert.Condition(t, func() bool {
				return false
			}, "Expected versionMismatch=false when versions match")
		}
	})

	t.Run("displays error banner in queue view when mismatched", func(t *testing.T) {
		m := newModel(testEndpoint, withExternalIODisabled())
		m.width = 100
		m.height = 30
		m.currentView = viewQueue
		m.versionMismatch = true
		m.daemonVersion = "old-version"

		output := m.View().Content

		if !strings.Contains(output, "Daemon: old-version") {
			assert.Condition(t, func() bool {
				return false
			}, "Expected queue view to show daemon version")
		}
		if !strings.Contains(output, "[MISMATCH]") {
			assert.Condition(t, func() bool {
				return false
			}, "Expected queue view to show mismatch badge")
		}
		if strings.Contains(output, "VERSION MISMATCH") {
			assert.Condition(t, func() bool {
				return false
			}, "Expected queue view to move mismatch warning out of footer")
		}
	})

	t.Run("does not display daemon status in review view when mismatched", func(t *testing.T) {
		m := newModel(testEndpoint, withExternalIODisabled())
		m.width = 100
		m.height = 30
		m.currentView = viewReview
		m.versionMismatch = true
		m.daemonVersion = "old-version"
		m.currentReview = &storage.Review{
			ID:     1,
			Output: "Test review",
			Job: &storage.ReviewJob{
				ID:       1,
				GitRef:   "abc123",
				RepoName: "test",
				Agent:    "test",
			},
		}

		output := m.View().Content

		if strings.Contains(output, "Daemon: old-version") {
			assert.Condition(t, func() bool {
				return false
			}, "Expected review view to omit daemon status")
		}
		if strings.Contains(output, "[MISMATCH]") {
			assert.Condition(t, func() bool {
				return false
			}, "Expected review view to omit mismatch badge")
		}
	})

	t.Run("review flash still renders when mismatch is present", func(t *testing.T) {
		m := newModel(testEndpoint, withExternalIODisabled())
		m.width = 100
		m.height = 30
		m.currentView = viewReview
		m.versionMismatch = true
		m.daemonVersion = "old-version"
		m.currentReview = &storage.Review{
			ID:     1,
			Output: "Test review",
			Job: &storage.ReviewJob{
				ID:       1,
				GitRef:   "abc123",
				RepoName: "test",
				Agent:    "test",
			},
		}
		m.setFlash("Saved", time.Minute, viewReview)

		output := m.View().Content

		if !strings.Contains(output, "Saved") {
			assert.Condition(t, func() bool {
				return false
			}, "Expected review flash message to remain visible")
		}
		if strings.Contains(output, "Daemon: old-version") {
			assert.Condition(t, func() bool {
				return false
			}, "Expected review view to omit daemon status even when flash is present")
		}
		if strings.Contains(output, "[MISMATCH]") {
			assert.Condition(t, func() bool {
				return false
			}, "Expected review view to omit mismatch badge even when flash is present")
		}
	})
}

func TestTUIConfigReloadFlash(t *testing.T) {
	_ = setupTuiTestEnv(t)
	m := newModel(testEndpoint, withExternalIODisabled())

	t.Run("no flash on first status fetch", func(t *testing.T) {
		// First status fetch with a ConfigReloadCounter should NOT flash
		status1 := statusMsg{status: storage.DaemonStatus{
			Version:             "1.0.0",
			ConfigReloadCounter: 1,
		}}

		m2, _ := updateModel(t, m, status1)

		if m2.flashMessage != "" {
			assert.Condition(t, func() bool {
				return false
			}, "Expected no flash on first fetch, got %q", m2.flashMessage)
		}
		if !m2.statusFetchedOnce {
			assert.Condition(t, func() bool {
				return false
			}, "Expected statusFetchedOnce to be true after first fetch")
		}
		if m2.lastConfigReloadCounter != 1 {
			assert.Condition(t, func() bool {
				return false
			}, "Expected lastConfigReloadCounter to be 1, got %d", m2.lastConfigReloadCounter)
		}
	})

	t.Run("flash on config reload after first fetch", func(t *testing.T) {
		// Start with a model that has already fetched status once
		m := newModel(testEndpoint, withExternalIODisabled())
		m.statusFetchedOnce = true
		m.lastConfigReloadCounter = 1

		// Second status with different ConfigReloadCounter should flash
		status2 := statusMsg{status: storage.DaemonStatus{
			Version:             "1.0.0",
			ConfigReloadCounter: 2,
		}}

		m2, _ := updateModel(t, m, status2)

		if m2.flashMessage != "Config reloaded" {
			assert.Condition(t, func() bool {
				return false
			}, "Expected flash 'Config reloaded', got %q", m2.flashMessage)
		}
		if m2.lastConfigReloadCounter != 2 {
			assert.Condition(t, func() bool {
				return false
			}, "Expected lastConfigReloadCounter updated to 2, got %d", m2.lastConfigReloadCounter)
		}
	})

	t.Run("flash when ConfigReloadCounter changes from zero to non-zero", func(t *testing.T) {
		// Model has fetched status once but daemon hadn't reloaded yet
		m := newModel(testEndpoint, withExternalIODisabled())
		m.statusFetchedOnce = true
		m.lastConfigReloadCounter = 0 // No reload had occurred

		// Now config is reloaded
		status := statusMsg{status: storage.DaemonStatus{
			Version:             "1.0.0",
			ConfigReloadCounter: 1,
		}}

		m2, _ := updateModel(t, m, status)

		if m2.flashMessage != "Config reloaded" {
			assert.Condition(t, func() bool {
				return false
			}, "Expected flash when ConfigReloadCounter goes from 0 to 1, got %q", m2.flashMessage)
		}
	})

	t.Run("no flash when ConfigReloadCounter unchanged", func(t *testing.T) {
		m := newModel(testEndpoint, withExternalIODisabled())
		m.statusFetchedOnce = true
		m.lastConfigReloadCounter = 1

		// Same counter
		status := statusMsg{status: storage.DaemonStatus{
			Version:             "1.0.0",
			ConfigReloadCounter: 1,
		}}

		m2, _ := updateModel(t, m, status)

		if m2.flashMessage != "" {
			assert.Condition(t, func() bool {
				return false
			}, "Expected no flash when counter unchanged, got %q", m2.flashMessage)
		}
	})
}

func TestTUIReconnectOnConsecutiveErrors(t *testing.T) {
	_ = setupTuiTestEnv(t)

	type testCase struct {
		name              string
		initialErrors     int
		reconnecting      bool
		initialErr        error
		msg               tea.Msg
		wantErrors        int
		wantReconnecting  bool
		wantCmd           bool
		wantBaseURL       string
		wantDaemonVersion string
		wantErrNil        bool
	}

	tests := []testCase{
		{
			name:             "triggers reconnection after 3 consecutive connection errors",
			initialErrors:    2,
			msg:              jobsErrMsg{err: mockConnError("connection refused")},
			wantErrors:       3,
			wantReconnecting: true,
			wantCmd:          true,
		},
		{
			name:             "does not trigger reconnection before 3 errors",
			initialErrors:    1,
			msg:              jobsErrMsg{err: mockConnError("connection refused")},
			wantErrors:       2,
			wantReconnecting: false,
			wantCmd:          false,
		},
		{
			name:             "does not count application errors for reconnection",
			initialErrors:    2,
			msg:              errMsg(fmt.Errorf("no review found")),
			wantErrors:       2,
			wantReconnecting: false,
			wantCmd:          false,
		},
		{
			name:             "does not count non-connection errors in jobs fetch",
			initialErrors:    2,
			msg:              jobsErrMsg{err: fmt.Errorf("invalid JSON response")},
			wantErrors:       2,
			wantReconnecting: false,
			wantCmd:          false,
		},
		{
			name:             "pagination errors also trigger reconnection",
			initialErrors:    2,
			msg:              paginationErrMsg{err: mockConnError("connection refused")},
			wantErrors:       3,
			wantReconnecting: true,
			wantCmd:          true,
		},
		{
			name:             "status/review connection errors trigger reconnection",
			initialErrors:    2,
			msg:              errMsg(mockConnError("connection refused")),
			wantErrors:       3,
			wantReconnecting: true,
			wantCmd:          true,
		},
		{
			name:             "status/review application errors do not trigger reconnection",
			initialErrors:    2,
			msg:              errMsg(fmt.Errorf("review not found")),
			wantErrors:       2,
			wantReconnecting: false,
			wantCmd:          false,
		},
		{
			name:             "resets error count on successful jobs fetch",
			initialErrors:    5,
			msg:              jobsMsg{jobs: []storage.ReviewJob{}, hasMore: false},
			wantErrors:       0,
			wantReconnecting: false,
			wantCmd:          false,
		},
		{
			name:             "resets error count on successful status fetch",
			initialErrors:    5,
			msg:              statusMsg{status: storage.DaemonStatus{Version: "1.0.0"}},
			wantErrors:       0,
			wantReconnecting: false,
			wantCmd:          false,
		},
		{
			name:              "updates server address on successful reconnection",
			initialErrors:     0,
			reconnecting:      true,
			initialErr:        errors.New("connection refused"),
			msg:               reconnectMsg{endpoint: testEndpointFromURL("http://127.0.0.1:7374"), version: "2.0.0"},
			wantErrors:        0,
			wantReconnecting:  false,
			wantCmd:           true,
			wantBaseURL:       "http://127.0.0.1:7374",
			wantDaemonVersion: "2.0.0",
			wantErrNil:        true,
		},
		{
			name:             "handles reconnection to same address",
			initialErrors:    3,
			reconnecting:     true,
			msg:              reconnectMsg{endpoint: testEndpoint},
			wantErrors:       0,
			wantReconnecting: false,
			wantCmd:          true,
			wantErrNil:       true,
			wantBaseURL:      testEndpoint.BaseURL(),
		},
		{
			name:             "handles failed reconnection",
			initialErrors:    3,
			reconnecting:     true,
			msg:              reconnectMsg{err: fmt.Errorf("no daemon found")},
			wantErrors:       3,
			wantReconnecting: false,
			wantCmd:          false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newModel(testEndpoint, withExternalIODisabled())
			m.consecutiveErrors = tt.initialErrors
			m.reconnecting = tt.reconnecting
			m.err = tt.initialErr

			m2, cmd := updateModel(t, m, tt.msg)

			if m2.consecutiveErrors != tt.wantErrors {
				assert.Condition(t, func() bool {
					return false
				}, "consecutiveErrors = %d, want %d", m2.consecutiveErrors, tt.wantErrors)
			}
			if m2.reconnecting != tt.wantReconnecting {
				assert.Condition(t, func() bool {
					return false
				}, "reconnecting = %v, want %v", m2.reconnecting, tt.wantReconnecting)
			}
			if (cmd != nil) != tt.wantCmd {
				assert.Condition(t, func() bool {
					return false
				}, "cmd returned = %v, want %v", cmd != nil, tt.wantCmd)
			}
			if tt.wantBaseURL != "" && m2.endpoint.BaseURL() != tt.wantBaseURL {
				assert.Condition(t, func() bool {
					return false
				}, "endpoint.BaseURL() = %q, want %q", m2.endpoint.BaseURL(), tt.wantBaseURL)
			}
			if tt.wantDaemonVersion != "" && m2.daemonVersion != tt.wantDaemonVersion {
				assert.Condition(t, func() bool {
					return false
				}, "daemonVersion = %q, want %q", m2.daemonVersion, tt.wantDaemonVersion)
			}
			if tt.wantErrNil && m2.err != nil {
				assert.Condition(t, func() bool {
					return false
				}, "expected err to be nil, got %v", m2.err)
			}
		})
	}
}

func TestTUIStatusDisplaysCorrectly(t *testing.T) {
	// Test that the queue view renders status correctly
	m := newModel(testEndpoint, withExternalIODisabled())
	m.width = 200
	m.height = 30
	m.currentView = viewQueue

	m.jobs = []storage.ReviewJob{
		makeJob(1, withRepoName("repo"), withRepoPath("/path"), withRef("abc"), withStatus(storage.JobStatusRunning)),
		makeJob(2, withRepoName("repo"), withRepoPath("/path"), withRef("def"), withStatus(storage.JobStatusQueued)),
		makeJob(3, withRepoName("repo"), withRepoPath("/path"), withRef("ghi")),
		makeJob(4, withRepoName("repo"), withRepoPath("/path"), withRef("jkl"), withStatus(storage.JobStatusFailed)),
		makeJob(5, withRepoName("repo"), withRepoPath("/path"), withRef("mno"), withStatus(storage.JobStatusCanceled)),
	}
	m.selectedIdx = 0

	output := m.View().Content
	if len(output) == 0 {
		assert.Condition(t, func() bool {
			return false
		}, "Expected non-empty view output")
	}

	// Verify all status strings appear in output
	for _, status := range []string{"Running", "Queued", "Done", "Error", "Canceled"} {
		if !strings.Contains(output, status) {
			assert.Condition(t, func() bool {
				return false
			}, "Expected output to contain status '%s'", status)
		}
	}
}

func TestHandleFixKeyRejectsFixJob(t *testing.T) {
	m := newModel(testEndpoint, withExternalIODisabled())
	m.currentView = viewQueue
	m.tasksEnabled = true
	m.jobs = []storage.ReviewJob{
		{
			ID:      10,
			Status:  storage.JobStatusDone,
			JobType: storage.JobTypeFix,
		},
	}
	m.selectedIdx, m.selectedJobID = 0, 10

	result, cmd := m.Update(keyPressMsg('F'))
	updated := result.(model)

	if cmd != nil {
		assert.Condition(t, func() bool {
			return false
		}, "Expected nil cmd for rejected fix-of-fix")
	}
	if updated.flashMessage != "Cannot fix a fix job" {
		assert.Condition(t, func() bool {
			return false
		}, "Expected flash 'Cannot fix a fix job', got %q",
			updated.flashMessage)
	}
	if updated.currentView != viewQueue {
		assert.Condition(t, func() bool {
			return false
		}, "Expected view to stay on queue, got %d",
			updated.currentView)
	}
}

func TestTUIFixTriggerResultMsg(t *testing.T) {
	t.Run("warning shows flash and triggers refresh", func(t *testing.T) {
		m := newModel(testEndpoint, withExternalIODisabled())
		m.currentView = viewTasks
		m.width = 80
		m.height = 24

		msg := fixTriggerResultMsg{
			job:     &storage.ReviewJob{ID: 42},
			warning: "rebase job #42 enqueued but failed to mark #10 as rebased: server error",
		}

		result, cmd := m.Update(msg)
		updated := result.(model)

		if !strings.Contains(updated.flashMessage, "failed to mark") {
			assert.Condition(t, func() bool {
				return false
			}, "expected warning in flash, got %q", updated.flashMessage)
		}
		if updated.flashView != viewTasks {
			assert.Condition(t, func() bool {
				return false
			}, "expected flash view tasks, got %v", updated.flashView)
		}
		if cmd == nil {
			assert.Condition(t, func() bool {
				return false
			}, "expected refresh cmd, got nil")
		}
	})

	t.Run("success shows enqueued flash and triggers refresh", func(t *testing.T) {
		m := newModel(testEndpoint, withExternalIODisabled())
		m.currentView = viewTasks
		m.width = 80
		m.height = 24

		msg := fixTriggerResultMsg{
			job: &storage.ReviewJob{ID: 99},
		}

		result, cmd := m.Update(msg)
		updated := result.(model)

		if !strings.Contains(updated.flashMessage, "#99 enqueued") {
			assert.Condition(t, func() bool {
				return false
			}, "expected enqueued flash, got %q", updated.flashMessage)
		}
		if cmd == nil {
			assert.Condition(t, func() bool {
				return false
			}, "expected refresh cmd, got nil")
		}
	})

	t.Run("error shows failure flash with no refresh", func(t *testing.T) {
		m := newModel(testEndpoint, withExternalIODisabled())
		m.currentView = viewTasks
		m.width = 80
		m.height = 24

		msg := fixTriggerResultMsg{
			err: fmt.Errorf("connection refused"),
		}

		result, cmd := m.Update(msg)
		updated := result.(model)

		if !strings.Contains(updated.flashMessage, "Fix failed") {
			assert.Condition(t, func() bool {
				return false
			}, "expected failure flash, got %q", updated.flashMessage)
		}
		if cmd != nil {
			assert.Condition(t, func() bool {
				return false
			}, "expected no cmd on error, got non-nil")
		}
	})
}

func TestTUIColumnOptionsCanEnableTasksWorkflow(t *testing.T) {
	setupTuiTestEnv(t)

	m := newModel(testEndpoint, withExternalIODisabled())
	m.currentView = viewQueue

	result, _ := m.Update(keyPressMsg('o'))
	updated := result.(model)
	if updated.currentView != viewColumnOptions {
		require.Condition(t, func() bool {
			return false
		}, "expected column options view, got %v", updated.currentView)
	}

	idx := -1
	for i, opt := range updated.colOptionsList {
		if opt.id == colOptionTasksWorkflow {
			idx = i
			break
		}
	}
	if idx < 0 {
		require.Condition(t, func() bool {
			return false
		}, "expected tasks workflow option in column options")
	}
	updated.colOptionsIdx = idx

	result, _ = updated.Update(keySpecialMsg(tea.KeyEnter))
	toggled := result.(model)
	if !toggled.tasksEnabled {
		require.Condition(t, func() bool {
			return false
		}, "expected tasks workflow to be enabled after toggle")
	}

	result, cmd := toggled.Update(keySpecialMsg(tea.KeyEsc))
	closed := result.(model)
	if closed.currentView != viewQueue {
		require.Condition(t, func() bool {
			return false
		}, "expected to return to queue view, got %v", closed.currentView)
	}
	if cmd == nil {
		require.Condition(t, func() bool {
			return false
		}, "expected save command after closing column options")
	}
	if msg := cmd(); msg != nil {
		if errMsg, ok := msg.(configSaveErrMsg); ok {
			require.Condition(t, func() bool {
				return false
			}, "save config failed: %v", errMsg.err)
		}
	}

	cfg, err := config.LoadGlobal()
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "LoadGlobal failed: %v", err)
	}
	if !cfg.Advanced.TasksEnabled {
		require.Condition(t, func() bool {
			return false
		}, "expected advanced.tasks_enabled to persist as true")
	}
}

func TestTUIColumnOptionsCanDisableMouse(t *testing.T) {
	setupTuiTestEnv(t)

	m := newModel(testEndpoint, withExternalIODisabled())
	m.currentView = viewQueue

	result, _ := m.Update(keyPressMsg('o'))
	updated := result.(model)
	if updated.currentView != viewColumnOptions {
		require.Condition(t, func() bool {
			return false
		}, "expected column options view, got %v", updated.currentView)
	}

	idx := -1
	for i, opt := range updated.colOptionsList {
		if opt.id == colOptionMouse {
			idx = i
			break
		}
	}
	if idx < 0 {
		require.Condition(t, func() bool {
			return false
		}, "expected mouse option in column options")
	}
	updated.colOptionsIdx = idx

	result, cmd := updated.Update(keySpecialMsg(tea.KeyEnter))
	toggled := result.(model)
	if toggled.mouseEnabled {
		require.Condition(t, func() bool {
			return false
		}, "expected mouse to be disabled after toggle")
	}
	if cmd != nil {
		require.Condition(t, func() bool {
			return false
		}, "expected no mouse toggle command after disabling mouse")
	}
	if toggled.View().MouseMode != tea.MouseModeNone {
		require.Condition(t, func() bool {
			return false
		}, "expected mouse mode none after disabling mouse")
	}

	result, cmd = toggled.Update(keySpecialMsg(tea.KeyEsc))
	closed := result.(model)
	if closed.currentView != viewQueue {
		require.Condition(t, func() bool {
			return false
		}, "expected to return to queue view, got %v", closed.currentView)
	}
	if cmd == nil {
		require.Condition(t, func() bool {
			return false
		}, "expected save command after closing column options")
	}
	msgs := collectMsgs(cmd)
	if len(msgs) > 0 {
		if last := msgs[len(msgs)-1]; last != nil {
			if errMsg, ok := last.(configSaveErrMsg); ok {
				require.Condition(t, func() bool {
					return false
				}, "save config failed: %v", errMsg.err)
			}
		}
	}

	cfg, err := config.LoadGlobal()
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "LoadGlobal failed: %v", err)
	}
	if cfg.MouseEnabled {
		require.Condition(t, func() bool {
			return false
		}, "expected mouse_enabled to persist as false")
	}
}

func TestTUIColumnOptionsCanReEnableMouse(t *testing.T) {
	setupTuiTestEnv(t)

	m := newModel(testEndpoint, withExternalIODisabled())
	m.currentView = viewQueue
	m.width = 120
	m.height = 20
	m.jobs = []storage.ReviewJob{
		makeJob(1),
		makeJob(2),
		makeJob(3),
	}
	m.selectedIdx = 0
	m.selectedJobID = 1

	result, _ := m.Update(keyPressMsg('o'))
	updated := result.(model)

	idx := -1
	for i, opt := range updated.colOptionsList {
		if opt.id == colOptionMouse {
			idx = i
			break
		}
	}
	if idx < 0 {
		require.Condition(t, func() bool {
			return false
		}, "expected mouse option in column options")
	}
	updated.colOptionsIdx = idx

	result, _ = updated.Update(keySpecialMsg(tea.KeyEnter))
	disabled := result.(model)
	if disabled.mouseEnabled {
		require.Condition(t, func() bool {
			return false
		}, "expected mouse to be disabled after first toggle")
	}

	result, cmd := disabled.Update(keySpecialMsg(tea.KeyEnter))
	reenabled := result.(model)
	if !reenabled.mouseEnabled {
		require.Condition(t, func() bool {
			return false
		}, "expected mouse to be enabled after second toggle")
	}
	if cmd != nil {
		require.Condition(t, func() bool {
			return false
		}, "expected no mouse toggle command after enabling mouse")
	}
	if reenabled.View().MouseMode != tea.MouseModeCellMotion {
		require.Condition(t, func() bool {
			return false
		}, "expected cell-motion mouse mode after enabling mouse")
	}

	result, _ = reenabled.Update(keySpecialMsg(tea.KeyEsc))
	closed := result.(model)
	if closed.currentView != viewQueue {
		require.Condition(t, func() bool {
			return false
		}, "expected to return to queue view, got %v", closed.currentView)
	}

	m2, _ := updateModel(t, closed, mouseWheelDown())
	if m2.selectedIdx != 1 || m2.selectedJobID != 2 {
		require.Condition(t, func() bool {
			return false
		}, "expected wheel to work after re-enabling mouse, got idx=%d id=%d", m2.selectedIdx, m2.selectedJobID)
	}
}

func TestNewModelLoadsMouseDisabledFromConfig(t *testing.T) {
	tmpDir := setupTuiTestEnv(t)

	configPath := filepath.Join(tmpDir, "config.toml")
	if err := os.WriteFile(configPath, []byte("mouse_enabled = false\n"), 0o644); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "write config: %v", err)
	}

	m := newModel(testEndpoint)
	if m.mouseEnabled {
		require.Condition(t, func() bool {
			return false
		}, "expected newModel to load mouse_enabled = false from config")
	}
	if m.View().MouseMode != tea.MouseModeNone {
		require.Condition(t, func() bool {
			return false
		}, "expected rendered view without mouse capture when disabled")
	}
}

func TestTUISelection(t *testing.T) {
	t.Run("MaintainedOnInsert", func(t *testing.T) {
		m := newModel(testEndpoint, withExternalIODisabled())

		// Initial state with 3 jobs, select the middle one (ID=2)
		m.jobs = []storage.ReviewJob{
			makeJob(3), makeJob(2), makeJob(1),
		}
		m.selectedIdx = 1
		m.selectedJobID = 2

		// New jobs added at the top (newer jobs first)
		newJobs := jobsMsg{jobs: []storage.ReviewJob{
			makeJob(5), makeJob(4), makeJob(3), makeJob(2), makeJob(1),
		}}

		m, _ = updateModel(t, m, newJobs)

		// Should still be on job ID=2, now at index 3
		assertSelection(t, m, 3, 2)
	})
	t.Run("ClampsOnRemoval", func(t *testing.T) {
		m := newModel(testEndpoint, withExternalIODisabled())

		// Initial state with 3 jobs, select the last one (ID=1)
		m.jobs = []storage.ReviewJob{
			makeJob(3), makeJob(2), makeJob(1),
		}
		m.selectedIdx = 2
		m.selectedJobID = 1

		// Job ID=1 is removed
		newJobs := jobsMsg{jobs: []storage.ReviewJob{
			makeJob(3), makeJob(2),
		}}

		m, _ = updateModel(t, m, newJobs)

		// Should clamp to last valid index and update selectedJobID
		assertSelection(t, m, 1, 2)
	})
	t.Run("FirstJobOnEmpty", func(t *testing.T) {
		m := newModel(testEndpoint, withExternalIODisabled())

		// No prior selection (empty jobs list, zero selectedJobID)
		m.jobs = []storage.ReviewJob{}
		m.selectedIdx = 0
		m.selectedJobID = 0

		// Jobs arrive
		newJobs := jobsMsg{jobs: []storage.ReviewJob{
			makeJob(5), makeJob(4), makeJob(3),
		}}

		m, _ = updateModel(t, m, newJobs)

		// Should select first job
		assertSelection(t, m, 0, 5)
	})
	t.Run("EmptyList", func(t *testing.T) {
		m := newModel(testEndpoint, withExternalIODisabled())

		// Had jobs, now empty
		m.jobs = []storage.ReviewJob{makeJob(1)}
		m.selectedIdx = 0
		m.selectedJobID = 1

		newJobs := jobsMsg{jobs: []storage.ReviewJob{}}

		m, _ = updateModel(t, m, newJobs)

		// Empty list should have selectedIdx=-1 (no valid selection)
		assertSelection(t, m, -1, 0)
	})
	t.Run("MaintainedOnLargeBatch", func(t *testing.T) {
		m := newModel(testEndpoint, withExternalIODisabled())

		// Initial state with 1 job selected
		m.jobs = []storage.ReviewJob{makeJob(1)}
		m.selectedIdx = 0
		m.selectedJobID = 1

		// 30 new jobs added at the top (simulating large batch)
		newJobs := make([]storage.ReviewJob, 31)
		for i := range 30 {
			newJobs[i] = makeJob(int64(31 - i)) // IDs 31, 30, 29, ..., 2
		}
		newJobs[30] = makeJob(1) // Original job at the end

		m, _ = updateModel(t, m, jobsMsg{jobs: newJobs})

		// Should still follow job ID=1, now at index 30
		assertSelection(t, m, 30, 1)
	})
}

func TestTUIHideClosed(t *testing.T) {
	t.Run("DefaultFromConfig", func(t *testing.T) {
		tmpDir := setupTuiTestEnv(t)

		configPath := filepath.Join(tmpDir, "config.toml")
		if err := os.WriteFile(configPath, []byte("hide_addressed_by_default = true\n"), 0o644); err != nil {
			require.Condition(t, func() bool {
				return false
			}, "write config: %v", err)
		}

		m := newModel(testEndpoint)
		if !m.hideClosed {
			assert.Condition(t, func() bool {
				return false
			}, "hideClosed should be true when config sets hide_addressed_by_default = true")
		}
	})
	t.Run("Toggle", func(t *testing.T) {
		m := newModel(testEndpoint, withExternalIODisabled())
		m.currentView = viewQueue

		// Initial state: hideClosed is false (TestMain isolates from real config)
		if m.hideClosed {
			assert.Condition(t, func() bool {
				return false
			}, "hideClosed should be false initially")
		}

		// Press 'h' to toggle
		m2, _ := pressKey(m, 'h')

		if !m2.hideClosed {
			assert.Condition(t, func() bool {
				return false
			}, "hideClosed should be true after pressing 'h'")
		}

		// Press 'h' again to toggle back
		m3, _ := pressKey(m2, 'h')

		if m3.hideClosed {
			assert.Condition(t, func() bool {
				return false
			}, "hideClosed should be false after pressing 'h' again")
		}
	})
	t.Run("FiltersJobs", func(t *testing.T) {
		m := newModel(testEndpoint, withExternalIODisabled())
		m.currentView = viewQueue
		m.hideClosed = true

		m.jobs = []storage.ReviewJob{
			makeJob(1, withClosed(new(true))),                 // hidden: closed
			makeJob(2, withClosed(new(false))),                // visible
			makeJob(3, withStatus(storage.JobStatusFailed)),   // hidden: failed
			makeJob(4, withStatus(storage.JobStatusCanceled)), // hidden: canceled
			makeJob(5, withClosed(new(false))),                // visible
		}

		// Check visibility
		if m.isJobVisible(m.jobs[0]) {
			assert.Condition(t, func() bool {
				return false
			}, "Closed job should be hidden")
		}
		if !m.isJobVisible(m.jobs[1]) {
			assert.Condition(t, func() bool {
				return false
			}, "Open job should be visible")
		}
		if m.isJobVisible(m.jobs[2]) {
			assert.Condition(t, func() bool {
				return false
			}, "Failed job should be hidden")
		}
		if m.isJobVisible(m.jobs[3]) {
			assert.Condition(t, func() bool {
				return false
			}, "Canceled job should be hidden")
		}
		if !m.isJobVisible(m.jobs[4]) {
			assert.Condition(t, func() bool {
				return false
			}, "Open job should be visible")
		}

		// getVisibleJobs should only return 2 jobs
		visible := m.getVisibleJobs()
		if len(visible) != 2 {
			assert.Condition(t, func() bool {
				return false
			}, "Expected 2 visible jobs, got %d", len(visible))
		}
		if visible[0].ID != 2 || visible[1].ID != 5 {
			assert.Condition(t, func() bool {
				return false
			}, "Expected visible jobs 2 and 5, got %d and %d", visible[0].ID, visible[1].ID)
		}
	})
	t.Run("SelectionMovesToVisible", func(t *testing.T) {
		m := newModel(testEndpoint, withExternalIODisabled())
		m.currentView = viewQueue

		m.jobs = []storage.ReviewJob{
			makeJob(1, withClosed(new(true))),  // will be hidden
			makeJob(2, withClosed(new(false))), // will be visible
			makeJob(3, withClosed(new(false))), // will be visible
		}

		// Select the first job (closed)
		m.selectedIdx = 0
		m.selectedJobID = 1

		// Toggle hide closed
		m2, _ := pressKey(m, 'h')

		assertSelection(t, m2, -1, 0)
		assert.Empty(t, m2.jobs)
		assert.True(t, m2.loadingJobs)
	})
	t.Run("RefreshRevalidatesSelection", func(t *testing.T) {
		m := newModel(testEndpoint, withExternalIODisabled())
		m.currentView = viewQueue
		m.hideClosed = true

		m.jobs = []storage.ReviewJob{
			makeJob(1, withClosed(new(false))),
			makeJob(2, withClosed(new(false))),
		}
		m.selectedIdx = 0
		m.selectedJobID = 1

		// Simulate jobs refresh where job 1 is now closed

		m2, _ := updateModel(t, m, jobsMsg{
			jobs: []storage.ReviewJob{
				makeJob(1, withClosed(new(true))),  // now closed (hidden)
				makeJob(2, withClosed(new(false))), // still visible
			},
			hasMore: false,
		})

		// Selection should move to job 2 since job 1 is now hidden
		assertSelection(t, m2, 1, 2)
	})
	t.Run("RefreshPreservesSelectionInReviewView", func(t *testing.T) {
		// When in review view, a job refresh should NOT move selectedIdx
		// away from a closed job. Navigation (←/→) is relative to the
		// viewed job's position; normalizeSelectionIfHidden handles
		// adjustment when the user returns to the queue.
		m := newModel(testEndpoint, withExternalIODisabled())
		m.currentView = viewReview
		m.hideClosed = true

		m.jobs = []storage.ReviewJob{
			makeJob(1, withClosed(new(false))),
			makeJob(2, withClosed(new(false))),
			makeJob(3, withClosed(new(false))),
			makeJob(4, withClosed(new(false))),
			makeJob(5, withClosed(new(false))),
		}
		// User is viewing job 3 (index 2)
		m.selectedIdx = 2
		m.selectedJobID = 3
		m.currentReview = &storage.Review{
			ID:    100,
			JobID: 3,
			Job:   &storage.ReviewJob{ID: 3},
		}

		// Simulate closing the current review
		m.pendingClosed = map[int64]pendingState{3: {newState: true, seq: 1}}

		// Simulate a job refresh where job 3 is now closed
		m2, _ := updateModel(t, m, jobsMsg{
			jobs: []storage.ReviewJob{
				makeJob(1, withClosed(new(false))),
				makeJob(2, withClosed(new(false))),
				makeJob(3, withClosed(new(true))),
				makeJob(4, withClosed(new(false))),
				makeJob(5, withClosed(new(false))),
			},
			hasMore: false,
		})

		// Selection must stay at index 2 (job 3) so ←/→ navigation
		// continues from the user's current position.
		assertSelection(t, m2, 2, 3)
	})
	t.Run("NavigationSkipsHidden", func(t *testing.T) {
		m := newModel(testEndpoint, withExternalIODisabled())
		m.currentView = viewQueue
		m.hideClosed = true

		m.jobs = []storage.ReviewJob{
			makeJob(1, withClosed(new(false))),              // visible
			makeJob(2, withClosed(new(true))),               // hidden
			makeJob(3, withStatus(storage.JobStatusFailed)), // hidden
			makeJob(4, withClosed(new(false))),              // visible
		}
		m.selectedIdx = 0
		m.selectedJobID = 1

		// Navigate down - should skip jobs 2 and 3
		m2, _ := pressKey(m, 'j')

		assertSelection(t, m2, 3, 4)
	})
	t.Run("withRepoFilter", func(t *testing.T) {
		m := newModel(testEndpoint, withExternalIODisabled())
		m.currentView = viewQueue
		m.hideClosed = true
		m.activeRepoFilter = []string{"/repo/a"}

		m.jobs = []storage.ReviewJob{
			makeJob(1, withRepoPath("/repo/a"), withClosed(new(false))),              // visible: matches repo, not closed
			makeJob(2, withRepoPath("/repo/b"), withClosed(new(false))),              // hidden: wrong repo
			makeJob(3, withRepoPath("/repo/a"), withClosed(new(true))),               // hidden: closed
			makeJob(4, withRepoPath("/repo/a"), withStatus(storage.JobStatusFailed)), // hidden: failed
		}

		// Only job 1 should be visible
		visible := m.getVisibleJobs()
		if len(visible) != 1 {
			assert.Condition(t, func() bool {
				return false
			}, "Expected 1 visible job, got %d", len(visible))
		}
		if visible[0].ID != 1 {
			assert.Condition(t, func() bool {
				return false
			}, "Expected visible job ID=1, got %d", visible[0].ID)
		}
	})
	t.Run("ClearRepoFilterRefetches", func(t *testing.T) {
		// Scenario: hide closed enabled, then filter by repo, then press escape
		// to clear the repo filter. Should trigger a refetch to show all open reviews.
		m := newModel(testEndpoint, withExternalIODisabled())
		m.currentView = viewQueue
		m.hideClosed = true
		m.activeRepoFilter = []string{"/repo/a"}
		m.filterStack = []string{"repo"}
		m.loadingJobs = false // Simulate that initial load has completed

		m.jobs = []storage.ReviewJob{
			makeJob(1, withRepoPath("/repo/a"), withClosed(new(false))),
		}
		m.selectedIdx = 0
		m.selectedJobID = 1

		// Press escape to clear the repo filter
		m2, cmd := pressSpecial(m, tea.KeyEscape)

		// Repo filter should be cleared
		if m2.activeRepoFilter != nil {
			assert.Condition(t, func() bool {
				return false
			}, "Expected activeRepoFilter to be nil, got %v", m2.activeRepoFilter)
		}

		// hideClosed should still be true
		if !m2.hideClosed {
			assert.Condition(t, func() bool {
				return false
			}, "hideClosed should still be true after clearing repo filter")
		}

		// Filter stack should be empty
		if len(m2.filterStack) != 0 {
			assert.Condition(t, func() bool {
				return false
			}, "Expected empty filter stack, got %v", m2.filterStack)
		}

		assert.Empty(t, m2.jobs)

		// A refetch command should be returned
		if cmd == nil {
			assert.Condition(t, func() bool {
				return false
			}, "Expected a refetch command when clearing repo filter with hide-closed active")
		}

		// loadingJobs should be set
		if !m2.loadingJobs {
			assert.Condition(t, func() bool {
				return false
			}, "loadingJobs should be set when refetching after clearing repo filter")
		}
	})
	t.Run("EnableTriggersRefetch", func(t *testing.T) {
		m := newModel(testEndpoint, withExternalIODisabled())
		m.currentView = viewQueue
		m.hideClosed = false

		m.jobs = []storage.ReviewJob{
			makeJob(1, withClosed(new(false))),
		}
		m.selectedIdx = 0
		m.selectedJobID = 1

		// Toggle hide closed ON
		m2, cmd := pressKey(m, 'h')

		// hideClosed should be enabled
		if !m2.hideClosed {
			assert.Condition(t, func() bool {
				return false
			}, "hideClosed should be true after pressing 'h'")
		}

		// A command should be returned to fetch all jobs
		if cmd == nil {
			assert.Condition(t, func() bool {
				return false
			}, "Command should be returned to fetch all jobs when enabling hideClosed")
		}
	})
	t.Run("DisableRefetches", func(t *testing.T) {
		m := newModel(testEndpoint, withExternalIODisabled())
		m.currentView = viewQueue
		m.hideClosed = true // Already enabled

		m.jobs = []storage.ReviewJob{
			makeJob(1, withClosed(new(false))),
		}
		m.selectedIdx = 0
		m.selectedJobID = 1

		// Toggle hide closed OFF
		m2, cmd := pressKey(m, 'h')

		// hideClosed should be disabled
		if m2.hideClosed {
			assert.Condition(t, func() bool {
				return false
			}, "hideClosed should be false after pressing 'h' to disable")
		}

		// Disabling triggers a refetch to get previously-filtered closed jobs
		if cmd == nil {
			assert.Condition(t, func() bool {
				return false
			}, "Expected a refetch command when disabling hideClosed")
		}
	})
	t.Run("ValidConfigNotMutated", func(t *testing.T) {
		tmpDir := setupTuiTestEnv(t)

		// Write a valid config with the hide-closed default enabled
		validConfig := []byte("hide_addressed_by_default = true\n")
		configPath := filepath.Join(tmpDir, "config.toml")
		if err := os.WriteFile(configPath, validConfig, 0o644); err != nil {
			require.Condition(t, func() bool {
				return false
			}, "write config: %v", err)
		}

		m := newModel(testEndpoint)
		m.currentView = viewQueue

		// Verify the default was loaded
		if !m.hideClosed {
			require.Condition(t, func() bool {
				return false
			}, "hideClosed should be true from config")
		}

		// Toggle hide closed OFF
		m2, _ := pressKey(m, 'h')
		if m2.hideClosed {
			assert.Condition(t, func() bool {
				return false
			}, "hideClosed should be false after pressing 'h'")
		}

		// Toggle hide closed back ON
		m3, _ := pressKey(m2, 'h')
		if !m3.hideClosed {
			assert.Condition(t, func() bool {
				return false
			}, "hideClosed should be true after pressing 'h' again")
		}

		// Valid config file must not have been mutated by either toggle
		got, err := os.ReadFile(configPath)
		if err != nil {
			require.Condition(t, func() bool {
				return false
			}, "read config: %v", err)
		}
		if string(got) != string(validConfig) {
			assert.Condition(t, func() bool {
				return false
			}, "valid config was mutated:\n  before: %q\n  after:  %q", validConfig, got)
		}
	})
}

func TestTUIFlashMessage(t *testing.T) {
	t.Run("AppearsInQueueView", func(t *testing.T) {
		m := newModel(testEndpoint, withExternalIODisabled())
		m.currentView = viewQueue
		m.width = 80
		m.height = 24
		m.flashMessage = "Copied to clipboard"
		m.flashExpiresAt = time.Now().Add(2 * time.Second)
		m.flashView = viewQueue // Flash was triggered in queue view

		output := m.renderQueueView()
		if !strings.Contains(output, "Copied to clipboard") {
			assert.Condition(t, func() bool {
				return false
			}, "Expected flash message to appear in queue view")
		}
	})
	t.Run("NotShownInDifferentView", func(t *testing.T) {
		m := newModel(testEndpoint, withExternalIODisabled())
		m.currentView = viewReview
		m.width = 80
		m.height = 24
		m.flashMessage = "Copied to clipboard"
		m.flashExpiresAt = time.Now().Add(2 * time.Second)
		m.flashView = viewQueue // Flash was triggered in queue view, not review view
		m.currentReview = makeReview(1, &storage.ReviewJob{}, withReviewOutput("Test review content"))

		output := m.renderReviewView()
		if strings.Contains(output, "Copied to clipboard") {
			assert.Condition(t, func() bool {
				return false
			}, "Flash message should not appear when viewing different view than where it was triggered")
		}
	})
}

func TestNewTuiModelOptions(t *testing.T) {
	tests := []struct {
		name                 string
		opts                 []option
		expectedRepoFilter   []string
		expectedBranchFilter string
		expectedFilterStack  []string
		expectedLockedRepo   bool
		expectedLockedBranch bool
		expectedDaemonVer    string
	}{
		{
			name:                 "withExternalIODisabled only",
			opts:                 []option{withExternalIODisabled()},
			expectedRepoFilter:   nil,
			expectedBranchFilter: "",
			expectedFilterStack:  nil,
			expectedLockedRepo:   false,
			expectedLockedBranch: false,
			expectedDaemonVer:    "?",
		},
		{
			name:                 "With RepoFilter",
			opts:                 []option{withExternalIODisabled(), withRepoFilter("/path/to/repo")},
			expectedRepoFilter:   []string{"/path/to/repo"},
			expectedBranchFilter: "",
			expectedFilterStack:  []string{filterTypeRepo},
			expectedLockedRepo:   true,
			expectedLockedBranch: false,
			expectedDaemonVer:    "?",
		},
		{
			name:                 "With BranchFilter",
			opts:                 []option{withExternalIODisabled(), withBranchFilter("feature-branch")},
			expectedRepoFilter:   nil,
			expectedBranchFilter: "feature-branch",
			expectedFilterStack:  []string{filterTypeBranch},
			expectedLockedRepo:   false,
			expectedLockedBranch: true,
			expectedDaemonVer:    "?",
		},
		{
			name:                 "With RepoFilter and BranchFilter",
			opts:                 []option{withExternalIODisabled(), withRepoFilter("/path/to/repo"), withBranchFilter("feature-branch")},
			expectedRepoFilter:   []string{"/path/to/repo"},
			expectedBranchFilter: "feature-branch",
			expectedFilterStack:  []string{filterTypeRepo, filterTypeBranch},
			expectedLockedRepo:   true,
			expectedLockedBranch: true,
			expectedDaemonVer:    "?",
		},
		{
			name:                 "With AutoFilterBranch",
			opts:                 []option{withExternalIODisabled(), withAutoFilterBranch("feat/my-worktree")},
			expectedRepoFilter:   nil,
			expectedBranchFilter: "feat/my-worktree",
			expectedFilterStack:  []string{filterTypeBranch},
			expectedLockedRepo:   false,
			expectedLockedBranch: false,
			expectedDaemonVer:    "?",
		},
		{
			name:                 "With AutoFilterRepo and AutoFilterBranch",
			opts:                 []option{withExternalIODisabled(), withAutoFilterRepo("/path/to/repo"), withAutoFilterBranch("feat/my-worktree")},
			expectedRepoFilter:   []string{"/path/to/repo"},
			expectedBranchFilter: "feat/my-worktree",
			expectedFilterStack:  []string{filterTypeRepo, filterTypeBranch},
			expectedLockedRepo:   false,
			expectedLockedBranch: false,
			expectedDaemonVer:    "?",
		},
		{
			name:                 "BranchFilter flag overrides AutoFilterBranch",
			opts:                 []option{withExternalIODisabled(), withAutoFilterBranch("feat/auto"), withBranchFilter("feat/manual")},
			expectedRepoFilter:   nil,
			expectedBranchFilter: "feat/manual",
			expectedFilterStack:  []string{filterTypeBranch},
			expectedLockedRepo:   false,
			expectedLockedBranch: true,
			expectedDaemonVer:    "?",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := newModel(testEndpoint, tc.opts...)

			if !reflect.DeepEqual(m.activeRepoFilter, tc.expectedRepoFilter) {
				assert.Condition(t, func() bool {
					return false
				}, "activeRepoFilter mismatch: got %v, want %v", m.activeRepoFilter, tc.expectedRepoFilter)
			}
			if m.activeBranchFilter != tc.expectedBranchFilter {
				assert.Condition(t, func() bool {
					return false
				}, "activeBranchFilter mismatch: got %q, want %q", m.activeBranchFilter, tc.expectedBranchFilter)
			}
			if !reflect.DeepEqual(m.filterStack, tc.expectedFilterStack) {
				assert.Condition(t, func() bool {
					return false
				}, "filterStack mismatch: got %v, want %v", m.filterStack, tc.expectedFilterStack)
			}
			if m.lockedRepoFilter != tc.expectedLockedRepo {
				assert.Condition(t, func() bool {
					return false
				}, "lockedRepoFilter mismatch: got %v, want %v", m.lockedRepoFilter, tc.expectedLockedRepo)
			}
			if m.lockedBranchFilter != tc.expectedLockedBranch {
				assert.Condition(t, func() bool {
					return false
				}, "lockedBranchFilter mismatch: got %v, want %v", m.lockedBranchFilter, tc.expectedLockedBranch)
			}
			if m.daemonVersion != tc.expectedDaemonVer {
				assert.Condition(t, func() bool {
					return false
				}, "daemonVersion mismatch: got %q, want %q", m.daemonVersion, tc.expectedDaemonVer)
			}
		})
	}
}

func TestSSEEventTriggersRefresh(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.sseCh = make(chan struct{}, 1)
	m.loadingJobs = false

	m, cmd := updateModel(t, m, sseEventMsg{})

	assert.NotNil(t, cmd, "expected commands from SSE event")
}

func TestSSEEventSkipsRefreshWhileLoading(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.sseCh = make(chan struct{}, 1)
	m.loadingJobs = true

	m, cmd := updateModel(t, m, sseEventMsg{})

	assert.NotNil(t, cmd, "expected re-subscribe command even while loading")
}

func TestSSEDisabledInTestMode(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())

	assert.Nil(t, m.sseCh, "sseCh should be nil when external IO is disabled")
	assert.Nil(t, m.sseStop, "sseStop should be nil when external IO is disabled")
}

func TestSSEPendingRefreshStateMachine(t *testing.T) {
	t.Run("set during loadingJobs and drained on jobs completion", func(t *testing.T) {
		assert := assert.New(t)
		m := newModel(localhostEndpoint, withExternalIODisabled())
		m.sseCh = make(chan struct{}, 1)
		m.sseStop = make(chan struct{})
		m.loadingJobs = true

		m, _ = updateModel(t, m, sseEventMsg{})
		assert.True(m.ssePendingRefresh, "expected ssePendingRefresh to be set")

		m, cmd := updateModel(t, m, jobsMsg{seq: m.fetchSeq})
		assert.False(m.ssePendingRefresh, "expected ssePendingRefresh to be cleared")
		assert.NotNil(cmd, "expected deferred refresh command")
	})

	t.Run("set during loadingMore and drained on pagination completion", func(t *testing.T) {
		assert := assert.New(t)
		m := newModel(localhostEndpoint, withExternalIODisabled())
		m.sseCh = make(chan struct{}, 1)
		m.sseStop = make(chan struct{})
		m.loadingMore = true

		m, _ = updateModel(t, m, sseEventMsg{})
		assert.True(m.ssePendingRefresh)

		m, cmd := updateModel(t, m, jobsMsg{append: true, seq: m.fetchSeq})
		assert.False(m.ssePendingRefresh)
		assert.NotNil(cmd)
	})

	t.Run("not set when no fetch in flight", func(t *testing.T) {
		assert := assert.New(t)
		m := newModel(localhostEndpoint, withExternalIODisabled())
		m.sseCh = make(chan struct{}, 1)
		m.sseStop = make(chan struct{})
		m.loadingJobs = false

		m, _ = updateModel(t, m, sseEventMsg{})
		assert.False(m.ssePendingRefresh, "should not set flag when not loading")
	})
}
