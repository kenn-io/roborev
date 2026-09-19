package tui

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/tui/splitlayout"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/streamfmt"
)

// If the split-pane tail drops job identity at startup, provider-shaped output
// can be decoded with the wrong protocol instead of following the selected
// agent and source.
func TestPaneLogPollAppendsNewBytesWhileRunning(t *testing.T) {
	const firstLine = "first streamed line"
	const secondLine = "second streamed line"
	firstBody := firstLine + "\n"
	secondBody := secondLine + "\n"

	_, m := mockServerModel(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("offset") {
		case "0":
			w.Header().Set("X-Job-Status", "running")
			w.Header().Set("X-Log-Offset", fmt.Sprintf("%d", len(firstBody)))
			_, _ = fmt.Fprint(w, firstBody)
		case fmt.Sprintf("%d", len(firstBody)):
			w.Header().Set("X-Job-Status", "running")
			w.Header().Set("X-Log-Offset", fmt.Sprintf("%d", len(firstBody)+len(secondBody)))
			_, _ = fmt.Fprint(w, secondBody)
		default:
			http.Error(w, "unexpected offset", http.StatusBadRequest)
		}
	})
	m.currentView = viewQueue
	m.layout = splitlayout.Split
	m.preferredLayout = splitlayout.Split
	m.width, m.height = 150, 40
	job := storage.ReviewJob{
		ID: 42, Status: storage.JobStatusRunning, Agent: "test",
	}
	m.jobs = []storage.ReviewJob{job}
	m.selectedIdx, m.selectedJobID = 0, job.ID

	started, cmd := m.startPaneLog(job)
	m = started.(model)
	require.NotNil(t, cmd)
	first, ok := cmd().(paneLogOutputMsg)
	require.True(t, ok)
	require.NoError(t, first.err)
	updated, _ := m.handlePaneLogOutputMsg(first)
	m = updated.(model)
	assert.Equal(t, []string{firstLine}, plainLogLines(m.paneLogLines))
	assert.True(t, m.paneLogStreaming)

	ticked, fetchCmd := m.handlePaneLogTickMsg(paneLogTickMsg{seq: m.paneLogSeq})
	m = ticked.(model)
	require.NotNil(t, fetchCmd)
	second, ok := fetchCmd().(paneLogOutputMsg)
	require.True(t, ok)
	require.NoError(t, second.err)
	updated, _ = m.handlePaneLogOutputMsg(second)
	m = updated.(model)
	assert.Equal(t, []string{firstLine, secondLine}, plainLogLines(m.paneLogLines))
	assert.True(t, m.paneLogStreaming)
}

func TestPaneLogPollGrowsUnterminatedLastRow(t *testing.T) {
	_, m := mockServerModel(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("offset") {
		case "0":
			w.Header().Set("X-Job-Status", "running")
			w.Header().Set("X-Log-Offset", "3")
			_, _ = fmt.Fprint(w, "Hel")
		case "3":
			w.Header().Set("X-Job-Status", "running")
			w.Header().Set("X-Log-Offset", "5")
			_, _ = fmt.Fprint(w, "lo")
		case "5":
			w.Header().Set("X-Job-Status", "running")
			w.Header().Set("X-Log-Offset", "12")
			_, _ = fmt.Fprint(w, " world\n")
		default:
			http.Error(w, "unexpected offset", http.StatusBadRequest)
		}
	})
	m.currentView = viewQueue
	m.layout = splitlayout.Split
	m.preferredLayout = splitlayout.Split
	m.width, m.height = 150, 40
	job := storage.ReviewJob{
		ID: 42, Status: storage.JobStatusRunning, Agent: "test",
	}
	m.jobs = []storage.ReviewJob{job}
	m.selectedIdx, m.selectedJobID = 0, job.ID

	started, cmd := m.startPaneLog(job)
	m = started.(model)
	first, ok := cmd().(paneLogOutputMsg)
	require.True(t, ok)
	require.NoError(t, first.err)
	updated, _ := m.handlePaneLogOutputMsg(first)
	m = updated.(model)
	assert.Equal(t, []string{"Hel"}, plainLogLines(m.paneLogLines))

	second, ok := m.fetchPaneLog(job.ID)().(paneLogOutputMsg)
	require.True(t, ok)
	require.NoError(t, second.err)
	updated, _ = m.handlePaneLogOutputMsg(second)
	m = updated.(model)
	assert.Equal(t, []string{"Hello"}, plainLogLines(m.paneLogLines))

	third, ok := m.fetchPaneLog(job.ID)().(paneLogOutputMsg)
	require.True(t, ok)
	require.NoError(t, third.err)
	updated, _ = m.handlePaneLogOutputMsg(third)
	m = updated.(model)
	assert.Equal(t, []string{"Hello world"}, plainLogLines(m.paneLogLines))
}

