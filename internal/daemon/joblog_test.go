package daemon

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestJobLogDir(t *testing.T) {
	tmpDir := setupTestEnv(t)
	got := JobLogDir()
	want := filepath.Join(tmpDir, "logs", "jobs")
	assert.Equal(t, want, got)
}

func TestJobLogPath(t *testing.T) {
	tmpDir := setupTestEnv(t)
	got := JobLogPath(42)
	want := filepath.Join(tmpDir, "logs", "jobs", "42.log")
	assert.Equal(t, want, got)
}

func assertStrictPerms(t *testing.T, path string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return
	}
	info, err := os.Stat(path)
	require.NoError(t, err, "stat")
	assert.Zero(t, info.Mode().Perm()&0o077, "permissions for %s should be strict", path)
}

func TestOpenJobLog(t *testing.T) {
	t.Run("creates_and_writes", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)

		setupTestEnv(t)

		f := openJobLog(99)
		require.NotNil(f, "openJobLog returned nil")
		defer f.Close()

		// Write some data and verify it lands on disk
		_, err := f.WriteString("hello\n")
		require.NoError(err, "write")
		f.Close()

		data, err := os.ReadFile(JobLogPath(99))
		require.NoError(err, "read")
		assert.Equal("hello\n", string(data))
	})

	t.Run("strict_permissions", func(t *testing.T) {
		setupTestEnv(t)
		f := openJobLog(99)
		require.NotNil(t, f, "openJobLog returned nil")
		f.Close()

		assertStrictPerms(t, JobLogPath(99))
		assertStrictPerms(t, JobLogDir())
	})
}

func TestOpenJobLog_TightensPermissivePerms(t *testing.T) {
	require := require.New(t)

	if runtime.GOOS == "windows" {
		t.Skip("POSIX permissions not applicable on Windows")
	}
	setupTestEnv(t)

	// Pre-create dir and file with permissive modes, simulating
	// an install upgraded from a version that used 0755/0644.
	dir := JobLogDir()
	require.NoError(os.MkdirAll(dir, 0o755))
	path := JobLogPath(500)
	require.NoError(os.WriteFile(path, []byte("old"), 0o644))

	f := openJobLog(500)
	require.NotNil(f, "openJobLog returned nil")
	f.Close()

	assertStrictPerms(t, dir)
	assertStrictPerms(t, path)
}

func createLogFile(t *testing.T, path, content string, mtime time.Time) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	require.NoError(t, os.Chtimes(path, mtime, mtime))
}

func TestCleanJobLogs(t *testing.T) {
	t.Run("removes_old_keeps_new", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)

		setupTestEnv(t)

		dir := JobLogDir()
		require.NoError(os.MkdirAll(dir, 0o755))

		// Create an "old" log file by writing and then back-dating its mtime
		oldPath := filepath.Join(dir, "1.log")
		createLogFile(t, oldPath, "old", time.Now().Add(-8*24*time.Hour))

		// Create a "new" log file (default mtime = now)
		newPath := filepath.Join(dir, "2.log")
		createLogFile(t, newPath, "new", time.Now())

		// Create a non-log file (should be ignored)
		txtPath := filepath.Join(dir, "notes.txt")
		createLogFile(t, txtPath, "ignore", time.Now())

		removed := CleanJobLogs(7 * 24 * time.Hour)
		assert.Equal(1, removed)

		// Old file should be gone
		_, err := os.Stat(oldPath)
		assert.True(os.IsNotExist(err), "old log file should be removed")

		// New file should remain
		_, err = os.Stat(newPath)
		require.NoError(err, "new log file should still exist")

		// Non-log file should remain
		_, err = os.Stat(txtPath)
		require.NoError(err, "non-log file should still exist")
	})

	t.Run("no_dir", func(t *testing.T) {
		setupTestEnv(t)
		// No logs/jobs directory exists — should return 0 without error
		removed := CleanJobLogs(7 * 24 * time.Hour)
		assert.Zero(t, removed)
	})
}

