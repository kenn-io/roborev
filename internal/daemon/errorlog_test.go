package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func createTestErrorLog(t *testing.T) (*ErrorLog, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "errors.log")
	el, err := NewErrorLog(path)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "NewErrorLog failed: %v", err)
	}
	t.Cleanup(func() { el.Close() })
	return el, path
}

func seedErrorLog(el *ErrorLog, count int) {
	for i := 1; i <= count; i++ {
		el.Log("error", "worker", "error", int64(i))
	}
}

func TestNewErrorLog(t *testing.T) {
	_, path := createTestErrorLog(t)

	// Verify file was created
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		assert.Condition(t, func() bool {
			return false
		}, "Error log file was not created")
	}
}

func TestErrorLogLog(t *testing.T) {
	el, path := createTestErrorLog(t)

	// Log an error
	el.Log("error", "worker", "test error message", 123)
	el.Close()

	// Verify file content
	content, err := os.ReadFile(path)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Failed to read error log: %v", err)
	}

	var entry ErrorEntry
	decoder := json.NewDecoder(bytes.NewReader(content))
	if err := decoder.Decode(&entry); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Failed to parse error entry: %v", err)
	}

	// Verify no trailing data
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		assert.Condition(t, func() bool {
			return false
		}, "Expected EOF after first entry, got %v", err)
	}

	entry.Timestamp = time.Time{} // Clear timestamp for comparison
	expected := ErrorEntry{
		Level:     "error",
		Component: "worker",
		Message:   "test error message",
		JobID:     123,
	}
	if entry != expected {
		assert.Condition(t, func() bool {
			return false
		}, "Expected entry %+v, got %+v", expected, entry)
	}
}

func TestErrorLogRecent(t *testing.T) {
	el, _ := createTestErrorLog(t)

	// Log several errors
	for i := 1; i <= 5; i++ {
		el.Log("error", "worker", fmt.Sprintf("error %d", i), int64(i))
	}

	// Get recent errors - should be in reverse order (newest first)
	recent := el.Recent()
	if len(recent) != 5 {
		require.Condition(t, func() bool {
			return false
		}, "Expected 5 recent errors, got %d", len(recent))
	}

	// First entry should be the most recent (error 5)
	if !strings.Contains(recent[0].Message, "error 5") {
		assert.Condition(t, func() bool {
			return false
		}, "Expected first error to be 'error 5', got %q", recent[0].Message)
	}
	// Last entry should be the oldest (error 1)
	if !strings.Contains(recent[4].Message, "error 1") {
		assert.Condition(t, func() bool {
			return false
		}, "Expected last error to be 'error 1', got %q", recent[4].Message)
	}
}

func TestErrorLogRecentN(t *testing.T) {
	el, _ := createTestErrorLog(t)

	seedErrorLog(el, 10)

	// Get only 3 recent errors
	recent := el.RecentN(3)
	if len(recent) != 3 {
		require.Condition(t, func() bool {
			return false
		}, "Expected 3 recent errors, got %d", len(recent))
	}

	// Should be 10, 9, 8
	expectedIDs := []int64{10, 9, 8}
	for i, entry := range recent {
		if entry.JobID != expectedIDs[i] {
			assert.Condition(t, func() bool {
				return false
			}, "Expected index %d to be JobID %d, got %d", i, expectedIDs[i], entry.JobID)
		}
	}
}

func TestErrorLogCount24h(t *testing.T) {
	el, _ := createTestErrorLog(t)

	seedErrorLog(el, 5)

	count := el.Count24h()
	if count != 5 {
		assert.Condition(t, func() bool {
			return false
		}, "Expected count 5, got %d", count)
	}
}

func TestErrorLogRingBuffer(t *testing.T) {
	el, _ := createTestErrorLog(t)

	seedCount := MaxErrorLogEntries + 50
	seedErrorLog(el, seedCount)

	// Should only have the last MaxErrorLogEntries errors
	recent := el.Recent()
	if len(recent) != MaxErrorLogEntries {
		assert.Condition(t, func() bool {
			return false
		}, "Expected %d recent errors (ring buffer limit), got %d", MaxErrorLogEntries, len(recent))
	}

	// The oldest should be #51 (first 50 were evicted)
	if recent[MaxErrorLogEntries-1].JobID != 51 {
		assert.Condition(t, func() bool {
			return false
		}, "Expected oldest error to be #51, got #%d", recent[MaxErrorLogEntries-1].JobID)
	}
}

func TestErrorLogConcurrency(t *testing.T) {
	el, _ := createTestErrorLog(t)

	// Log from multiple goroutines
	var wg sync.WaitGroup
	for i := range 10 {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := range 10 {
				el.Log("error", "worker", "concurrent error", int64(id*10+j))
			}
		}(i)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// continued
	case <-time.After(5 * time.Second):
		require.Condition(t, func() bool {
			return false
		}, "Timeout waiting for concurrent logging to finish")
	}

	// Should have MaxErrorLogEntries entries
	recent := el.Recent()
	if len(recent) != MaxErrorLogEntries {
		assert.Condition(t, func() bool {
			return false
		}, "Expected %d entries, got %d", MaxErrorLogEntries, len(recent))
	}
}
