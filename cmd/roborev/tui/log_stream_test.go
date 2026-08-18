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

// If the replacement grows beyond the old offset, only the server's reset
// signal distinguishes a full replacement from an incremental auto-design
// chunk. Ignoring it leaves stale rows ahead of the new provider output.
func TestTUILogFetchReplacesAutoDesignRowsOnServerReset(t *testing.T) {
	const response = "replacement auto-design output"
	_, m := mockServerModel(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "50", r.URL.Query().Get("offset"))
		assert.Equal(t, "codex", r.Header.Get("X-Job-Agent"))
		w.Header().Set("X-Job-Agent", "grok")
		w.Header().Set("X-Job-Source", storage.JobSourceAutoDesign)
		w.Header().Set("X-Job-Status", "done")
		w.Header().Set("X-Log-Offset", "100")
		w.Header().Set("X-Log-Reset", "true")
		_, _ = fmt.Fprintln(w, `{"type":"text","data":"replacement auto-design output"}`)
		_, _ = fmt.Fprintln(w, `{"type":"end"}`)
	})
	m.width, m.height = 80, 24
	job := storage.ReviewJob{
		ID: 42, Status: storage.JobStatusRunning,
		Agent: "codex", Source: storage.JobSourceAutoDesign,
	}
	opened, _ := m.openLogView(job, viewQueue)
	m = opened.(model)
	m.logOffset = 50
	m.logLines = []logLine{{text: "stale provider output"}}

	msg, ok := m.fetchJobLog(job.ID)().(logOutputMsg)
	require.True(t, ok)
	require.NoError(t, msg.err)
	require.False(t, msg.append)
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
