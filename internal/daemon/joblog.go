package daemon

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.kenn.io/roborev/internal/config"
)

var jobLogOpenRetryInterval = 5 * time.Second

const (
	maxBufferedJobLogBytes     = 256 * 1024
	maxNormalizedJobOutputSize = 512 * 1024
)

// JobLogDir returns the directory for per-job log files.
func JobLogDir() string {
	return filepath.Join(config.DataDir(), "logs", "jobs")
}

// JobLogPath returns the log file path for a given job ID.
func JobLogPath(jobID int64) string {
	return filepath.Join(JobLogDir(), fmt.Sprintf("%d.log", jobID))
}

func jobLogAppendMarkerPath(jobID int64) string {
	return filepath.Join(JobLogDir(), fmt.Sprintf("%d.append", jobID))
}

// markJobLogForAppend preserves classifier output for the immediately
// following promoted design-review attempt. The marker is created before the
// database row becomes claimable and is consumed exactly once by processJob.
func markJobLogForAppend(jobID int64) error {
	dir := JobLogDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create job log dir %s: %w", dir, err)
	}
	return os.WriteFile(jobLogAppendMarkerPath(jobID), nil, 0o600)
}

func consumeJobLogAppendMarker(jobID int64) bool {
	err := os.Remove(jobLogAppendMarkerPath(jobID))
	return err == nil
}

func discardJobLogAppendMarker(jobID int64) {
	if err := os.Remove(jobLogAppendMarkerPath(jobID)); err != nil && !os.IsNotExist(err) {
		log.Printf("Warning: cannot remove job log append marker for job %d: %v", jobID, err)
	}
}

func truncateJobLog(jobID int64) error {
	f, err := openJobLogFile(jobID, os.O_CREATE|os.O_WRONLY|os.O_TRUNC)
	if err != nil {
		return err
	}
	return f.Close()
}

func openJobLogFile(jobID int64, flags int) (*os.File, error) {
	dir := JobLogDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create job log dir %s: %w", dir, err)
	}
	// Tighten pre-existing directories from older installs.
	if err := os.Chmod(dir, 0o700); err != nil {
		log.Printf("Warning: cannot chmod job log dir: %v", err)
	}
	path := JobLogPath(jobID)
	f, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open job log file for job %d: %w", jobID, err)
	}
	// Tighten pre-existing files from older installs.
	if err := f.Chmod(0o600); err != nil {
		log.Printf("Warning: cannot chmod job log file: %v", err)
	}
	return f, nil
}

// openJobLog creates the log directory and opens a log file for the
// given job. Returns nil if the file cannot be created (logged, not
// fatal — the review still runs without disk logging).
func openJobLog(jobID int64) *os.File {
	f, err := openJobLogFile(jobID, os.O_CREATE|os.O_WRONLY|os.O_TRUNC)
	if err != nil {
		log.Printf("Warning: cannot create job log file for job %d: %v", jobID, err)
		return nil
	}
	return f
}

// CleanJobLogs removes log files older than maxAge and returns the
// number of files removed.
func CleanJobLogs(maxAge time.Duration) int {
	dir := JobLogDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	cutoff := time.Now().Add(-maxAge)
	removed := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".log") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			if os.Remove(filepath.Join(dir, e.Name())) == nil {
				removed++
			}
		}
	}
	return removed
}

// ReadJobLog returns the contents of the log file for a job, or an
// error if the file doesn't exist.
func ReadJobLog(jobID int64) ([]byte, error) {
	return os.ReadFile(JobLogPath(jobID))
}

// readNormalizedJobOutput returns the tail of a persisted job log in the same
// shape as live worker output. This keeps completed-job output available after
// the daemon restarts without loading an unbounded log into memory.
func readNormalizedJobOutput(jobID int64, agentName string) ([]OutputLine, error) {
	f, err := os.Open(JobLogPath(jobID))
	if err != nil {
		return nil, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	start := max(info.Size()-maxNormalizedJobOutputSize, 0)
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), maxNormalizedJobOutputSize)
	if start > 0 {
		// The tail normally begins in the middle of a record.
		scanner.Scan()
	}

	normalize := GetNormalizer(agentName)
	lines := make([]OutputLine, 0)
	for scanner.Scan() {
		line := normalize(scanner.Text())
		if line == nil {
			continue
		}
		line.Timestamp = info.ModTime()
		lines = append(lines, *line)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return lines, nil
}

// readNormalizedJobOutputForAttempt rejects a persisted log that predates the
// current attempt. Normal attempt startup truncates the file before any setup
// can fail; this timestamp check keeps the output endpoint fail-closed if that
// truncation is blocked by a filesystem error.
func readNormalizedJobOutputForAttempt(
	jobID int64, agentName string, startedAt *time.Time,
) ([]OutputLine, error) {
	if startedAt != nil {
		info, err := os.Stat(JobLogPath(jobID))
		if err != nil {
			return nil, err
		}
		if info.ModTime().Before(*startedAt) {
			return nil, nil
		}
	}
	return readNormalizedJobOutput(jobID, agentName)
}

