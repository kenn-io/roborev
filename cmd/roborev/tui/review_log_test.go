package tui

import (
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/mattn/go-runewidth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/streamfmt"
)

func collectMsgs(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if msg == nil {
		return nil
	}
	if batch, ok := msg.(tea.BatchMsg); ok {
		var all []tea.Msg
		for _, c := range batch {
			all = append(all, collectMsgs(c)...)
		}
		return all
	}
	return []tea.Msg{msg}
}

func hasMsgType(msgs []tea.Msg, typeName string) bool {
	for _, msg := range msgs {
		if fmt.Sprintf("%T", msg) == typeName {
			return true
		}
	}
	return false
}

func TestTUILogVisibleLinesWithCommandHeader(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.height = 30
	m.logJobID = 1

	m.jobs = []storage.ReviewJob{
		makeJob(1, withAgent("")),
	}
	noHeader := m.logVisibleLines()

	m.jobs = []storage.ReviewJob{
		makeJob(1, withAgent("test")),
	}
	withHeader := m.logVisibleLines()

	assert.Equal(t, 1, noHeader-withHeader)
}

func TestTUILogPagingUsesLogVisibleLines(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.currentView = viewLog
	m.logJobID = 1
	m.height = 20
	m.logScroll = 0
	m.logFollow = false

	m.jobs = []storage.ReviewJob{
		makeJob(1, withAgent("test")),
	}

	for i := range 50 {
		m.logLines = append(m.logLines,
			logLine{text: fmt.Sprintf("line %d", i)})
	}

	visLines := m.logVisibleLines()

	m2, _ := pressSpecial(m, tea.KeyPgDown)
	assert.Equal(t, m2.logScroll, visLines)

	m3, _ := pressSpecial(m, tea.KeyEnd)
	expectedMax := max(50-visLines, 0)
	assert.Equal(t, expectedMax, m3.logScroll)

	m4, _ := pressKeys(m, []rune{'g'})
	assert.Equal(t, expectedMax, m4.logScroll)

	mMid := m
	mMid.logScroll = 2 * visLines
	m5, _ := pressSpecial(mMid, tea.KeyPgUp)
	assert.Equal(t, m5.logScroll, visLines)

	m6, _ := pressSpecial(m, tea.KeyPgUp)
	assert.Equal(t, 0, m6.logScroll)
}

func TestTUILogPagingNoHeader(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.currentView = viewLog
	m.logJobID = 1
	m.height = 20
	m.logScroll = 0
	m.logFollow = false

	m.jobs = []storage.ReviewJob{
		makeJob(1, withAgent("")),
	}

	for i := range 50 {
		m.logLines = append(m.logLines,
			logLine{text: fmt.Sprintf("line %d", i)})
	}

	visLines := m.logVisibleLines()
	expectedMax := max(50-visLines, 0)

	m2, _ := pressSpecial(m, tea.KeyPgDown)
	assert.Equal(t, m2.logScroll, visLines)

	m3, _ := pressSpecial(m, tea.KeyEnd)
	assert.Equal(t, expectedMax, m3.logScroll)
}

func TestTUILogLoadingGuard(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.currentView = viewLog
	m.logJobID = 1
	m.logStreaming = true
	m.logLoading = true
	m.height = 30

	m2, cmd := updateModel(t, m, logTickMsg{})

	assert.Nil(t, cmd)
	assert.True(t, m2.logLoading)
}

func TestTUILogFetchWrapsChunkedGrokTextAtPaneWidth(t *testing.T) {
	// If adjacent Grok response chunks are rendered as separate messages,
	// wide TUI log panes show one token per row and hide most of the review.
	const response = "This commit is an empty live fire probe."
	events := strings.Join([]string{
		`{"type":"text","data":"This"}`,
		`{"type":"text","data":" commit"}`,
		`{"type":"text","data":" is an"}`,
		`{"type":"text","data":" empty live"}`,
		`{"type":"text","data":" fire probe."}`,
		`{"type":"end","stopReason":"EndTurn"}`,
	}, "\n") + "\n"

	tests := []struct {
		name      string
		width     int
		wantLines int
	}{
		{name: "wide pane", width: 80, wantLines: 1},
		{name: "narrow pane", width: 18, wantLines: 3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			_, m := mockServerModel(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("X-Job-Status", "done")
				_, _ = fmt.Fprint(w, events)
			})
			m.width = tt.width
			m.height = 24
			job := storage.ReviewJob{
				ID: 42, Status: storage.JobStatusDone, Agent: "grok",
			}
			opened, _ := m.openLogView(job, viewQueue)
			m = opened.(model)

			msg, ok := m.fetchJobLog(42)().(logOutputMsg)
			require.True(t, ok)
			require.NoError(t, msg.err)
			require.NotNil(t, msg.fmtr)
			assert.Equal(tt.width, msg.fmtr.Width())

			var lines []string
			for _, line := range msg.lines {
				plain := strings.TrimSpace(streamfmt.StripANSI(line.text))
				if plain == "" {
					continue
				}
				lines = append(lines, plain)
				assert.LessOrEqual(runewidth.StringWidth(plain), tt.width)
			}

			assert.Equal(response, strings.Join(lines, " "))
			assert.Len(lines, tt.wantLines)
		})
	}
}

