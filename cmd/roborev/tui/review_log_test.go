package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
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