func TestPaneLogPollReplacesAllWrappedPendingRows(t *testing.T) {
	// Incremental replacement used to drop only the last rendered row. A
	// growing unterminated suffix that wraps then duplicated earlier wrap
	// fragments on every poll.
	bodies := map[string]string{}
	_, m := mockServerModel(t, func(w http.ResponseWriter, r *http.Request) {
		offset := r.URL.Query().Get("offset")
		body, ok := bodies[offset]
		if !ok {
			http.Error(w, "unexpected offset", http.StatusBadRequest)
			return
		}
		w.Header().Set("X-Job-Status", "running")
		w.Header().Set("X-Log-Offset", fmt.Sprintf("%d", len(body)))
		if offset != "0" {
			w.Header().Set("X-Log-Offset", fmt.Sprintf("%d", len(bodies["0"])+len(body)))
		}
		_, _ = fmt.Fprint(w, body)
	})
	m.currentView = viewQueue
	m.layout = splitlayout.Split
	m.preferredLayout = splitlayout.Split
	m.width, m.height = 80, 24
	wrapWidth := m.paneLogWidth()
	require.Greater(t, wrapWidth, 4)
	first := strings.Repeat("x", wrapWidth+3)
	second := strings.Repeat("x", wrapWidth)
	bodies["0"] = first
	bodies[fmt.Sprintf("%d", len(first))] = second
	full := first + second
	job := storage.ReviewJob{
		ID: 42, Status: storage.JobStatusRunning, Agent: "test",
	}
	m.jobs = []storage.ReviewJob{job}
	m.selectedIdx, m.selectedJobID = 0, job.ID

	started, cmd := m.startPaneLog(job)
	m = started.(model)
	firstMsg, ok := cmd().(paneLogOutputMsg)
	require.True(t, ok)
	require.NoError(t, firstMsg.err)
	updated, _ := m.handlePaneLogOutputMsg(firstMsg)
	m = updated.(model)
	firstRows := plainLogLines(m.paneLogLines)
	require.Greater(t, len(firstRows), 1, "unterminated suffix must wrap")
	assert.Equal(t, first, strings.Join(firstRows, ""))

	secondMsg, ok := m.fetchPaneLog(job.ID)().(paneLogOutputMsg)
	require.True(t, ok)
	require.NoError(t, secondMsg.err)
	updated, _ = m.handlePaneLogOutputMsg(secondMsg)
	m = updated.(model)
	rows := plainLogLines(m.paneLogLines)
	assert.Greater(t, len(rows), len(firstRows))
	assert.Equal(t, full, strings.Join(rows, ""))
}

