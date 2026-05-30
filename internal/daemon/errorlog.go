package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.kenn.io/roborev/internal/config"
)

// ErrorEntry represents a single error log entry
type ErrorEntry struct {
	Timestamp time.Time `json:"ts"`
	Level     string    `json:"level"`     // "error", "warn"
	Component string    `json:"component"` // "worker", "sync", "server"
	Message   string    `json:"message"`
	JobID     int64     `json:"job_id,omitempty"`
}

// MaxErrorLogEntries is the maximum number of error log entries kept in memory
const MaxErrorLogEntries = 100

// ErrorLog manages error logging to a file and maintains an in-memory ring buffer
type ErrorLog struct {
	mu        sync.Mutex
	file      *os.File
	path      string
	recent    []ErrorEntry // Ring buffer of last N errors
	maxRecent int
	writeIdx  int // Next write position in ring buffer
	count     int // Total entries in ring buffer (up to maxRecent)
}

// NewErrorLog creates a new error log writer
func NewErrorLog(path string) (*ErrorLog, error) {
	// Ensure directory exists
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}

	// Open file in append mode
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}

	return &ErrorLog{
		file:      file,
		path:      path,
		recent:    make([]ErrorEntry, MaxErrorLogEntries),
		maxRecent: MaxErrorLogEntries,
	}, nil
}

// DefaultErrorLogPath returns the default path for the error log
func DefaultErrorLogPath() string {
	return filepath.Join(config.DataDir(), "errors.log")
}

// Log writes an error entry to both file and in-memory buffer
func (e *ErrorLog) Log(level, component, message string, jobID int64) {
	entry := ErrorEntry{
		Timestamp: time.Now(),
		Level:     level,
		Component: component,
		Message:   message,
		JobID:     jobID,
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	// Write to file as JSONL
	if e.file != nil {
		data, err := json.Marshal(entry)
		if err == nil {
			_, _ = e.file.Write(data)
			_, _ = e.file.Write([]byte("\n"))
		}
	}

	// Add to ring buffer
	e.recent[e.writeIdx] = entry
	e.writeIdx = (e.writeIdx + 1) % e.maxRecent
	if e.count < e.maxRecent {
		e.count++
	}
}

// LogError is a convenience method for logging errors
func (e *ErrorLog) LogError(component, message string, jobID int64) {
	e.Log("error", component, message, jobID)
}

// LogWarn is a convenience method for logging warnings
func (e *ErrorLog) LogWarn(component, message string, jobID int64) {
	e.Log("warn", component, message, jobID)
}

// Recent returns the most recent error entries (newest first)
func (e *ErrorLog) Recent() []ErrorEntry {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.count == 0 {
		return nil
	}

	result := make([]ErrorEntry, e.count)
	// Read from ring buffer in reverse chronological order
	readIdx := (e.writeIdx - 1 + e.maxRecent) % e.maxRecent
	for i := 0; i < e.count; i++ {
		result[i] = e.recent[readIdx]
		readIdx = (readIdx - 1 + e.maxRecent) % e.maxRecent
	}
	return result
}

// RecentN returns up to n most recent error entries (newest first)
func (e *ErrorLog) RecentN(n int) []ErrorEntry {
	all := e.Recent()
	if len(all) <= n {
		return all
	}
	return all[:n]
}

// Count24h returns the count of errors in the last 24 hours from the in-memory buffer.
// Note: This only counts up to maxRecent (100) entries. If error volume is high,
// the actual 24h count may be higher. For precise counts, parse the log file.
func (e *ErrorLog) Count24h() int {
	e.mu.Lock()
	defer e.mu.Unlock()

	cutoff := time.Now().Add(-24 * time.Hour)
	count := 0
	readIdx := (e.writeIdx - 1 + e.maxRecent) % e.maxRecent
	for i := 0; i < e.count; i++ {
		if e.recent[readIdx].Timestamp.After(cutoff) {
			count++
		}
		readIdx = (readIdx - 1 + e.maxRecent) % e.maxRecent
	}
	return count
}

// Close closes the error log file
func (e *ErrorLog) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.file != nil {
		err := e.file.Close()
		e.file = nil
		return err
	}
	return nil
}
