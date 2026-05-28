package daemon

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

// jobOutputResponse covers the union of fields returned by GET /api/job/output
// in both polling and streaming modes.
type jobOutputResponse struct {
	JobID   int64  `json:"job_id"`
	Status  string `json:"status"`
	Type    string `json:"type"`
	HasMore bool   `json:"has_more"`
	Lines   []struct {
		TS       string `json:"ts"`
		Text     string `json:"text"`
		LineType string `json:"line_type"`
	} `json:"lines"`
}

func TestHandleJobOutput(t *testing.T) {
	server, db, tmpDir := newTestServer(t)

	t.Run("missing job_id", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/job/output", nil)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)

		require.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
	})

	t.Run("invalid job_id", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/job/output?job_id=notanumber", nil)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)

		require.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
	})

	t.Run("nonexistent job", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/job/output?job_id=99999", nil)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)

		require.Equal(t, http.StatusNotFound, w.Code, "body: %s", w.Body.String())
	})

	t.Run("polling running job", func(t *testing.T) {
		job := createTestJob(t, db, filepath.Join(tmpDir, "test-repo-running"), "abc123", "test-agent")
		setJobStatus(t, db, job.ID, storage.JobStatusRunning)

		req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/job/output?job_id=%d", job.ID), nil)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

		var resp jobOutputResponse
		testutil.DecodeJSON(t, w, &resp)

		assert.Equal(t, job.ID, resp.JobID)
		assert.Equal(t, "running", resp.Status)
		assert.True(t, resp.HasMore, "expected has_more=true for running job")
	})

	t.Run("polling completed job", func(t *testing.T) {
		job := createTestJob(t, db, filepath.Join(tmpDir, "test-repo-done"), "abc123", "test-agent")
		setJobStatus(t, db, job.ID, storage.JobStatusDone)

		req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/job/output?job_id=%d", job.ID), nil)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

		var resp jobOutputResponse
		testutil.DecodeJSON(t, w, &resp)

		assert.Equal(t, "done", resp.Status)
		assert.False(t, resp.HasMore, "expected has_more=false for completed job")
	})

	t.Run("stream completed job returns NDJSON complete", func(t *testing.T) {
		job := createTestJob(t, db, filepath.Join(tmpDir, "test-repo-stream"), "abc123", "test-agent")
		setJobStatus(t, db, job.ID, storage.JobStatusDone)

		req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/job/output?job_id=%d&stream=1", job.ID), nil)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)

		require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
		assert.Equal(t, "application/x-ndjson", w.Header().Get("Content-Type"))

		var resp jobOutputResponse
		testutil.DecodeJSON(t, w, &resp)

		assert.Equal(t, "complete", resp.Type)
		assert.Equal(t, "done", resp.Status)
	})
}

func TestHandleJobOutputIDParsing(t *testing.T) {
	server, _, _ := newTestServer(t)
	for _, id := range []string{"abc", "10abc", "1.5"} {
		t.Run("invalid_id_"+id, func(t *testing.T) {
			rr := serveHuma(t, server, http.MethodGet,
				"/api/job/output?job_id="+id, nil)
			assert.GreaterOrEqual(t, rr.Code, 400,
				"expected client error for invalid id %q", id)
		})
	}
}