func TestJobLogWriter(t *testing.T) {
	t.Run("writes_immediately", func(t *testing.T) {
		setupTestEnv(t)
		w := newJobLogWriter(200)
		defer w.Close()

		n, err := w.Write([]byte("line 1\n"))
		if err != nil {
			require.Condition(t, func() bool {
				return false
			}, "Write error: %v", err)
		}
		if n != 7 {
			assert.Condition(t, func() bool {
				return false
			}, "Write returned %d, want 7", n)
		}

		n, err = w.Write([]byte("line 2\n"))
		if err != nil {
			require.Condition(t, func() bool {
				return false
			}, "Write error: %v", err)
		}
		if n != 7 {
			assert.Condition(t, func() bool {
				return false
			}, "Write returned %d, want 7", n)
		}

		if err := w.Close(); err != nil {
			require.Condition(t, func() bool {
				return false
			}, "Close: %v", err)
		}
		data, _ := os.ReadFile(JobLogPath(200))
		if string(data) != "line 1\nline 2\n" {
			assert.Condition(t, func() bool {
				return false
			}, "contents = %q", data)
		}
	})

	t.Run("append_mode_preserves_existing_log", func(t *testing.T) {
		setupTestEnv(t)
		require.NoError(t, os.MkdirAll(JobLogDir(), 0o700))
		require.NoError(t, os.WriteFile(JobLogPath(205), []byte("classifier\n"), 0o600))

		w := newAppendingJobLogWriter(205)
		_, err := w.Write([]byte("review\n"))
		require.NoError(t, err)
		require.NoError(t, w.Close())

		data, err := os.ReadFile(JobLogPath(205))
		require.NoError(t, err)
		assert.Equal(t, "classifier\nreview\n", string(data))
	})

	t.Run("default_mode_truncates_existing_log", func(t *testing.T) {
		setupTestEnv(t)
		require.NoError(t, os.MkdirAll(JobLogDir(), 0o700))
		require.NoError(t, os.WriteFile(JobLogPath(206), []byte("old\n"), 0o600))

		w := newJobLogWriter(206)
		_, err := w.Write([]byte("new\n"))
		require.NoError(t, err)
		require.NoError(t, w.Close())

		data, err := os.ReadFile(JobLogPath(206))
		require.NoError(t, err)
		assert.Equal(t, "new\n", string(data))
	})

	t.Run("retries_after_initial_open_failure", func(t *testing.T) {
		setupTestEnv(t)
		prevRetry := jobLogOpenRetryInterval
		jobLogOpenRetryInterval = 0
		t.Cleanup(func() {
			jobLogOpenRetryInterval = prevRetry
		})

		logsDir := filepath.Join(filepath.Dir(JobLogDir()))
		if err := os.MkdirAll(logsDir, 0o700); err != nil {
			require.Condition(t, func() bool {
				return false
			}, "MkdirAll: %v", err)
		}
		if err := os.WriteFile(JobLogDir(), []byte("blocked"), 0o600); err != nil {
			require.Condition(t, func() bool {
				return false
			}, "WriteFile: %v", err)
		}

		w := newJobLogWriter(201)
		if _, err := w.Write([]byte("line 1\n")); err != nil {
			require.Condition(t, func() bool {
				return false
			}, "Write while blocked: %v", err)
		}

		if err := os.Remove(JobLogDir()); err != nil {
			require.Condition(t, func() bool {
				return false
			}, "Remove blocker: %v", err)
		}

		if _, err := w.Write([]byte("line 2\n")); err != nil {
			require.Condition(t, func() bool {
				return false
			}, "Write after recovery: %v", err)
		}
		if err := w.Close(); err != nil {
			require.Condition(t, func() bool {
				return false
			}, "Close: %v", err)
		}

		data, err := os.ReadFile(JobLogPath(201))
		if err != nil {
			require.Condition(t, func() bool {
				return false
			}, "ReadFile: %v", err)
		}
		if string(data) != "line 1\nline 2\n" {
			assert.Condition(t, func() bool {
				return false
			}, "contents = %q, want %q", data, "line 1\nline 2\n")
		}
	})

	t.Run("flushes_buffer_on_close_after_recovery", func(t *testing.T) {
		setupTestEnv(t)
		prevRetry := jobLogOpenRetryInterval
		jobLogOpenRetryInterval = 0
		t.Cleanup(func() {
			jobLogOpenRetryInterval = prevRetry
		})

		logsDir := filepath.Join(filepath.Dir(JobLogDir()))
		if err := os.MkdirAll(logsDir, 0o700); err != nil {
			require.Condition(t, func() bool {
				return false
			}, "MkdirAll: %v", err)
		}
		if err := os.WriteFile(JobLogDir(), []byte("blocked"), 0o600); err != nil {
			require.Condition(t, func() bool {
				return false
			}, "WriteFile: %v", err)
		}

		w := newJobLogWriter(202)
		if _, err := w.Write([]byte("buffered\n")); err != nil {
			require.Condition(t, func() bool {
				return false
			}, "Write while blocked: %v", err)
		}

		if err := os.Remove(JobLogDir()); err != nil {
			require.Condition(t, func() bool {
				return false
			}, "Remove blocker: %v", err)
		}
		if err := w.Close(); err != nil {
			require.Condition(t, func() bool {
				return false
			}, "Close: %v", err)
		}

		data, err := os.ReadFile(JobLogPath(202))
		if err != nil {
			require.Condition(t, func() bool {
				return false
			}, "ReadFile: %v", err)
		}
		if string(data) != "buffered\n" {
			assert.Condition(t, func() bool {
				return false
			}, "contents = %q, want %q", data, "buffered\n")
		}
	})

	t.Run("writes_log_before_companion_failure", func(t *testing.T) {
		setupTestEnv(t)
		w := newJobLogWriter(203)
		defer w.Close()

		mw := io.MultiWriter(w, failingWriter{})
		if _, err := mw.Write([]byte("persist me\n")); err == nil {
			require.Condition(t, func() bool {
				return false
			}, "expected companion writer failure")
		}
		if err := w.Close(); err != nil {
			require.Condition(t, func() bool {
				return false
			}, "Close: %v", err)
		}

		data, err := os.ReadFile(JobLogPath(203))
		if err != nil {
			require.Condition(t, func() bool {
				return false
			}, "ReadFile: %v", err)
		}
		if string(data) != "persist me\n" {
			assert.Condition(t, func() bool {
				return false
			}, "contents = %q, want %q", data, "persist me\n")
		}
	})

	t.Run("partial_direct_write_buffers_only_suffix", func(t *testing.T) {
		pw := &partialErrorWriteCloser{partial: 3}
		w := &jobLogWriter{jobID: 204, f: pw}

		if _, err := w.Write([]byte("abcdef")); err != nil {
			require.Condition(t, func() bool {
				return false
			}, "Write: %v", err)
		}
		if got := pw.String(); got != "abc" {
			require.Condition(t, func() bool {
				return false
			}, "written prefix = %q, want %q", got, "abc")
		}
		if got := w.buf.String(); got != "def" {
			require.Condition(t, func() bool {
				return false
			}, "buffered suffix = %q, want %q", got, "def")
		}

		w.f = pw
		if err := w.Close(); err != nil {
			require.Condition(t, func() bool {
				return false
			}, "Close: %v", err)
		}
		if got := pw.String(); got != "abcdef" {
			require.Condition(t, func() bool {
				return false
			}, "final content = %q, want %q", got, "abcdef")
		}
	})

	t.Run("partial_flush_trims_written_prefix", func(t *testing.T) {
		pw := &partialErrorWriteCloser{partial: 3}
		w := &jobLogWriter{jobID: 205, f: pw}
		w.buf.WriteString("abcdef")

		err := w.flushBufferedLocked()
		if err == nil {
			require.Condition(t, func() bool {
				return false
			}, "expected flush error")
		}
		if got := pw.String(); got != "abc" {
			require.Condition(t, func() bool {
				return false
			}, "written prefix = %q, want %q", got, "abc")
		}
		if got := w.buf.String(); got != "def" {
			require.Condition(t, func() bool {
				return false
			}, "remaining buffer = %q, want %q", got, "def")
		}

		if err := w.flushBufferedLocked(); err != nil {
			require.Condition(t, func() bool {
				return false
			}, "second flush: %v", err)
		}
		if got := pw.String(); got != "abcdef" {
			require.Condition(t, func() bool {
				return false
			}, "final content = %q, want %q", got, "abcdef")
		}
	})

	t.Run("partial_notice_flush_preserves_order_before_buffer", func(t *testing.T) {
		pw := &partialErrorWriteCloser{partial: 3}
		w := &jobLogWriter{jobID: 206, f: pw, dropped: 5, noticed: 5}
		w.notice.WriteString("NOTICE")
		w.buf.WriteString("tail")

		err := w.flushBufferedLocked()
		if err == nil {
			require.Condition(t, func() bool {
				return false
			}, "expected flush error")
		}
		if got := pw.String(); got != "NOT" {
			require.Condition(t, func() bool {
				return false
			}, "written prefix = %q, want %q", got, "NOT")
		}
		if got := w.notice.String(); got != "ICE" {
			require.Condition(t, func() bool {
				return false
			}, "remaining notice = %q, want %q", got, "ICE")
		}
		if got := w.buf.String(); got != "tail" {
			require.Condition(t, func() bool {
				return false
			}, "buffer should remain queued until notice completes, got %q", got)
		}

		if err := w.flushBufferedLocked(); err != nil {
			require.Condition(t, func() bool {
				return false
			}, "second flush: %v", err)
		}
		if got := pw.String(); got != "NOTICEtail" {
			require.Condition(t, func() bool {
				return false
			}, "final content = %q, want %q", got, "NOTICEtail")
		}
	})
}