// JobLogExists reports whether a log file exists for the given job.
func JobLogExists(jobID int64) bool {
	_, err := os.Stat(JobLogPath(jobID))
	return err == nil
}

// ParseJobIDFromLogName extracts the job ID from a log file name
// like "42.log". Returns 0, false if the name doesn't match.
func ParseJobIDFromLogName(name string) (int64, bool) {
	base, ok := strings.CutSuffix(name, ".log")
	if !ok {
		return 0, false
	}
	id, err := strconv.ParseInt(base, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

type jobLogOpenMode int

const (
	jobLogTruncate jobLogOpenMode = iota
	jobLogAppend
)

// jobLogWriter retries transient log-file open/write failures while buffering
// a bounded amount of output in memory so jobs still get on-disk logs once the
// filesystem recovers.
type jobLogWriter struct {
	mu              sync.Mutex
	jobID           int64
	f               io.WriteCloser
	buf             bytes.Buffer
	notice          bytes.Buffer
	lastTry         time.Time
	dropped         int
	noticed         int
	truncatePending bool
}

func newJobLogWriter(jobID int64) *jobLogWriter {
	return newJobLogWriterWithMode(jobID, jobLogTruncate)
}

func newAppendingJobLogWriter(jobID int64) *jobLogWriter {
	return newJobLogWriterWithMode(jobID, jobLogAppend)
}

func newJobLogWriterWithMode(jobID int64, mode jobLogOpenMode) *jobLogWriter {
	w := &jobLogWriter{
		jobID:           jobID,
		truncatePending: mode == jobLogTruncate,
	}
	w.tryOpenLocked()
	return w
}

func (w *jobLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	origLen := len(p)
	if origLen == 0 {
		return 0, nil
	}
	if w.f == nil && time.Since(w.lastTry) >= jobLogOpenRetryInterval {
		w.tryOpenLocked()
	}
	if w.f != nil {
		if err := w.flushBufferedLocked(); err == nil {
			n, err := w.f.Write(p)
			if err == nil && n == len(p) {
				return origLen, nil
			}
			if n > 0 {
				p = p[n:]
			}
			if err == nil {
				err = io.ErrShortWrite
			}
			w.handleWriteFailureLocked(err)
		} else {
			w.handleWriteFailureLocked(err)
		}
	}
	w.bufferLocked(p)
	return origLen, nil
}

func (w *jobLogWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		w.tryOpenLocked()
	}
	if w.f == nil {
		return nil
	}
	if err := w.flushBufferedLocked(); err != nil {
		w.handleWriteFailureLocked(err)
		return nil
	}
	f := w.f
	w.f = nil
	return f.Close()
}

func (w *jobLogWriter) tryOpenLocked() {
	w.lastTry = time.Now()
	flags := os.O_CREATE | os.O_WRONLY
	if w.truncatePending {
		flags |= os.O_TRUNC
	} else {
		flags |= os.O_APPEND
	}
	f, err := openJobLogFile(w.jobID, flags)
	if err != nil {
		log.Printf("Warning: cannot open job log file for job %d: %v", w.jobID, err)
		return
	}
	w.f = f
	w.truncatePending = false
}

func (w *jobLogWriter) flushBufferedLocked() error {
	for {
		if w.f == nil {
			return nil
		}
		if w.notice.Len() > 0 {
			if err := w.flushBufferLocked(&w.notice); err != nil {
				return err
			}
			w.dropped -= w.noticed
			if w.dropped < 0 {
				w.dropped = 0
			}
			w.noticed = 0
			continue
		}
		if err := w.flushBufferLocked(&w.buf); err != nil {
			return err
		}
		if w.dropped == 0 {
			return nil
		}
		w.noticed = w.dropped
		fmt.Fprintf(&w.notice,
			"[roborev] dropped %d log bytes while disk logging was unavailable\n",
			w.noticed,
		)
	}
}

func (w *jobLogWriter) handleWriteFailureLocked(err error) {
	if w.f != nil {
		_ = w.f.Close()
		w.f = nil
	}
	log.Printf("Warning: job log write failed for job %d, retrying later: %v", w.jobID, err)
}

func (w *jobLogWriter) bufferLocked(p []byte) {
	if len(p) == 0 {
		return
	}
	if w.buf.Len() >= maxBufferedJobLogBytes {
		w.dropped += len(p)
		return
	}
	remaining := maxBufferedJobLogBytes - w.buf.Len()
	if len(p) <= remaining {
		_, _ = w.buf.Write(p)
		return
	}
	_, _ = w.buf.Write(p[:remaining])
	w.dropped += len(p) - remaining
}

func (w *jobLogWriter) flushBufferLocked(buf *bytes.Buffer) error {
	for buf.Len() > 0 {
		n, err := w.f.Write(buf.Bytes())
		if n > 0 {
			buf.Next(n)
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}