func TestHandleJobLog(t *testing.T) {
	server, db, tmpDir := newTestServer(t)
	t.Setenv("ROBOREV_DATA_DIR", tmpDir)

	// Create a repo and a job
	repo, err := db.GetOrCreateRepo(filepath.Join(tmpDir, "testrepo"))
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "GetOrCreateRepo: %v", err)
	}
	job, err := db.EnqueueJob(storage.EnqueueOpts{
		RepoID: repo.ID,
		GitRef: "abc123",
		Agent:  "test",
	})
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "EnqueueJob: %v", err)
	}

	t.Run("missing job_id returns 400", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/job/log", nil)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			assert.Condition(t, func() bool {
				return false
			}, "expected 400, got %d", w.Code)
		}
	})

	t.Run("nonexistent job returns 404", func(t *testing.T) {
		req := httptest.NewRequest(
			http.MethodGet, "/api/job/log?job_id=99999", nil,
		)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			assert.Condition(t, func() bool {
				return false
			}, "expected 404, got %d", w.Code)
		}
	})

	t.Run("no log file returns 404", func(t *testing.T) {
		req := httptest.NewRequest(
			http.MethodGet,
			fmt.Sprintf("/api/job/log?job_id=%d", job.ID),
			nil,
		)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			assert.Condition(t, func() bool {
				return false
			}, "expected 404, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("returns log content with headers", func(t *testing.T) {
		// Create a log file
		logDir := JobLogDir()
		if err := os.MkdirAll(logDir, 0o755); err != nil {
			require.Condition(t, func() bool {
				return false
			}, "MkdirAll: %v", err)
		}
		logContent := `{"type":"assistant","message":{"content":[{"type":"text","text":"hello"}]}}` + "\n"
		if err := os.WriteFile(
			JobLogPath(job.ID), []byte(logContent), 0o644,
		); err != nil {
			require.Condition(t, func() bool {
				return false
			}, "WriteFile: %v", err)
		}

		req := httptest.NewRequest(
			http.MethodGet,
			fmt.Sprintf("/api/job/log?job_id=%d", job.ID),
			nil,
		)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			require.Condition(t, func() bool {
				return false
			}, "expected 200, got %d: %s", w.Code, w.Body.String())
		}
		if ct := w.Header().Get("Content-Type"); ct != "application/x-ndjson" {
			assert.Condition(t, func() bool {
				return false
			}, "expected Content-Type application/x-ndjson, got %q", ct)
		}
		if js := w.Header().Get("X-Job-Status"); js != "queued" {
			assert.Condition(t, func() bool {
				return false
			}, "expected X-Job-Status queued, got %q", js)
		}
		if w.Body.String() != logContent {
			assert.Condition(t, func() bool {
				return false
			}, "expected log content %q, got %q", logContent, w.Body.String())
		}
	})

	t.Run("running job with no log returns empty 200", func(t *testing.T) {
		// Claim the existing queued job to move it to "running"
		claimed, err := db.ClaimJob("worker-test")
		if err != nil {
			require.Condition(t, func() bool {
				return false

				// Remove any log file to simulate startup race
			}, "ClaimJob: %v", err)
		}

		os.Remove(JobLogPath(claimed.ID))

		req := httptest.NewRequest(
			http.MethodGet,
			fmt.Sprintf("/api/job/log?job_id=%d", claimed.ID),
			nil,
		)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			require.Condition(t, func() bool {
				return false
			}, "expected 200, got %d: %s", w.Code, w.Body.String())
		}
		if js := w.Header().Get("X-Job-Status"); js != "running" {
			assert.Condition(t, func() bool {
				return false
			}, "expected X-Job-Status running, got %q", js)
		}
		if w.Body.Len() != 0 {
			assert.Condition(t, func() bool {
				return false
			}, "expected empty body, got %q", w.Body.String())
		}
	})

	t.Run("POST returns 405", func(t *testing.T) {
		req := httptest.NewRequest(
			http.MethodPost,
			fmt.Sprintf("/api/job/log?job_id=%d", job.ID),
			nil,
		)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)
		if w.Code != http.StatusMethodNotAllowed {
			assert.Condition(t, func() bool {
				return false
			}, "expected 405, got %d", w.Code)
		}
	})
}