// If the full-screen log opener drops the selected agent, provider-shaped
// frames can be interpreted with the wrong protocol or hidden from users who
// need unknown-agent output for diagnosis.
func TestTUILogFetchUsesOpenedJobIdentity(t *testing.T) {
	const grokLine = `{"type":"text","data":"wrong provider"}`
	tests := []struct {
		name  string
		agent string
		want  string
	}{
		{name: "known agent suppresses another provider", agent: "codex"},
		{name: "unknown agent stays literal", agent: "future-agent", want: grokLine},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, m := mockServerModel(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("X-Job-Status", "done")
				_, _ = fmt.Fprintln(w, grokLine)
			})
			m.width = 200
			job := storage.ReviewJob{
				ID: 42, Status: storage.JobStatusDone, Agent: tt.agent,
			}
			opened, _ := m.openLogView(job, viewQueue)
			m = opened.(model)

			msg, ok := m.fetchJobLog(job.ID)().(logOutputMsg)
			require.True(t, ok)
			require.NoError(t, msg.err)
			plain := strings.Join(plainLogLines(msg.lines), "\n")
			if tt.want == "" {
				assert.Empty(t, plain)
			} else {
				assert.Equal(t, tt.want, plain)
			}
		})
	}
}

// If a retry changes provider on the same job row, the log response identity
// must replace the decoder captured when the view first opened.
func TestTUILogFetchRefreshesIdentityAfterFailover(t *testing.T) {
	const response = "replacement provider output"
	_, m := mockServerModel(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "codex", r.Header.Get("X-Job-Agent"))
		w.Header().Set("X-Job-Agent", "grok")
		w.Header().Set("X-Job-Status", "done")
		w.Header().Set("X-Log-Offset", "100")
		_, _ = fmt.Fprintln(w, `{"type":"text","data":"replacement provider output"}`)
		_, _ = fmt.Fprintln(w, `{"type":"end"}`)
	})
	m.width, m.height = 80, 24
	job := storage.ReviewJob{
		ID: 42, Status: storage.JobStatusRunning, Agent: "codex",
	}
	opened, _ := m.openLogView(job, viewQueue)
	m = opened.(model)
	m.logOffset = 50

	msg, ok := m.fetchJobLog(job.ID)().(logOutputMsg)
	require.True(t, ok)
	require.NoError(t, msg.err)
	m, _ = updateModel(t, m, msg)

	assert.Equal(t, "grok", m.logAgent)
	assert.Equal(t, []string{response}, plainLogLines(m.logLines))
}

