package tui

import (
	"bytes"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
)

type mockClipboard struct {
	lastText string
	err      error
}

func (m *mockClipboard) WriteText(text string) error {
	if m.err != nil {
		return m.err
	}
	m.lastText = text
	return nil
}

func TestTUIYankCopyFromReviewView(t *testing.T) {
	mock := &mockClipboard{}

	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.clipboard = mock
	m.currentView = viewReview
	m.currentReview = makeReview(1, &storage.ReviewJob{ID: 1}, withReviewAgent("test"), withReviewOutput("This is the review content to copy"))

	m, cmd := pressKey(m, 'y')

	assert.NotNil(t, cmd, "Expected a command to be returned")

	msg := cmd()
	result, ok := msg.(clipboardResultMsg)
	assert.True(t, ok)

	require.NoError(t, result.err)

	expectedContent := "Review #1\n\nThis is the review content to copy"
	assert.Equal(t, expectedContent, mock.lastText)
}

func TestTUIYankCopyShowsFlashMessage(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.currentView = viewReview
	m.currentReview = makeReview(1, &storage.ReviewJob{ID: 1}, withReviewAgent("test"), withReviewOutput("Review content"))
	m.width = 80
	m.height = 24

	m, _ = updateModel(t, m, clipboardResultMsg{err: nil, view: viewReview})

	assert.Equal(t, "Copied to clipboard", m.flashMessage)

	assert.False(t, m.flashExpiresAt.IsZero())

	assert.Equal(t, viewReview, m.flashView)

	output := m.renderReviewView()
	assert.Contains(t, output, "Copied to clipboard")
}

func TestTUIYankCopyShowsErrorOnFailure(t *testing.T) {
	assert := assert.New(t)
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.currentView = viewQueue
	m.width = 80
	m.height = 24
	m.jobs = []storage.ReviewJob{
		makeJob(1, withRef("abc123"), withAgent("test"), withStatus(storage.JobStatusDone)),
	}

	m, _ = updateModel(t, m, clipboardResultMsg{err: fmt.Errorf("clipboard not available"), view: viewQueue})

	require.Error(t, m.err)
	assert.Contains(m.err.Error(), "copy failed")

	assert.True(m.flashWarning, "expected warning-styled flash")
	assert.Equal("Copy failed: clipboard not available", m.flashMessage)
	assert.Equal(viewQueue, m.flashView)
	assert.False(m.flashExpiresAt.IsZero())

	output := m.renderQueueView()
	assert.Contains(output, "Copy failed: clipboard not available")
}

func TestTUIYankCopyShowsFriendlyMessageWhenNoClipboardTool(t *testing.T) {
	assert := assert.New(t)
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.currentView = viewQueue
	m.width = 80
	m.height = 24
	m.jobs = []storage.ReviewJob{
		makeJob(1, withRef("abc123"), withAgent("test"), withStatus(storage.JobStatusDone)),
	}

	atottoErr := fmt.Errorf("No clipboard utilities available. Please install xsel, xclip, wl-clipboard or Termux:API add-on for termux-clipboard-get/set.")
	m, _ = updateModel(t, m, clipboardResultMsg{err: atottoErr, view: viewQueue})

	assert.True(m.flashWarning)
	assert.Equal("Copy failed: install xclip, wl-clipboard, or xsel", m.flashMessage)

	output := m.renderQueueView()
	assert.Contains(output, "Copy failed: install xclip, wl-clipboard, or xsel")
}

func TestTUIYankFlashViewNotAffectedByViewChange(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.currentView = viewQueue
	m.width = 80
	m.height = 24
	m.currentReview = makeReview(1, &storage.ReviewJob{ID: 1}, withReviewAgent("test"), withReviewOutput("Review content"))

	m.currentView = viewReview

	m, _ = updateModel(t, m, clipboardResultMsg{err: nil, view: viewQueue})

	assert.Equal(t, viewQueue, m.flashView)

	output := m.renderReviewView()
	assert.NotContains(t, output, "Copied to clipboard")
}

func TestTUIYankFromQueueRequiresCompletedJob(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.currentView = viewQueue
	m.jobs = []storage.ReviewJob{
		makeJob(1, withRef("abc123"), withAgent("test"), withStatus(storage.JobStatusRunning)),
		makeJob(2, withRef("def456"), withAgent("test")),
	}
	m.selectedIdx, m.selectedJobID = 0, 1

	_, cmd := pressKey(m, 'y')
	assert.Nil(t, cmd)

	m.selectedIdx, m.selectedJobID = 1, 2
	_, cmd = pressKey(m, 'y')
	assert.NotNil(t, cmd)
}