func TestHandleJobLogOffset(t *testing.T) {
	server, db, tmpDir := newTestServer(t)
	t.Setenv("ROBOREV_DATA_DIR", tmpDir)

	repo, err := db.GetOrCreateRepo(
		filepath.Join(tmpDir, "testrepo"),
	)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "GetOrCreateRepo: %v", err)
	}
	job, err := db.EnqueueJob(storage.EnqueueOpts{
		RepoID: repo.ID,
		GitRef: "def456",
		Agent:  "test",
	})
	if err != nil {
		require.Condition(t, func() bool {
			return false

			// Create log file with two JSONL lines.
		}, "EnqueueJob: %v", err)
	}

	logDir := JobLogDir()
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "MkdirAll: %v", err)
	}
	line1 := `{"type":"assistant","message":{"content":[{"type":"text","text":"first"}]}}` + "\n"
	line2 := `{"type":"assistant","message":{"content":[{"type":"text","text":"second"}]}}` + "\n"
	logContent := line1 + line2
	if err := os.WriteFile(
		JobLogPath(job.ID), []byte(logContent), 0o644,
	); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "WriteFile: %v", err)
	}

	t.Run("offset=0 returns full content", func(t *testing.T) {
		req := httptest.NewRequest(
			http.MethodGet,
			fmt.Sprintf(
				"/api/job/log?job_id=%d&offset=0", job.ID,
			),
			nil,
		)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			require.Condition(t, func() bool {
				return false
			}, "expected 200, got %d", w.Code)
		}
		if w.Body.String() != logContent {
			assert.Condition(t, func() bool {
				return false
			}, "expected full content, got %q",
				w.Body.String())
		}

		// X-Log-Offset should equal file size.
		offsetStr := w.Header().Get("X-Log-Offset")
		offset, err := strconv.ParseInt(offsetStr, 10, 64)
		if err != nil {
			require.Condition(t, func() bool {
				return false
			}, "parse X-Log-Offset %q: %v", offsetStr, err)
		}
		if offset != int64(len(logContent)) {
			assert.Condition(t, func() bool {
				return false
			}, "X-Log-Offset = %d, want %d",
				offset, len(logContent))
		}
	})

	t.Run("offset returns partial content", func(t *testing.T) {
		off := len(line1)
		req := httptest.NewRequest(
			http.MethodGet,
			fmt.Sprintf(
				"/api/job/log?job_id=%d&offset=%d",
				job.ID, off,
			),
			nil,
		)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			require.Condition(t, func() bool {
				return false
			}, "expected 200, got %d", w.Code)
		}
		if w.Body.String() != line2 {
			assert.Condition(t, func() bool {
				return false
			}, "expected second line only, got %q",
				w.Body.String())
		}
	})

	t.Run("offset at end returns empty", func(t *testing.T) {
		off := len(logContent)
		req := httptest.NewRequest(
			http.MethodGet,
			fmt.Sprintf(
				"/api/job/log?job_id=%d&offset=%d",
				job.ID, off,
			),
			nil,
		)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			require.Condition(t, func() bool {
				return false
			}, "expected 200, got %d", w.Code)
		}
		if w.Body.Len() != 0 {
			assert.Condition(t, func() bool {
				return false
			}, "expected empty body, got %q",
				w.Body.String())
		}
	})

	t.Run("negative offset returns 400", func(t *testing.T) {
		req := httptest.NewRequest(
			http.MethodGet,
			fmt.Sprintf(
				"/api/job/log?job_id=%d&offset=-1", job.ID,
			),
			nil,
		)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			assert.Condition(t, func() bool {
				return false
			}, "expected 400, got %d", w.Code)
		}
	})

	t.Run("offset beyond file resets to 0", func(t *testing.T) {
		req := httptest.NewRequest(
			http.MethodGet,
			fmt.Sprintf(
				"/api/job/log?job_id=%d&offset=999999",
				job.ID,
			),
			nil,
		)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			require.Condition(t, func() bool {
				return false
			}, "expected 200, got %d", w.Code)
		}
		// Should return full content since offset was clamped.
		if w.Body.String() != logContent {
			assert.Condition(t, func() bool {
				return false
			}, "expected full content after clamp, got %q",
				w.Body.String())
		}
	})

	t.Run("running job snaps to newline boundary", func(t *testing.T) {
		// Claim the existing queued job first so the next
		// ClaimJob picks up job2.
		if _, err := db.ClaimJob("worker-drain"); err != nil {
			require.Condition(t, func() bool {
				return false
			}, "ClaimJob (drain): %v", err)
		}

		// Create a new running job with a partial line at end.
		job2, err := db.EnqueueJob(storage.EnqueueOpts{
			RepoID: repo.ID,
			GitRef: "ghi789",
			Agent:  "test",
		})
		if err != nil {
			require.Condition(t, func() bool {
				return false
			}, "EnqueueJob: %v", err)
		}
		if _, err := db.ClaimJob("worker-test2"); err != nil {
			require.Condition(t, func() bool {
				return false

				// Write a complete line + partial line.
			}, "ClaimJob: %v", err)
		}

		completeLine := `{"type":"assistant","message":{"content":[{"type":"text","text":"done"}]}}` + "\n"
		partialLine := `{"type":"assistant","message":{"content":`
		if err := os.WriteFile(
			JobLogPath(job2.ID),
			[]byte(completeLine+partialLine),
			0o644,
		); err != nil {
			require.Condition(t, func() bool {
				return false
			}, "WriteFile: %v", err)
		}

		req := httptest.NewRequest(
			http.MethodGet,
			fmt.Sprintf(
				"/api/job/log?job_id=%d", job2.ID,
			),
			nil,
		)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			require.Condition(t, func() bool {
				return false
			}, "expected 200, got %d", w.Code)
		}

		// Should only return up to the newline, not the partial.
		body := w.Body.String()
		if body != completeLine {
			assert.Condition(t, func() bool {
				return false
			}, "expected only complete line, got %q",
				body)
		}

		// X-Log-Offset should point past the newline.
		offsetStr := w.Header().Get("X-Log-Offset")
		offset, err := strconv.ParseInt(offsetStr, 10, 64)
		if err != nil {
			require.Condition(t, func() bool {
				return false
			}, "parse X-Log-Offset: %v", err)
		}
		if offset != int64(len(completeLine)) {
			assert.Condition(t, func() bool {
				return false
			}, "X-Log-Offset = %d, want %d",
				offset, len(completeLine))
		}
	})
}