func TestPaneLogPollTracksPendingRowsAfterCompleteLine(t *testing.T) {
	bodies := map[string]string{}
	_, m := mockServerModel(t, func(w http.ResponseWriter, r *http.Request) {
		offset := r.URL.Query().Get("offset")
		body, ok := bodies[offset]
		if !ok {
			http.Error(w, "unexpected offset", http.StatusBadRequest)
			return
		}
		off, err := strconv.Atoi(offset)
		if err != nil {
			http.Error(w, "bad offset", http.StatusBadRequest)
			return
		}
		w.Header().Set("X-Job-Status", "running")
		w.Header().Set("X-Log-Offset", strconv.Itoa(off+len(body)))
		_, _ = fmt.Fprint(w, body)
	})
	m.currentView = viewQueue
	m.layout = splitlayout.Split
	m.preferredLayout = splitlayout.Split
	m.width, m.height = 80, 24
	wrapWidth := m.paneLogWidth()
	require.Greater(t, wrapWidth, 4)
	complete := "done\n"
	firstPending := strings.Repeat("x", wrapWidth+3)
	secondPending := strings.Repeat("x", wrapWidth)
	bodies["0"] = complete
	bodies[strconv.Itoa(len(complete))] = firstPending
	bodies[strconv.Itoa(len(complete)+len(firstPending))] = secondPending
	job := storage.ReviewJob{
		ID: 42, Status: storage.JobStatusRunning, Agent: "test",
	}
	m.jobs = []storage.ReviewJob{job}
	m.selectedIdx, m.selectedJobID = 0, job.ID

	started, cmd := m.startPaneLog(job)
	m = started.(model)
	firstMsg, ok := cmd().(paneLogOutputMsg)
	require.True(t, ok)
	require.NoError(t, firstMsg.err)
	updated, _ := m.handlePaneLogOutputMsg(firstMsg)
	m = updated.(model)
	assert.Equal(t, []string{"done"}, plainLogLines(m.paneLogLines))

	secondMsg, ok := m.fetchPaneLog(job.ID)().(paneLogOutputMsg)
	require.True(t, ok)
	require.NoError(t, secondMsg.err)
	updated, _ = m.handlePaneLogOutputMsg(secondMsg)
	m = updated.(model)
	afterFirstPending := plainLogLines(m.paneLogLines)
	require.Greater(t, len(afterFirstPending), 2, "unterminated suffix must wrap")
	assert.Equal(t, "done"+firstPending, strings.Join(afterFirstPending, ""))

	thirdMsg, ok := m.fetchPaneLog(job.ID)().(paneLogOutputMsg)
	require.True(t, ok)
	require.NoError(t, thirdMsg.err)
	updated, _ = m.handlePaneLogOutputMsg(thirdMsg)
	m = updated.(model)
	rows := plainLogLines(m.paneLogLines)
	assert.Equal(t, "done"+firstPending+secondPending, strings.Join(rows, ""))
}

func TestPaneLogPollClearsPendingRowsWhenDecoderEmitsNothing(t *testing.T) {
	const incomplete = `{"type":"end"`
	bodies := map[string]string{
		"0":                                incomplete,
		fmt.Sprintf("%d", len(incomplete)): "}\n",
	}
	_, m := mockServerModel(t, func(w http.ResponseWriter, r *http.Request) {
		offset := r.URL.Query().Get("offset")
		body, ok := bodies[offset]
		if !ok {
			http.Error(w, "unexpected offset", http.StatusBadRequest)
			return
		}
		w.Header().Set("X-Job-Status", "running")
		w.Header().Set("X-Log-Offset", fmt.Sprintf("%d", len(incomplete)+len(body)))
		if offset == "0" {
			w.Header().Set("X-Log-Offset", fmt.Sprintf("%d", len(body)))
		}
		_, _ = fmt.Fprint(w, body)
	})
	m.currentView = viewQueue
	m.layout = splitlayout.Split
	m.preferredLayout = splitlayout.Split
	m.width, m.height = 80, 24
	job := storage.ReviewJob{
		ID: 42, Status: storage.JobStatusRunning, Agent: "grok",
	}
	m.jobs = []storage.ReviewJob{job}
	m.selectedIdx, m.selectedJobID = 0, job.ID

	started, cmd := m.startPaneLog(job)
	m = started.(model)
	first, ok := cmd().(paneLogOutputMsg)
	require.True(t, ok)
	require.NoError(t, first.err)
	updated, _ := m.handlePaneLogOutputMsg(first)
	m = updated.(model)
	require.Equal(t, []string{incomplete}, plainLogLines(m.paneLogLines))

	second, ok := m.fetchPaneLog(job.ID)().(paneLogOutputMsg)
	require.True(t, ok)
	require.NoError(t, second.err)
	updated, _ = m.handlePaneLogOutputMsg(second)
	m = updated.(model)
	assert.Empty(t, plainLogLines(m.paneLogLines))
	assert.Empty(t, m.paneLogPending)
	assert.Zero(t, m.paneLogPendingRows)
}

