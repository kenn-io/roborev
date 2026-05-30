package daemon

import (
	"encoding/json"
	"log"
	"maps"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.kenn.io/roborev/internal/config"
)

// ActivityEntry represents a single activity log entry
type ActivityEntry struct {
	Timestamp time.Time         `json:"ts"`
	Event     string            `json:"event"`
	Component string            `json:"component"`
	Message   string            `json:"message"`
	Details   map[string]string `json:"details,omitempty"`
}

// ActivityLog manages activity logging to a JSONL file and maintains
// an in-memory ring buffer. Follows the same pattern as ErrorLog.
type ActivityLog struct {
	mu            sync.Mutex
	file          *os.File
	path          string
	recent        []ActivityEntry
	maxRecent     int
	writeIdx      int
	count         int
	writeCount    int // Writes since last size check
	maxSize       int64
	checkInterval int
}

const activityLogCapacity = 500

// maxActivityLogSize is the threshold at which the log file is
// truncated on open. 5MB is generous for structured JSONL entries
// (~200 bytes each) and covers months of typical daemon activity.
const maxActivityLogSize = 5 * 1024 * 1024

// NewActivityLog creates a new activity log writer.
// If the existing file exceeds maxActivityLogSize it is truncated.
func NewActivityLog(path string) (*ActivityLog, error) {
	return newActivityLogWithConfig(path, maxActivityLogSize, rotateCheckInterval)
}

func newActivityLogWithConfig(path string, maxSize int64, checkInterval int) (*ActivityLog, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}

	if err := truncateIfOversized(path, maxSize); err != nil {
		log.Printf("Activity log: failed to truncate %s: %v", path, err)
	}

	file, err := os.OpenFile(
		path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644,
	)
	if err != nil {
		return nil, err
	}

	return &ActivityLog{
		file:          file,
		path:          path,
		recent:        make([]ActivityEntry, activityLogCapacity),
		maxRecent:     activityLogCapacity,
		maxSize:       maxSize,
		checkInterval: checkInterval,
	}, nil
}

// DefaultActivityLogPath returns the default path for the activity log
func DefaultActivityLogPath() string {
	return filepath.Join(config.DataDir(), "activity.log")
}

// Log writes an activity entry to both file and in-memory buffer.
// The details map is copied; callers may safely mutate it after calling Log.
func (a *ActivityLog) Log(
	event, component, message string,
	details map[string]string,
) {
	entry := ActivityEntry{
		Timestamp: time.Now(),
		Event:     event,
		Component: component,
		Message:   message,
		Details:   copyDetails(details),
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if a.file != nil {
		data, err := json.Marshal(entry)
		if err == nil {
			_, _ = a.file.Write(data)
			_, _ = a.file.Write([]byte("\n"))
		}
		a.maybeRotate()
	}

	a.recent[a.writeIdx] = entry
	a.writeIdx = (a.writeIdx + 1) % a.maxRecent
	if a.count < a.maxRecent {
		a.count++
	}
}

// Recent returns all activity entries in the ring buffer (newest first)
func (a *ActivityLog) Recent() []ActivityEntry {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.count == 0 {
		return nil
	}

	result := make([]ActivityEntry, a.count)
	readIdx := (a.writeIdx - 1 + a.maxRecent) % a.maxRecent
	for i := range a.count {
		e := a.recent[readIdx]
		e.Details = copyDetails(e.Details)
		result[i] = e
		readIdx = (readIdx - 1 + a.maxRecent) % a.maxRecent
	}
	return result
}

// RecentN returns up to n most recent entries (newest first)
func (a *ActivityLog) RecentN(n int) []ActivityEntry {
	if n <= 0 {
		return nil
	}
	all := a.Recent()
	if len(all) <= n {
		return all
	}
	return all[:n]
}

// Close closes the activity log file
func (a *ActivityLog) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.file != nil {
		err := a.file.Close()
		a.file = nil
		return err
	}
	return nil
}

// rotateCheckInterval is how often (in writes) we stat the file to
// check its size. Each entry is ~200 bytes, so 1000 writes ≈ 200KB.
const rotateCheckInterval = 1000

// maybeRotate checks the log file size every checkInterval
// writes and truncates it to zero if over maxSize. Must
// be called with a.mu held. Closes and reopens the file because
// Windows does not allow Truncate on an O_APPEND handle.
func (a *ActivityLog) maybeRotate() {
	a.writeCount++
	if a.writeCount < a.checkInterval {
		return
	}
	a.writeCount = 0

	info, err := a.file.Stat()
	if err != nil || info.Size() <= a.maxSize {
		return
	}

	a.file.Close()
	f, err := os.OpenFile(
		a.path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644,
	)
	if err != nil {
		log.Printf("Activity log: rotate reopen failed, retrying append: %v", err)
		// Fall back to append mode so logging isn't permanently disabled
		f, err = os.OpenFile(
			a.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644,
		)
		if err != nil {
			log.Printf("Activity log: fallback reopen also failed: %v", err)
			a.file = nil
			return
		}
	}
	a.file = f
}

// copyDetails returns a shallow copy of a string map.
// Returns nil for nil input.
func copyDetails(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	cp := make(map[string]string, len(m))
	maps.Copy(cp, m)
	return cp
}

// truncateIfOversized removes the file at path if it exceeds limit bytes.
func truncateIfOversized(path string, limit int64) error {
	info, err := os.Stat(path)
	if err != nil {
		return nil // File doesn't exist or can't stat — nothing to do.
	}
	if info.Size() > limit {
		return os.Remove(path)
	}
	return nil
}