func TestJobLogSafeEnd(t *testing.T) {
	t.Run("empty file", func(t *testing.T) {
		f := writeTempFile(t, []byte{})
		if got := jobLogSafeEnd(f, 0); got != 0 {
			assert.Condition(t, func() bool {
				return false
			}, "expected 0, got %d", got)
		}
	})

	t.Run("ends with newline", func(t *testing.T) {
		data := []byte("line1\nline2\n")
		f := writeTempFile(t, data)
		got := jobLogSafeEnd(f, int64(len(data)))
		assert.Equal(t, int64(len(data)), got, "full data length should be returned when data ends with newline")
	})

	t.Run("partial line at end", func(t *testing.T) {
		data := []byte("line1\npartial")
		f := writeTempFile(t, data)
		got := jobLogSafeEnd(f, int64(len(data)))
		assert.Equal(t, int64(6), got, "\"line1\\n\" should return index 6")
	})

	t.Run("no newlines at all", func(t *testing.T) {
		data := []byte("no-newlines-here")
		f := writeTempFile(t, data)
		got := jobLogSafeEnd(f, int64(len(data)))
		assert.Equal(t, int64(0), got, "files without newlines should return 0")
	})

	t.Run("large partial beyond 64KB", func(t *testing.T) {
		// A complete line followed by a partial line > 64KB.
		// The chunked backward scan should still find the newline.
		completeLine := "line1\n"
		partial := strings.Repeat("x", 100*1024) // 100KB
		data := []byte(completeLine + partial)
		f := writeTempFile(t, data)
		got := jobLogSafeEnd(f, int64(len(data)))
		want := int64(len(completeLine))
		assert.Equal(t, want, got, "partial chunk should align at the end of complete line")
	})
}

func writeTempFile(t *testing.T, data []byte) *os.File {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "logtest-*")
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "CreateTemp: %v", err)
	}
	t.Cleanup(func() { f.Close() })
	if _, err := f.Write(data); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Write: %v", err)
	}
	return f
}