// If source identity is ignored, archived auto-design logs lose either the
// classifier output or the appended design output, while ordinary logs regain
// protocol-shape guessing.
func TestTUILogFetchUsesMixedDecoderOnlyForAutoDesign(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"item.completed","item":{"type":"agent_message","text":"classifier"}}`,
		`{"type":"text","data":"design"}`,
		`{"type":"end"}`,
	}, "\n") + "\n"
	tests := []struct {
		name           string
		source         string
		wantClassifier bool
	}{
		{name: "ordinary log", source: ""},
		{name: "auto design mixed log", source: storage.JobSourceAutoDesign, wantClassifier: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, m := mockServerModel(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("X-Job-Status", "done")
				_, _ = fmt.Fprint(w, input)
			})
			m.width = 80
			job := storage.ReviewJob{
				ID: 42, Status: storage.JobStatusDone,
				Agent: "grok", Source: tt.source,
			}
			opened, _ := m.openLogView(job, viewQueue)
			m = opened.(model)

			msg, ok := m.fetchJobLog(job.ID)().(logOutputMsg)
			require.True(t, ok)
			require.NoError(t, msg.err)
			plain := strings.Join(plainLogLines(msg.lines), "\n")
			assert.Contains(t, plain, "design")
			assert.Equal(t, tt.wantClassifier, strings.Contains(plain, "classifier"))
		})
	}
}

func TestTUILogFetchKeepsGrokTextChunksTogetherAcrossPolls(t *testing.T) {
	// If a live-log poll finalizes Grok text before the response event ends,
	// users still get one short row per poll after the job completes.
	const response = "This commit is an empty live fire probe."
	_, m := mockServerModel(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("offset") {
		case "0":
			w.Header().Set("X-Job-Status", "running")
			w.Header().Set("X-Log-Offset", "40")
			_, _ = fmt.Fprint(w, strings.Join([]string{
				`{"type":"text","data":"This"}`,
				`{"type":"text","data":" commit"}`,
			}, "\n")+"\n")
		case "40":
			w.Header().Set("X-Job-Status", "done")
			w.Header().Set("X-Log-Offset", "100")
			_, _ = fmt.Fprint(w, strings.Join([]string{
				`{"type":"text","data":" is an empty"}`,
				`{"type":"text","data":" live fire probe."}`,
				`{"type":"end","stopReason":"EndTurn"}`,
			}, "\n")+"\n")
		default:
			http.Error(w, "unexpected offset", http.StatusBadRequest)
		}
	})
	m.width = 80
	m.height = 24
	job := storage.ReviewJob{
		ID: 42, Status: storage.JobStatusRunning, Agent: "grok",
	}
	opened, _ := m.openLogView(job, viewQueue)
	m = opened.(model)

	first, ok := m.fetchJobLog(42)().(logOutputMsg)
	require.True(t, ok)
	require.NoError(t, first.err)
	m, _ = updateModel(t, m, first)

	second, ok := m.fetchJobLog(42)().(logOutputMsg)
	require.True(t, ok)
	require.NoError(t, second.err)
	require.NotNil(t, second.fmtr)
	assert.Equal(t, 80, second.fmtr.Width())
	m, _ = updateModel(t, m, second)

	assert.Equal(t, []string{response}, plainLogLines(m.logLines))
}

func TestTUILogEmptyTerminalPollFlushesBufferedGrokText(t *testing.T) {
	// If the final poll returns no new bytes, it still marks the boundary
	// where the persistent decoder must release text buffered by earlier
	// running polls.
	const response = "buffered until the terminal poll"
	_, m := mockServerModel(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("offset") {
		case "0":
			w.Header().Set("X-Job-Status", "running")
			w.Header().Set("X-Log-Offset", "50")
			_, _ = fmt.Fprintf(w, `{"type":"text","data":%q}`+"\n", response)
		case "50":
			w.Header().Set("X-Job-Status", "done")
			w.Header().Set("X-Log-Offset", "50")
		default:
			http.Error(w, "unexpected offset", http.StatusBadRequest)
		}
	})
	m.width, m.height = 80, 24
	job := storage.ReviewJob{
		ID: 42, Status: storage.JobStatusRunning, Agent: "grok",
	}
	opened, _ := m.openLogView(job, viewQueue)
	m = opened.(model)

	first, ok := m.fetchJobLog(job.ID)().(logOutputMsg)
	require.True(t, ok)
	require.NoError(t, first.err)
	assert.Empty(t, first.lines)
	m, _ = updateModel(t, m, first)

	final, ok := m.fetchJobLog(job.ID)().(logOutputMsg)
	require.True(t, ok)
	require.NoError(t, final.err)
	m, cmd := updateModel(t, m, final)

	assert.Equal(t, []string{response}, plainLogLines(m.logLines))
	assert.False(t, m.logStreaming)
	assert.Nil(t, cmd, "a terminal poll must not schedule another fetch")
}

func TestTUILogOffsetResetClearsRowsBeforeBufferedReplacement(t *testing.T) {
	// A reset can return a smaller, nonzero replacement chunk whose Grok
	// text is still buffered. The absence of rendered rows must not leave
	// pre-reset rows visible.
	_, m := mockServerModel(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "100", r.URL.Query().Get("offset"))
		w.Header().Set("X-Job-Status", "running")
		w.Header().Set("X-Log-Offset", "50")
		_, _ = fmt.Fprintln(w, `{"type":"text","data":"fresh replacement"}`)
	})
	m.width, m.height = 80, 24
	job := storage.ReviewJob{
		ID: 42, Status: storage.JobStatusRunning, Agent: "grok",
	}
	opened, _ := m.openLogView(job, viewQueue)
	m = opened.(model)
	m.logOffset = 100
	m.logLines = []logLine{{text: "stale row before truncation"}}

	msg, ok := m.fetchJobLog(job.ID)().(logOutputMsg)
	require.True(t, ok)
	require.NoError(t, msg.err)
	require.False(t, msg.append)
	require.Empty(t, msg.lines)
	m, _ = updateModel(t, m, msg)

	assert.Empty(t, m.logLines)
	assert.Equal(t, int64(50), m.logOffset)
	assert.True(t, m.logStreaming)
}

// If an offset reset rebuilds the formatter without the retained source, a
// replacement auto-design log drops the classifier half of the mixed stream.
func TestTUILogFetchKeepsIdentityOnOffsetReset(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"item.completed","item":{"type":"agent_message","text":"classifier after reset"}}`,
		`{"type":"text","data":"design after reset"}`,
		`{"type":"end"}`,
	}, "\n") + "\n"
	_, m := mockServerModel(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "100", r.URL.Query().Get("offset"))
		w.Header().Set("X-Job-Status", "done")
		w.Header().Set("X-Log-Offset", "50")
		_, _ = fmt.Fprint(w, input)
	})
	m.width = 100
	job := storage.ReviewJob{
		ID: 42, Status: storage.JobStatusRunning,
		Agent: "grok", Source: storage.JobSourceAutoDesign,
	}
	opened, _ := m.openLogView(job, viewQueue)
	m = opened.(model)
	m.logOffset = 100

	msg, ok := m.fetchJobLog(job.ID)().(logOutputMsg)
	require.True(t, ok)
	require.NoError(t, msg.err)
	assert.False(t, msg.append)
	plain := strings.Join(plainLogLines(msg.lines), "\n")
	assert.Contains(t, plain, "classifier after reset")
	assert.Contains(t, plain, "design after reset")
}