func TestPaneLogFetchUsesJobIdentity(t *testing.T) {
	const grokLine = `{"type":"text","data":"wrong provider"}`
	mixed := strings.Join([]string{
		`{"type":"item.completed","item":{"type":"agent_message","text":"classifier"}}`,
		`{"type":"text","data":"design"}`,
		`{"type":"end"}`,
	}, "\n") + "\n"
	tests := []struct {
		name   string
		agent  string
		source string
		body   string
		want   []string
	}{
		{
			name:  "known agent suppresses another provider",
			agent: "codex", body: grokLine + "\n",
		},
		{
			name: "unknown agent stays literal", agent: "future-agent",
			body: grokLine + "\n", want: []string{grokLine},
		},
		{
			name: "auto design keeps mixed providers", agent: "grok",
			source: storage.JobSourceAutoDesign, body: mixed,
			want: []string{"classifier", "design"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, m := mockServerModel(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("X-Job-Status", "done")
				_, _ = fmt.Fprint(w, tt.body)
			})
			m.currentView = viewQueue
			m.layout = splitlayout.Split
			m.preferredLayout = splitlayout.Split
			m.width, m.height = 150, 40
			job := storage.ReviewJob{
				ID: 42, Status: storage.JobStatusRunning,
				Agent: tt.agent, Source: tt.source,
			}
			m.jobs = []storage.ReviewJob{job}
			m.selectedIdx, m.selectedJobID = 0, job.ID

			started, cmd := m.startPaneLog(job)
			m = started.(model)
			require.NotNil(t, cmd)
			msg, ok := cmd().(paneLogOutputMsg)
			require.True(t, ok)
			require.NoError(t, msg.err)
			plain := plainLogLines(msg.lines)
			if len(tt.want) == 0 {
				assert.Empty(t, plain)
			} else {
				joined := strings.Join(plain, "\n")
				for _, want := range tt.want {
					assert.Contains(t, joined, want)
				}
			}
		})
	}
}

func TestPaneLogFetchRefreshesIdentityAfterFailover(t *testing.T) {
	const response = "replacement split provider output"
	_, m := mockServerModel(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "codex", r.Header.Get("X-Job-Agent"))
		w.Header().Set("X-Job-Agent", "grok")
		w.Header().Set("X-Job-Status", "done")
		w.Header().Set("X-Log-Offset", "100")
		_, _ = fmt.Fprintln(w, `{"type":"text","data":"replacement split provider output"}`)
		_, _ = fmt.Fprintln(w, `{"type":"end"}`)
	})
	m.currentView = viewQueue
	m.layout = splitlayout.Split
	m.width, m.height = 150, 40
	job := storage.ReviewJob{
		ID: 42, Status: storage.JobStatusRunning, Agent: "codex",
	}
	m.jobs = []storage.ReviewJob{job}
	m.selectedIdx, m.selectedJobID = 0, job.ID
	started, _ := m.startPaneLog(job)
	m = started.(model)
	m.paneLogOffset = 50

	msg, ok := m.fetchPaneLog(job.ID)().(paneLogOutputMsg)
	require.True(t, ok)
	require.NoError(t, msg.err)
	updated, _ := m.handlePaneLogOutputMsg(msg)
	m = updated.(model)

	assert.Equal(t, "grok", m.paneLogAgent)
	assert.Equal(t, []string{response}, plainLogLines(m.paneLogLines))
}