type failingWriter struct{}

func (failingWriter) Write(_ []byte) (int, error) {
	return 0, errors.New("boom")
}

type partialErrorWriteCloser struct {
	buf     bytes.Buffer
	partial int
	failed  bool
}

func (w *partialErrorWriteCloser) Write(p []byte) (int, error) {
	if !w.failed {
		w.failed = true
		n := min(w.partial, len(p))
		if n > 0 {
			_, _ = w.buf.Write(p[:n])
		}
		return n, errors.New("short write")
	}
	return w.buf.Write(p)
}

func (w *partialErrorWriteCloser) Close() error { return nil }

func (w *partialErrorWriteCloser) String() string { return w.buf.String() }

func TestReadJobLog(t *testing.T) {
	setupTestEnv(t)

	t.Run("existing", func(t *testing.T) {
		f := openJobLog(300)
		if f == nil {
			require.Condition(t, func() bool {
				return false
			}, "openJobLog returned nil")
		}
		_, _ = f.WriteString("log content")
		f.Close()

		data, err := ReadJobLog(300)
		if err != nil {
			require.Condition(t, func() bool {
				return false
			}, "ReadJobLog: %v", err)
		}
		if string(data) != "log content" {
			assert.Condition(t, func() bool {
				return false
			}, "contents = %q, want %q", data, "log content")
		}
	})

	t.Run("missing", func(t *testing.T) {
		_, err := ReadJobLog(999)
		if err == nil {
			assert.Condition(t, func() bool {
				return false
			}, "ReadJobLog should error for missing file")
		}
	})
}

func TestJobLogExists(t *testing.T) {
	setupTestEnv(t)

	if JobLogExists(400) {
		assert.Condition(t, func() bool {
			return false
		}, "should not exist before creation")
	}

	f := openJobLog(400)
	if f == nil {
		require.Condition(t, func() bool {
			return false
		}, "openJobLog returned nil")
	}
	f.Close()

	if !JobLogExists(400) {
		assert.Condition(t, func() bool {
			return false
		}, "should exist after creation")
	}
}

func TestParseJobIDFromLogName(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		wantID int64
		wantOK bool
	}{
		{"valid", "42.log", 42, true},
		{"large", "12345.log", 12345, true},
		{"no_suffix", "42.txt", 0, false},
		{"not_numeric", "abc.log", 0, false},
		{"negative", "-1.log", 0, false},
		{"zero", "0.log", 0, false},
		{"empty", ".log", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, ok := ParseJobIDFromLogName(tt.input)
			if id != tt.wantID || ok != tt.wantOK {
				assert.Condition(t, func() bool {
					return false
				}, "ParseJobIDFromLogName(%q) = (%d, %v), want (%d, %v)",
					tt.input, id, ok, tt.wantID, tt.wantOK)
			}
		})
	}
}