// If a resize rebuild drops source identity, the freshly rendered full log no
// longer shows both halves of an auto-design stream.
func TestTUILogResizeKeepsOpenedJobIdentity(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"item.completed","item":{"type":"agent_message","text":"classifier after resize"}}`,
		`{"type":"text","data":"design after resize"}`,
		`{"type":"end"}`,
	}, "\n") + "\n"
	_, m := mockServerModel(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Job-Status", "done")
		_, _ = fmt.Fprint(w, input)
	})
	m.width = 100
	m.height = 24
	job := storage.ReviewJob{
		ID: 42, Status: storage.JobStatusDone,
		Agent: "grok", Source: storage.JobSourceAutoDesign,
	}
	opened, _ := m.openLogView(job, viewQueue)
	m = opened.(model)
	m.logLines = []logLine{{text: "stale width"}}

	res, cmd := m.handleWindowSizeMsg(tea.WindowSizeMsg{Width: 120, Height: 24})
	m = res.(model)
	require.NotNil(t, cmd)
	var fetched *logOutputMsg
	for _, raw := range collectMsgs(cmd) {
		if msg, ok := raw.(logOutputMsg); ok {
			msgCopy := msg
			fetched = &msgCopy
		}
	}
	require.NotNil(t, fetched)
	require.NoError(t, fetched.err)
	require.NotNil(t, fetched.fmtr)
	assert.Equal(t, 120, fetched.fmtr.Width())
	m, _ = updateModel(t, m, *fetched)
	plain := strings.Join(plainLogLines(m.logLines), "\n")
	assert.Contains(t, plain, "classifier after resize")
	assert.Contains(t, plain, "design after resize")
}

func TestTUILogResizeRebuildsBufferedDecoderAlongsideQueueRefill(t *testing.T) {
	// A running Grok stream can have decoder state but no visible rows. A
	// resize must still invalidate that fetch session and re-render the full
	// log at the new width, even when the taller viewport also needs jobs.
	const response = "This buffered response must wrap using the resized log width."
	var logRequests atomic.Int32
	var jobsRequests atomic.Int32
	_, m := mockServerModel(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/job/log":
			switch logRequests.Add(1) {
			case 1:
				w.Header().Set("X-Job-Status", "running")
				w.Header().Set("X-Log-Offset", "50")
				_, _ = fmt.Fprintln(w, `{"type":"text","data":"This buffered response"}`)
			default:
				w.Header().Set("X-Job-Status", "done")
				w.Header().Set("X-Log-Offset", "100")
				_, _ = fmt.Fprintf(w, `{"type":"text","data":%q}`+"\n", response)
				_, _ = fmt.Fprintln(w, `{"type":"end"}`)
			}
		case "/api/jobs":
			jobsRequests.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"jobs":[],"has_more":false}`)
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{}`)
		}
	})
	m.width, m.height = 80, 24
	job := storage.ReviewJob{
		ID: 42, Status: storage.JobStatusRunning, Agent: "grok",
	}
	m.jobs = []storage.ReviewJob{job}
	m.selectedIdx, m.selectedJobID = 0, job.ID
	opened, _ := m.openLogView(job, viewQueue)
	m = opened.(model)

	first, ok := m.fetchJobLog(job.ID)().(logOutputMsg)
	require.True(t, ok)
	require.NoError(t, first.err)
	m, _ = updateModel(t, m, first)
	require.Empty(t, m.logLines)
	require.Equal(t, int64(50), m.logOffset)
	oldSeq := m.logFetchSeq
	oldFetch := m.fetchJobLog(job.ID)
	m.hasMore = true
	m.loadingJobs = false
	m.loadingMore = false

	res, cmd := m.handleWindowSizeMsg(tea.WindowSizeMsg{Width: 24, Height: 80})
	m = res.(model)
	require.NotNil(t, cmd)
	assert.Greater(t, m.logFetchSeq, oldSeq)
	assert.Equal(t, 24, m.logFmtr.Width())
	assert.Equal(t, int64(0), m.logOffset)
	stale, ok := oldFetch().(logOutputMsg)
	require.True(t, ok)
	require.NoError(t, stale.err)
	m, _ = updateModel(t, m, stale)
	assert.Empty(t, m.logLines, "the pre-resize response must be rejected")

	var resized *logOutputMsg
	for _, raw := range collectMsgs(cmd) {
		if msg, ok := raw.(logOutputMsg); ok {
			copy := msg
			resized = &copy
		}
	}
	require.NotNil(t, resized)
	require.NoError(t, resized.err)
	m, _ = updateModel(t, m, *resized)

	lines := plainLogLines(m.logLines)
	assert.Equal(t, response, strings.Join(lines, " "))
	for _, line := range lines {
		assert.LessOrEqual(t, runewidth.StringWidth(line), 24)
	}
	assert.Equal(t, int32(3), logRequests.Load())
	assert.Equal(t, int32(1), jobsRequests.Load())
}

func plainLogLines(lines []logLine) []string {
	plain := make([]string, 0, len(lines))
	for _, line := range lines {
		text := strings.TrimSpace(streamfmt.StripANSI(line.text))
		if text != "" {
			plain = append(plain, text)
		}
	}
	return plain
}

func TestTUILogErrorDroppedOutsideLogView(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.currentView = viewQueue
	m.logFetchSeq = 3
	m.logLoading = true

	msg := logOutputMsg{
		err: fmt.Errorf("connection reset"),
		seq: 3,
	}

	m2, _ := updateModel(t, m, msg)

	require.NoError(t, m2.err)
	assert.False(t, m2.logLoading)
}

func TestTUILogViewLookupFixJob(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.currentView = viewLog
	m.logJobID = 42
	m.logFromView = viewTasks
	m.logStreaming = true
	m.height = 30
	m.width = 80

	m.fixJobs = []storage.ReviewJob{
		{
			ID:     42,
			Status: storage.JobStatusRunning,
			Agent:  "codex",
			GitRef: "abc1234",
		},
	}
	m.logLines = []logLine{{text: "output"}}

	view := m.View().Content
	assert.Contains(t, view, "#42")
	assert.Contains(t, view, "codex")
}

func TestTUILogCancelFixJob(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.currentView = viewLog
	m.logJobID = 42
	m.logFromView = viewTasks
	m.logStreaming = true
	m.height = 30

	m.fixJobs = []storage.ReviewJob{
		{
			ID:     42,
			Status: storage.JobStatusRunning,
			Agent:  "codex",
		},
	}

	m2, cmd := pressKey(m, 'x')

	assert.False(t, m2.logStreaming)
	assert.Equal(t, storage.JobStatusCanceled, m2.fixJobs[0].Status, "fix job status should be canceled, got %s", m2.fixJobs[0].Status)

	assert.NotNil(t, cmd, "expected command from cancel action")
}

func TestTUILogVisibleLinesFixJob(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.currentView = viewLog
	m.logJobID = 42
	m.logFromView = viewTasks
	m.height = 30

	m.fixJobs = []storage.ReviewJob{
		{ID: 42, Status: storage.JobStatusRunning, Agent: "test"},
	}

	m.jobs = nil

	visWithCmd := m.logVisibleLines()

	m.logLines = []logLine{{text: "output"}}
	m.width = 80
	view := m.View().Content
	hasCmd := strings.Contains(view, "Command:")

	if !hasCmd {
		assert.Contains(t, view, "Command:", "expected Command: header in rendered view")
	}

	m.fixJobs = []storage.ReviewJob{
		{ID: 42, Status: storage.JobStatusRunning},
	}
	visWithout := m.logVisibleLines()
	assert.Equal(t, visWithout-1, visWithCmd, "logVisibleLines mismatch: with cmd=%d, without=%d (expected difference of 1)", visWithCmd, visWithout)
}

func TestTUILogNavFromTasks(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.currentView = viewLog
	m.logJobID = 20
	m.logFromView = viewTasks
	m.logStreaming = false
	m.height = 30
	m.fixSelectedIdx = 1

	m.fixJobs = []storage.ReviewJob{
		{ID: 10, Status: storage.JobStatusDone},
		{ID: 20, Status: storage.JobStatusRunning},
		{ID: 30, Status: storage.JobStatusFailed},
	}

	m.jobs = []storage.ReviewJob{
		{ID: 100, Status: storage.JobStatusDone},
		{ID: 200, Status: storage.JobStatusDone},
	}
	m.selectedIdx = 0

	m2, cmd := pressSpecial(m, tea.KeyLeft)
	require.NotNil(t, cmd, "expected command from left arrow nav")
	assert.Equal(t, 2, m2.fixSelectedIdx, "fixSelectedIdx should be 2, got %d", m2.fixSelectedIdx)
	assert.Equal(t, 0, m2.selectedIdx, "selectedIdx should remain 0, got %d", m2.selectedIdx)

	m3, cmd := pressSpecial(m, tea.KeyRight)
	require.NotNil(t, cmd, "expected command from right arrow nav")
	assert.Equal(t, 0, m3.fixSelectedIdx, "fixSelectedIdx should be 0, got %d", m3.fixSelectedIdx)
}

func TestTUILogOutputTable(t *testing.T) {
	dummyFmtr := &streamfmt.Formatter{}

	tests := []struct {
		name             string
		initialView      viewKind
		initialJobStatus storage.JobStatus
		initialJobError  string
		initialLogLines  []logLine
		initialStreaming bool
		initialFetchSeq  uint64
		initialLoading   bool
		initialOffset    int64
		initialFmtr      *streamfmt.Formatter

		msg logOutputMsg

		wantView       viewKind
		wantLinesLen   int
		wantLines      []string
		wantLinesNil   bool
		wantLinesEmpty bool
		wantStreaming  bool
		wantFlashMsg   string
		wantLoading    bool
		wantOffset     int64
		wantFmtr       *streamfmt.Formatter
	}{
		{
			name:             "updates formatter from message",
			initialView:      viewLog,
			initialStreaming: true,
			msg:              logOutputMsg{fmtr: dummyFmtr, hasMore: true, append: true},
			wantView:         viewLog,
			wantFmtr:         dummyFmtr,
			wantStreaming:    true,
			wantLinesNil:     true,
		},
		{
			name:             "persists formatter when message has none",
			initialView:      viewLog,
			initialStreaming: true,
			initialFmtr:      dummyFmtr,
			msg:              logOutputMsg{hasMore: true, append: true},
			wantView:         viewLog,
			wantFmtr:         dummyFmtr,
			wantStreaming:    true,
			wantLinesNil:     true,
		},
		{
			name:             "preserves lines on empty response",
			initialView:      viewLog,
			initialStreaming: true,
			initialFetchSeq:  1,
			initialLogLines:  []logLine{{text: "Line 1"}, {text: "Line 2"}, {text: "Line 3"}},
			msg:              logOutputMsg{lines: []logLine{}, hasMore: false, err: nil, append: true, seq: 1},
			wantView:         viewLog,
			wantLinesLen:     3,
			wantLines:        []string{"Line 1", "Line 2", "Line 3"},
			wantStreaming:    false,
		},
		{
			name:             "updates lines when streaming",
			initialView:      viewLog,
			initialStreaming: true,
			initialLogLines:  []logLine{{text: "Old line"}},
			msg:              logOutputMsg{lines: []logLine{{text: "Old line"}, {text: "New line"}}, hasMore: true, err: nil},
			wantView:         viewLog,
			wantLinesLen:     2,
			wantLines:        []string{"Old line", "New line"},
			wantStreaming:    true,
		},
		{
			name:             "err no log shows job error",
			initialView:      viewLog,
			initialJobStatus: storage.JobStatusFailed,
			initialJobError:  "agent timeout after 300s",
			initialStreaming: false,
			msg:              logOutputMsg{err: errNoLog},
			wantView:         viewQueue,
			wantFlashMsg:     "agent timeout",
			wantLinesNil:     true,
		},
		{
			name:             "err no log generic for non failed",
			initialView:      viewLog,
			initialJobStatus: storage.JobStatusDone,
			msg:              logOutputMsg{err: errNoLog},
			wantView:         viewQueue,
			wantFlashMsg:     "No log available for this job",
			wantLinesNil:     true,
		},
		{
			name:             "running job keeps waiting",
			initialView:      viewLog,
			initialStreaming: true,
			initialLogLines:  nil,
			msg:              logOutputMsg{lines: nil, hasMore: true},
			wantView:         viewLog,
			wantLinesLen:     0,
			wantLinesNil:     true,
			wantStreaming:    true,
		},
		{
			name:            "ignored when not in log view",
			initialView:     viewQueue,
			initialLogLines: []logLine{{text: "Previous session line"}},
			msg:             logOutputMsg{lines: []logLine{{text: "Should be ignored"}}, hasMore: false, err: nil},
			wantView:        viewQueue,
			wantLinesLen:    1,
			wantLines:       []string{"Previous session line"},
		},
		{
			name:             "append mode",
			initialView:      viewLog,
			initialStreaming: true,
			initialLogLines:  []logLine{{text: "Line 1"}, {text: "Line 2"}},
			initialOffset:    100,
			msg:              logOutputMsg{lines: []logLine{{text: "Line 3"}, {text: "Line 4"}}, hasMore: true, newOffset: 200, append: true},
			wantView:         viewLog,
			wantLinesLen:     4,
			wantLines:        []string{"Line 1", "Line 2", "Line 3", "Line 4"},
			wantStreaming:    true,
			wantOffset:       200,
		},
		{
			name:             "append no new lines",
			initialView:      viewLog,
			initialStreaming: true,
			initialLogLines:  []logLine{{text: "Existing"}},
			initialOffset:    100,
			msg:              logOutputMsg{hasMore: true, newOffset: 100, append: true},
			wantView:         viewLog,
			wantLinesLen:     1,
			wantLines:        []string{"Existing"},
			wantStreaming:    true,
			wantOffset:       100,
		},
		{
			name:             "replace mode",
			initialView:      viewLog,
			initialStreaming: false,
			initialLogLines:  []logLine{{text: "Old line 1"}, {text: "Old line 2"}},
			msg:              logOutputMsg{lines: []logLine{{text: "New line 1"}}, hasMore: false, newOffset: 50, append: false},
			wantView:         viewLog,
			wantLinesLen:     1,
			wantLines:        []string{"New line 1"},
			wantStreaming:    false,
			wantOffset:       50,
		},
		{
			name:             "stale seq dropped",
			initialView:      viewLog,
			initialStreaming: true,
			initialFetchSeq:  5,
			initialLoading:   true,
			initialLogLines:  []logLine{{text: "Current"}},
			msg:              logOutputMsg{lines: []logLine{{text: "Stale data"}}, hasMore: true, newOffset: 999, append: false, seq: 3},
			wantView:         viewLog,
			wantLinesLen:     1,
			wantLines:        []string{"Current"},
			wantStreaming:    true,
			wantLoading:      true,
			wantOffset:       0,
		},
		{
			name:             "offset reset",
			initialView:      viewLog,
			initialStreaming: true,
			initialFetchSeq:  1,
			initialOffset:    500,
			initialLogLines:  []logLine{{text: "Old line 1"}, {text: "Old line 2"}},
			msg:              logOutputMsg{lines: []logLine{{text: "Reset line 1"}}, hasMore: true, newOffset: 100, append: false, seq: 1},
			wantView:         viewLog,
			wantLinesLen:     1,
			wantLines:        []string{"Reset line 1"},
			wantStreaming:    true,
			wantOffset:       100,
		},
		{
			name:             "replace mode empty clears stale",
			initialView:      viewLog,
			initialStreaming: true,
			initialFetchSeq:  1,
			initialLogLines:  []logLine{{text: "Stale line 1"}, {text: "Stale line 2"}},
			msg:              logOutputMsg{lines: nil, hasMore: false, newOffset: 0, append: false, seq: 1},
			wantView:         viewLog,
			wantLinesLen:     0,
			wantLinesEmpty:   true,
			wantStreaming:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newModel(localhostEndpoint, withExternalIODisabled())
			m.currentView = tt.initialView
			m.logJobID = 42
			m.logFromView = viewQueue
			m.logStreaming = tt.initialStreaming
			m.logFetchSeq = tt.initialFetchSeq
			m.logLoading = tt.initialLoading
			m.logOffset = tt.initialOffset
			m.logFmtr = tt.initialFmtr
			m.height = 30

			if tt.initialJobStatus != "" || tt.initialJobError != "" {
				m.jobs = []storage.ReviewJob{
					makeJob(42, withStatus(tt.initialJobStatus), withError(tt.initialJobError)),
				}
			}

			if tt.initialLogLines != nil {
				m.logLines = tt.initialLogLines
			}

			m2, _ := updateModel(t, m, tt.msg)

			assert.Equal(t, tt.wantView, m2.currentView)
			if tt.wantLinesNil {
				assert.Nil(t, m2.logLines)
			} else if tt.wantLinesEmpty {
				assert.False(t, m2.logLines == nil || len(m2.logLines) != 0)
			} else if m2.logLines == nil {
				assert.NotNil(t, m2.logLines, "Expected logLines to not be nil")
			}
			assert.Len(t, m2.logLines, tt.wantLinesLen)
			for i, line := range tt.wantLines {
				assert.False(t, i < len(m2.logLines) && m2.logLines[i].text != line)
			}
			assert.Equal(t, tt.wantStreaming, m2.logStreaming)
			assert.False(t, tt.wantFlashMsg != "" && !strings.Contains(m2.flashMessage, tt.wantFlashMsg))
			assert.Equal(t, tt.wantLoading, m2.logLoading)
			assert.Equal(t, tt.wantOffset, m2.logOffset)
			assert.False(t, tt.wantFmtr != nil && m2.logFmtr != tt.wantFmtr)
		})
	}
}

func TestMouseDisabledInContentViews(t *testing.T) {
	contentViews := []struct {
		name     string
		view     viewKind
		exitTo   viewKind
		enterKey rune
		exitKey  rune
		setup    func(m *model)
	}{
		{
			name:     "log view",
			view:     viewLog,
			enterKey: 'l',
			setup: func(m *model) {
				m.jobs = []storage.ReviewJob{
					makeJob(1, withStatus(storage.JobStatusRunning)),
				}
				m.selectedIdx, m.selectedJobID = 0, 1
			},
		},
		{
			name:     "review view via enter",
			view:     viewReview,
			enterKey: 0,
			setup: func(m *model) {
				m.jobs = []storage.ReviewJob{
					makeJob(1, withStatus(storage.JobStatusDone)),
				}
				m.selectedIdx, m.selectedJobID = 0, 1

				m.currentReview = &storage.Review{
					ID:     1,
					JobID:  1,
					Output: "test review",
					Job:    &m.jobs[0],
				}
			},
		},
		{
			name:   "patch view",
			view:   viewPatch,
			exitTo: viewTasks,
			setup: func(m *model) {
				m.patchText = "diff --git a/f.go b/f.go"
				m.patchJobID = 1
			},
		},
		{
			name: "commit message view",
			view: viewCommitMsg,
			setup: func(m *model) {
				m.commitMsgContent = "feat: test"
				m.commitMsgJobID = 1
				m.commitMsgFromView = viewQueue
			},
		},
	}

	for _, tc := range contentViews {

		if tc.enterKey != 0 {
			t.Run(tc.name+" enter disables mouse", func(t *testing.T) {
				m := newModel(localhostEndpoint, withExternalIODisabled())
				m.currentView = viewQueue
				m.height = 30
				m.width = 80
				if tc.setup != nil {
					tc.setup(&m)
				}

				var updated model
				var cmd tea.Cmd
				if tc.enterKey != 0 {
					updated, cmd = pressKey(m, tc.enterKey)
				} else {
					updated, cmd = pressSpecial(m, tea.KeyEnter)
				}

				if updated.currentView == viewQueue {
					require.Equal(t, viewQueue, updated.currentView, "view did not change from queue (setup may be incomplete)")
				}

				assert.NotNil(t, cmd)
				assert.Equal(t, tea.MouseModeNone, updated.View().MouseMode)
			})
		}

		t.Run(tc.name+" exit enables mouse", func(t *testing.T) {
			m := newModel(localhostEndpoint, withExternalIODisabled())
			m.currentView = tc.view
			m.height = 30
			m.width = 80
			if tc.setup != nil {
				tc.setup(&m)
			}

			if tc.view == viewLog {
				m.logJobID = 1
				m.logFromView = viewQueue
			}
			if tc.view == viewReview {
				m.reviewFromView = viewQueue
			}

			var updated model
			var cmd tea.Cmd
			if tc.exitKey != 0 {
				updated, cmd = pressKey(m, tc.exitKey)
			} else {
				updated, cmd = pressSpecial(m, tea.KeyEscape)
			}

			expectedView := tc.exitTo
			if expectedView == 0 {
				expectedView = viewQueue
			}
			assert.Equal(t, expectedView, updated.currentView)

			_ = cmd
			assert.Equal(t, tea.MouseModeCellMotion, updated.View().MouseMode)
		})
	}
}

func TestMouseNotToggledWithinContentViews(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.currentView = viewReview
	m.height = 30
	m.width = 80
	m.jobs = []storage.ReviewJob{
		makeJob(1, withStatus(storage.JobStatusDone)),
	}
	m.selectedIdx = 0
	m.currentReview = &storage.Review{
		ID:     1,
		JobID:  1,
		Output: "test",
		Prompt: "test prompt",
		Job:    &m.jobs[0],
	}
	m.reviewFromView = viewQueue

	updated, cmd := pressKey(m, 'p')
	if updated.currentView != viewKindPrompt {
		t.Skipf("did not switch to prompt view, got %d", updated.currentView)
	}

	msgs := collectMsgs(cmd)
	hasDisable := hasMsgType(msgs, "tea.disableMouseMsg")
	hasEnable := hasMsgType(msgs, "tea.enableMouseCellMotionMsg")
	assert.False(t, hasDisable || hasEnable)
}