func TestPaneLogFetchReplacesAutoDesignRowsOnServerReset(t *testing.T) {
	const response = "replacement split auto-design output"
	_, m := mockServerModel(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "50", r.URL.Query().Get("offset"))
		assert.Equal(t, "codex", r.Header.Get("X-Job-Agent"))
		w.Header().Set("X-Job-Agent", "grok")
		w.Header().Set("X-Job-Source", storage.JobSourceAutoDesign)
		w.Header().Set("X-Job-Status", "done")
		w.Header().Set("X-Log-Offset", "100")
		w.Header().Set("X-Log-Reset", "true")
		_, _ = fmt.Fprintln(w, `{"type":"text","data":"replacement split auto-design output"}`)
		_, _ = fmt.Fprintln(w, `{"type":"end"}`)
	})
	m.currentView = viewQueue
	m.layout = splitlayout.Split
	m.width, m.height = 150, 40
	job := storage.ReviewJob{
		ID: 42, Status: storage.JobStatusRunning,
		Agent: "codex", Source: storage.JobSourceAutoDesign,
	}
	m.jobs = []storage.ReviewJob{job}
	m.selectedIdx, m.selectedJobID = 0, job.ID
	started, _ := m.startPaneLog(job)
	m = started.(model)
	m.paneLogOffset = 50
	m.paneLogLines = []logLine{{text: "stale split provider output"}}

	msg, ok := m.fetchPaneLog(job.ID)().(paneLogOutputMsg)
	require.True(t, ok)
	require.NoError(t, msg.err)
	require.False(t, msg.append)
	updated, _ := m.handlePaneLogOutputMsg(msg)
	m = updated.(model)

	assert.Equal(t, "grok", m.paneLogAgent)
	assert.Equal(t, []string{response}, plainLogLines(m.paneLogLines))
}

// If a split-pane poll finalizes the Grok decoder while more bytes are
// expected, adjacent response chunks become separate Markdown rows and wrap
// independently.
func TestPaneLogFetchKeepsGrokTextTogetherAcrossPolls(t *testing.T) {
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
				`{"type":"end"}`,
			}, "\n")+"\n")
		default:
			http.Error(w, "unexpected offset", http.StatusBadRequest)
		}
	})
	m.currentView = viewQueue
	m.layout = splitlayout.Split
	m.preferredLayout = splitlayout.Split
	m.width, m.height = 150, 40
	job := storage.ReviewJob{
		ID: 42, Status: storage.JobStatusRunning, Agent: "grok",
	}
	m.jobs = []storage.ReviewJob{job}
	m.selectedIdx, m.selectedJobID = 0, job.ID

	started, cmd := m.startPaneLog(job)
	m = started.(model)
	first, ok := cmd().(paneLogOutputMsg)
	require.True(t, ok)
	require.NoError(t, first.err)
	assert.Empty(t, first.lines)
	updated, _ := m.handlePaneLogOutputMsg(first)
	m = updated.(model)

	second, ok := m.fetchPaneLog(job.ID)().(paneLogOutputMsg)
	require.True(t, ok)
	require.NoError(t, second.err)
	require.NotNil(t, second.fmtr)
	assert.Equal(t, m.paneLogWidth(), second.fmtr.Width())
	updated, _ = m.handlePaneLogOutputMsg(second)
	m = updated.(model)
	assert.Equal(t, []string{response}, plainLogLines(m.paneLogLines))
}

func TestPaneLogEmptyTerminalPollFlushesBufferedGrokText(t *testing.T) {
	const response = "buffered until the split terminal poll"
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
	m.currentView = viewQueue
	m.layout = splitlayout.Split
	m.preferredLayout = splitlayout.Split
	m.width, m.height = 150, 40
	job := storage.ReviewJob{
		ID: 42, Status: storage.JobStatusRunning, Agent: "grok",
	}
	m.jobs = []storage.ReviewJob{job}
	m.selectedIdx, m.selectedJobID = 0, job.ID

	started, firstCmd := m.startPaneLog(job)
	m = started.(model)
	first, ok := firstCmd().(paneLogOutputMsg)
	require.True(t, ok)
	require.NoError(t, first.err)
	require.Empty(t, first.lines)
	updated, _ := m.handlePaneLogOutputMsg(first)
	m = updated.(model)

	final, ok := m.fetchPaneLog(job.ID)().(paneLogOutputMsg)
	require.True(t, ok)
	require.NoError(t, final.err)
	updated, handoff := m.handlePaneLogOutputMsg(final)
	m = updated.(model)

	assert.Equal(t, []string{response}, plainLogLines(m.paneLogLines))
	assert.False(t, m.paneLogStreaming)
	assert.NotNil(t, handoff)
}