func TestTUIFetchReviewAndCopySuccess(t *testing.T) {
	mock := &mockClipboard{}

	_, m := mockServerModel(t, mockReviewHandler(
		storage.Review{ID: 1, JobID: 123, Agent: "test", Output: "Review content for clipboard"},
		nil,
	))
	m.clipboard = mock

	cmd := m.fetchReviewAndCopy(123, nil)
	msg := cmd()

	result, ok := msg.(clipboardResultMsg)
	assert.True(t, ok)

	require.NoError(t, result.err)

	expectedContent := "Review #123\n\nReview content for clipboard"
	assert.Equal(t, expectedContent, mock.lastText)
}

func TestTUIFetchReviewAndCopyIncludesComments(t *testing.T) {
	mock := &mockClipboard{}

	responses := []storage.Response{
		{
			ID:        1,
			Responder: "dev",
			Response:  "This is expected behavior",
			CreatedAt: time.Date(2025, 3, 10, 9, 0, 0, 0, time.UTC),
		},
	}
	_, m := mockServerModel(t, mockReviewHandler(
		storage.Review{ID: 1, JobID: 123, Agent: "test", Output: "Found an issue"},
		responses,
	))
	m.clipboard = mock

	cmd := m.fetchReviewAndCopy(123, nil)
	msg := cmd()

	result, ok := msg.(clipboardResultMsg)
	assert.True(t, ok)
	require.NoError(t, result.err)

	assert.Contains(t, mock.lastText, "Found an issue")
	assert.Contains(t, mock.lastText, "--- Comments ---")
	assert.Contains(t, mock.lastText, "[Mar 10 09:00] dev:")
	assert.Contains(t, mock.lastText, "This is expected behavior")
}

func TestTUIFetchReviewAndCopy404(t *testing.T) {
	_, m := mockServerModel(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	cmd := m.fetchReviewAndCopy(123, nil)
	msg := cmd()

	result, ok := msg.(clipboardResultMsg)
	assert.True(t, ok)

	require.Error(t, result.err)

	assert.Contains(t, result.err.Error(), "no review found")
}

func TestTUIFetchReviewAndCopyEmptyOutput(t *testing.T) {
	_, m := mockServerModel(t, mockReviewHandler(
		storage.Review{ID: 1, JobID: 123, Agent: "test", Output: ""},
		nil,
	))

	cmd := m.fetchReviewAndCopy(123, nil)
	msg := cmd()

	result, ok := msg.(clipboardResultMsg)
	assert.True(t, ok)

	require.Error(t, result.err)

	assert.Contains(t, result.err.Error(), "review has no content")
}

func TestTUIClipboardWriteFailurePropagates(t *testing.T) {
	mock := &mockClipboard{err: fmt.Errorf("clipboard unavailable: xclip not found")}

	m := initTestModel(
		withCurrentView(viewReview),
		withClipboard(mock),
		withReview(makeReview(1, &storage.ReviewJob{ID: 1}, withReviewAgent("test"), withReviewOutput("Review content"))),
	)

	_, cmd := pressKey(m, 'y')
	assert.NotNil(t, cmd, "Expected command to be returned")

	msg := cmd()
	result, ok := msg.(clipboardResultMsg)
	assert.True(t, ok)

	require.Error(t, result.err)

	assert.Contains(t, result.err.Error(), "clipboard unavailable")
}

func TestTUIFetchReviewAndCopyClipboardFailure(t *testing.T) {
	mock := &mockClipboard{err: fmt.Errorf("clipboard unavailable: pbcopy not found")}

	_, m := mockServerModel(t, mockReviewHandler(
		storage.Review{ID: 1, JobID: 123, Agent: "test", Output: "Review content"},
		nil,
	))
	m.clipboard = mock

	cmd := m.fetchReviewAndCopy(123, nil)
	msg := cmd()

	result, ok := msg.(clipboardResultMsg)
	assert.True(t, ok)

	require.Error(t, result.err)

	assert.Contains(t, result.err.Error(), "clipboard unavailable")
}

func TestTUIFetchReviewAndCopyJobInjection(t *testing.T) {
	mock := &mockClipboard{}

	_, m := mockServerModel(t, mockReviewHandler(
		storage.Review{ID: 42, JobID: 123, Agent: "test", Output: "Review content"},
		nil,
	))
	m.clipboard = mock

	j := makeJob(123, withRepoPath("/path/to/repo"), withRef("a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"))

	cmd := m.fetchReviewAndCopy(123, &j)
	msg := cmd()

	result, ok := msg.(clipboardResultMsg)
	assert.True(t, ok)

	require.NoError(t, result.err)

	expectedContent := "Review #123 /path/to/repo a1b2c3d\n\nReview content"
	assert.Equal(t, expectedContent, mock.lastText)
}

func TestFormatClipboardContent(t *testing.T) {
	tests := []struct {
		name     string
		review   *storage.Review
		expected string
	}{
		{
			name:     "nil review",
			review:   nil,
			expected: "",
		},
		{
			name: "empty output",
			review: &storage.Review{
				ID:     1,
				Output: "",
			},
			expected: "",
		},
		{
			name: "review with JobID only (no job struct)",
			review: &storage.Review{
				ID:     99,
				JobID:  42,
				Output: "Content here",
			},
			expected: "Review #42\n\nContent here",
		},
		{
			name: "review with JobID 0 but review ID set (legacy fallback)",
			review: &storage.Review{
				ID:     77,
				JobID:  0,
				Output: "Content here",
			},
			expected: "Review #77\n\nContent here",
		},
		{
			name: "review with all IDs 0 and no job struct (no header)",
			review: &storage.Review{
				ID:     0,
				JobID:  0,
				Output: "Content here",
			},
			expected: "Content here",
		},
		{
			name: "review with job - full SHA truncated",
			review: &storage.Review{
				ID:     99,
				Output: "Review content",
				Job: &storage.ReviewJob{
					ID:       99,
					RepoPath: "/Users/test/myrepo",
					GitRef:   "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2",
				},
			},
			expected: "Review #99 /Users/test/myrepo a1b2c3d\n\nReview content",
		},
		{
			name: "long branch name not truncated",
			review: &storage.Review{
				ID:     101,
				Output: "Review content",
				Job: &storage.ReviewJob{
					ID:       101,
					RepoPath: "/repo",
					GitRef:   "feature/very-long-branch-name-that-exceeds-forty-characters",
				},
			},
			expected: "Review #101 /repo feature/very-long-branch-name-that-exceeds-forty-characters\n\nReview content",
		},
		{
			name: "review with job - range not truncated",
			review: &storage.Review{
				ID:     100,
				Output: "Review content",
				Job: &storage.ReviewJob{
					ID:       100,
					RepoPath: "/path/to/repo",
					GitRef:   "abc1234..def5678",
				},
			},
			expected: "Review #100 /path/to/repo abc1234..def5678\n\nReview content",
		},
		{
			name: "always uses job ID from Job struct",
			review: &storage.Review{
				ID:     999,
				Output: "Review content",
				Job: &storage.ReviewJob{
					ID:       555,
					RepoPath: "/repo/path",
					GitRef:   "abcdef1234567890abcdef1234567890abcdef12",
				},
			},
			expected: "Review #555 /repo/path abcdef1\n\nReview content",
		},
		{
			name: "Job present but Job.ID is 0 falls back to JobID with context",
			review: &storage.Review{
				ID:     999,
				JobID:  123,
				Output: "Review content",
				Job: &storage.ReviewJob{
					ID:       0,
					RepoPath: "/repo/path",
					GitRef:   "abc1234",
				},
			},
			expected: "Review #123 /repo/path abc1234\n\nReview content",
		},
		{
			name: "Job present but Job.ID is 0 falls back to review.ID with context",
			review: &storage.Review{
				ID:     999,
				JobID:  0,
				Output: "Review content",
				Job: &storage.ReviewJob{
					ID:       0,
					RepoPath: "/repo/path",
					GitRef:   "abc1234",
				},
			},
			expected: "Review #999 /repo/path abc1234\n\nReview content",
		},
		{
			name: "short git ref not truncated",
			review: &storage.Review{
				ID:     10,
				Output: "Content",
				Job: &storage.ReviewJob{
					ID:       10,
					RepoPath: "/repo",
					GitRef:   "abc1234",
				},
			},
			expected: "Review #10 /repo abc1234\n\nContent",
		},
		{
			name: "uppercase SHA truncated",
			review: &storage.Review{
				ID:     102,
				Output: "Content",
				Job: &storage.ReviewJob{
					ID:       102,
					RepoPath: "/repo",
					GitRef:   "ABCDEF1234567890ABCDEF1234567890ABCDEF12",
				},
			},
			expected: "Review #102 /repo ABCDEF1\n\nContent",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatClipboardContent(tt.review, nil)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestFormatClipboardContentWithResponses(t *testing.T) {
	review := &storage.Review{
		ID:     1,
		JobID:  42,
		Output: "Some findings here",
	}
	responses := []storage.Response{
		{
			ID:        1,
			Responder: "alice",
			Response:  "Known issue, ignoring",
			CreatedAt: time.Date(2025, 3, 15, 14, 30, 0, 0, time.UTC),
		},
		{
			ID:        2,
			Responder: "bob",
			Response:  "Fixed in next commit",
			CreatedAt: time.Date(2025, 3, 15, 15, 0, 0, 0, time.UTC),
		},
	}

	got := formatClipboardContent(review, responses)

	assert.Contains(t, got, "Some findings here")

	assert.Contains(t, got, "--- Comments ---")

	assert.Contains(t, got, "[Mar 15 14:30] alice:")
	assert.Contains(t, got, "Known issue, ignoring")
	assert.Contains(t, got, "[Mar 15 15:00] bob:")
	assert.Contains(t, got, "Fixed in next commit")
}

func TestFormatClipboardContentNoResponses(t *testing.T) {
	review := &storage.Review{
		ID:     1,
		JobID:  42,
		Output: "Review content",
	}

	withNil := formatClipboardContent(review, nil)
	withEmpty := formatClipboardContent(review, []storage.Response{})
	assert.Equal(t, withEmpty, withNil, "nil and empty responses should produce same output")

	assert.NotContains(t, withNil, "Comments")
}

func TestOSC52ClipboardWritesEscapeSequence(t *testing.T) {
	var buf bytes.Buffer
	cb := &osc52Clipboard{output: &buf}

	err := cb.WriteText("hello clipboard")
	require.NoError(t, err)

	output := buf.String()
	// OSC52 sequences begin with ESC ] 52 ; and end with BEL or ST
	assert.Contains(t, output, "\x1b]52;")
	// The base64-encoded content of "hello clipboard" is aGVsbG8gY2xpcGJvYXJk
	assert.Contains(t, output, "aGVsbG8gY2xpcGJvYXJk")
}

func TestOSC52ClipboardEmptyText(t *testing.T) {
	var buf bytes.Buffer
	cb := &osc52Clipboard{output: &buf}

	err := cb.WriteText("")
	require.NoError(t, err)
	// An empty string still produces a valid OSC52 sequence (clears clipboard)
	assert.Contains(t, buf.String(), "\x1b]52;")
}

func TestNewClipboardReturnsOSC52OverSSH(t *testing.T) {
	tests := []struct {
		name    string
		envVars map[string]string
		wantOSC bool
	}{
		{
			name:    "SSH_TTY set",
			envVars: map[string]string{"SSH_TTY": "/dev/pts/0"},
			wantOSC: true,
		},
		{
			name:    "SSH_CLIENT set",
			envVars: map[string]string{"SSH_CLIENT": "192.168.1.1 50000 22"},
			wantOSC: true,
		},
		{
			name:    "SSH_CONNECTION set",
			envVars: map[string]string{"SSH_CONNECTION": "192.168.1.1 50000 10.0.0.1 22"},
			wantOSC: true,
		},
		{
			name:    "no SSH env vars",
			envVars: map[string]string{},
			wantOSC: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Clear SSH env vars before each sub-test
			for _, key := range []string{"SSH_TTY", "SSH_CLIENT", "SSH_CONNECTION"} {
				t.Setenv(key, "")
			}
			for k, v := range tt.envVars {
				t.Setenv(k, v)
			}

			cb := newClipboard()
			_, isOSC52 := cb.(*osc52Clipboard)
			assert.Equal(t, tt.wantOSC, isOSC52,
				"expected osc52Clipboard=%v for env %v", tt.wantOSC, tt.envVars)
		})
	}
}