func TestPaneLogJobsCompletionDoesNotDelayReviewHandoff(t *testing.T) {
	// Once the jobs feed marks the selected row terminal, the detail pane no
	// longer renders its live-log tail. Review handoff must not wait for a
	// scheduled poll that can only update that hidden buffer.
	for _, terminal := range []storage.JobStatus{
		storage.JobStatusDone,
		storage.JobStatusFailed,
	} {
		t.Run(string(terminal), func(t *testing.T) {
			m := splitModel(withSelection(2, 0))
			job := storage.ReviewJob{
				ID: 1, Status: terminal, Agent: "grok", Error: "agent failed",
			}
			m.jobs = []storage.ReviewJob{job}
			m.selectedIdx, m.selectedJobID = 0, job.ID
			m.paneLogJobID = job.ID
			m.paneLogStreaming = true
			m.paneLogOffset = 50
			m.paneLogFmtr = streamfmt.NewWithWidth(
				io.Discard, 80, m.glamourStyle, streamfmt.DecoderForAgent("grok"),
			)

			m, handoff := m.splitReconcileDetail()
			assert.False(t, m.paneLogStreaming)
			if terminal == storage.JobStatusDone {
				assert.NotNil(t, handoff, "done must start the review fetch immediately")
			} else {
				assert.NotNil(t, handoff, "failed jobs may load persisted comments after local synthesis")
				require.NotNil(t, m.currentReview)
				assert.Equal(t, job.ID, m.currentReview.JobID)
				assert.Contains(t, m.currentReview.Output, job.Error)
			}
		})
	}
}

// If a split-pane resize rebuild drops source identity, an auto-design tail
// loses the classifier half when it is re-rendered at the new width.
func TestPaneLogResizeKeepsJobIdentity(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"item.completed","item":{"type":"agent_message","text":"classifier after resize"}}`,
		`{"type":"text","data":"design after resize"}`,
		`{"type":"end"}`,
	}, "\n") + "\n"
	_, m := mockServerModel(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Job-Status", "running")
		w.Header().Set("X-Log-Offset", "100")
		_, _ = fmt.Fprint(w, input)
	})
	m.currentView = viewQueue
	m.layout = splitlayout.Split
	m.preferredLayout = splitlayout.Split
	m.width, m.height = 150, 40
	job := storage.ReviewJob{
		ID: 42, Status: storage.JobStatusRunning,
		Agent: "grok", Source: storage.JobSourceAutoDesign,
	}
	m.jobs = []storage.ReviewJob{job}
	m.selectedIdx, m.selectedJobID = 0, job.ID
	started, _ := m.startPaneLog(job)
	m = started.(model)
	m.paneLogLines = []logLine{{text: "stale width"}}
	m.paneLogOffset = 40

	res, cmd := m.handleWindowSizeMsg(tea.WindowSizeMsg{Width: 180, Height: 45})
	m = res.(model)
	require.NotNil(t, cmd)
	var fetched *paneLogOutputMsg
	for _, raw := range collectMsgs(cmd) {
		if msg, ok := raw.(paneLogOutputMsg); ok {
			msgCopy := msg
			fetched = &msgCopy
		}
	}
	require.NotNil(t, fetched)
	require.NoError(t, fetched.err)
	require.NotNil(t, fetched.fmtr)
	assert.Equal(t, m.paneLogWidth(), fetched.fmtr.Width())
	plain := strings.Join(plainLogLines(fetched.lines), "\n")
	assert.Contains(t, plain, "classifier after resize")
	assert.Contains(t, plain, "design after resize")
}
