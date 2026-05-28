package daemon

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func createTestActivityLog(t *testing.T) (*ActivityLog, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "activity.log")
	al, err := NewActivityLog(path)
	require.NoError(t, err, "NewActivityLog failed: %v")

	t.Cleanup(func() { al.Close() })
	return al, path
}

func TestActivityLog_WriteAndRecent(t *testing.T) {
	al, _ := createTestActivityLog(t)

	tests := []struct {
		event     string
		component string
		message   string
	}{
		{"daemon.started", "server", "daemon started on :7373"},
		{"job.enqueued", "server", "job 1 enqueued"},
		{"job.started", "worker", "job 1 started"},
		{"job.completed", "worker", "job 1 completed"},
	}

	for _, tt := range tests {
		al.Log(tt.event, tt.component, tt.message, nil)
	}

	recent := al.Recent()
	if len(recent) != len(tests) {
		assert.Len(t, recent, len(tests), "expected %d entries, got %d", len(tests), len(recent))
	}

	for i, tt := range tests {
		got := recent[len(tests)-1-i]
		assert.Equal(t, tt.event, got.Event, "entry %d: event = %q, want %q", i, got.Event, tt.event)
		assert.Equal(t, tt.component, got.Component, "entry %d: component = %q, want %q", i, got.Component, tt.component)
		assert.Equal(t, tt.message, got.Message, "entry %d: message = %q, want %q", i, got.Message, tt.message)
	}
}

func TestActivityLog_RecentN(t *testing.T) {
	al, _ := createTestActivityLog(t)

	for i := range 10 {
		al.Log("test", "test", fmt.Sprintf("msg %d", i), nil)
	}

	got := al.RecentN(3)
	if len(got) != 3 {
		assert.Len(t, got, 3, "assertion failed")
	}
	assert.Equal(t, "msg 9", got[0].Message, "first entry = %q, want %q", got[0].Message, "msg 9")

	got = al.RecentN(100)
	assert.Len(t, got, 10,
		"expected 10 entries, got %d", len(got))
}

func TestActivityLog_RingBuffer(t *testing.T) {
	al, _ := createTestActivityLog(t)

	total := activityLogCapacity + 100
	for i := range total {
		al.Log("test", "test", fmt.Sprintf("msg %d", i), nil)
	}

	recent := al.Recent()
	if len(recent) != activityLogCapacity {
		assert.Len(t, recent, activityLogCapacity, "expected %d entries (ring buffer cap), got %d", activityLogCapacity, len(recent))
	}
	assert.Equal(t, fmt.Sprintf("msg %d", total-1), recent[0].Message, "newest = %q, want %q", recent[0].Message, fmt.Sprintf("msg %d", total-1))

	oldest := recent[activityLogCapacity-1]
	want := fmt.Sprintf("msg %d", total-activityLogCapacity)
	assert.Equal(t, want, oldest.Message, "oldest = %q, want %q", oldest.Message, want)
}

func TestActivityLog_FileFormat(t *testing.T) {
	al, path := createTestActivityLog(t)

	al.Log("daemon.started", "server", "started", nil)
	al.Log(
		"job.enqueued", "server", "job enqueued",
		map[string]string{"job_id": "42", "agent": "test"},
	)
	al.Close()

	content, err := os.ReadFile(path)
	require.NoError(t, err, "read file: %v")

	decoder := json.NewDecoder(bytes.NewReader(content))

	want := []ActivityEntry{
		{Event: "daemon.started", Component: "server", Message: "started"},
		{Event: "job.enqueued", Component: "server", Message: "job enqueued", Details: map[string]string{"job_id": "42", "agent": "test"}},
	}

	for i, w := range want {
		var got ActivityEntry
		err := decoder.Decode(&got)
		require.NoError(t, err, "decode entry %d: %v", i, err)
		assert.False(t, got.Event != w.Event || got.Component != w.Component || got.Message != w.Message,
			"entry %d = %+v, want %+v", i, got, w)

		if len(got.Details) == 0 && len(w.Details) == 0 {
			continue
		}
		assert.True(t, maps.Equal(got.Details, w.Details),
			"entry %d details = %v, want %v", i, got.Details, w.Details)

	}
}

func TestActivityLog_Details(t *testing.T) {
	al, _ := createTestActivityLog(t)

	details := map[string]string{
		"version": "1.0.0",
		"addr":    "127.0.0.1:7373",
		"pid":     "1234",
	}
	al.Log("daemon.started", "server", "started", details)

	recent := al.Recent()
	if len(recent) != 1 {
		assert.Len(t, recent, 1, "assertion failed: expected 1 entry, got %d", len(recent))
	}

	got := recent[0].Details
	if !maps.Equal(got, details) {
		assert.Equal(t, details, got, "details = %v, want %v", got, details)
	}
}

func TestActivityLog_DetailsCopied(t *testing.T) {
	al, _ := createTestActivityLog(t)

	details := map[string]string{"key": "original"}
	al.Log("test", "test", "msg", details)

	details["key"] = "mutated"
	details["extra"] = "added"

	recent := al.Recent()
	assert.Equal(t, "original", recent[0].Details["key"], "stored details mutated: got %q, want %q", recent[0].Details["key"], "original")

	assert.NotContains(t, recent[0].Details, "extra", "stored details gained extra key from caller mutation")

	recent[0].Details["key"] = "returned-mutated"
	again := al.Recent()
	assert.Equal(t, "original", again[0].Details["key"],
		"ring buffer mutated via returned entry: got %q, want %q",
		again[0].Details["key"], "original")
}

func TestActivityLog_RecentN_Negative(t *testing.T) {
	al, _ := createTestActivityLog(t)
	al.Log("test", "test", "msg", nil)

	got := al.RecentN(-1)
	assert.Nil(t, got,
		"RecentN(-1) = %v, want nil", got)

	got = al.RecentN(0)
	assert.Nil(t, got,
		"RecentN(0) = %v, want nil", got)
}

func TestActivityLog_FileTruncation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "activity.log")

	data := make([]byte, maxActivityLogSize+1)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		require.NoError(t, err, "write seed file: %v")
	}

	al, err := NewActivityLog(path)
	require.NoError(t, err, "NewActivityLog: %v")

	defer al.Close()

	al.Log("test", "test", "after truncation", nil)
	al.Close()

	info, err := os.Stat(path)
	require.NoError(t, err, "stat: %v")

	assert.LessOrEqual(t, info.Size(), int64(1024),
		"file should be small after truncation, got %d bytes",
		info.Size())
}

func TestActivityLog_RuntimeRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "activity.log")

	const testMaxSize = 1024
	const testInterval = 10

	al, err := newActivityLogWithConfig(path, testMaxSize, testInterval)
	require.NoError(t, err, "newActivityLogWithConfig: %v")

	defer al.Close()

	bigMsg := string(bytes.Repeat([]byte("a"), 200))

	for i := range testInterval + 2 {
		al.Log("test", "test", fmt.Sprintf("entry-%d", i),
			map[string]string{"big": bigMsg})
	}

	al.Close()
	info, err := os.Stat(path)
	require.NoError(t, err, "stat: %v")

	assert.
		LessOrEqual(t, info.Size(), int64(testMaxSize),

			"file should be under cap after rotation, got %d bytes",
			info.Size())
}

func TestActivityLog_RotationRecovery(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "activity.log")

	const testMaxSize = 1024
	const testInterval = 2

	al, err := newActivityLogWithConfig(path, testMaxSize, testInterval)
	require.NoError(t, err, "newActivityLogWithConfig: %v")

	defer al.Close()

	al.Log("test", "test", "msg1", nil)

	bigMsg := string(make([]byte, testMaxSize+1024))
	al.Log("test", "test", bigMsg, nil)

	al.Log("post-rotate", "test", "still logging", nil)

	entries := al.RecentN(1)
	assert.NotEmpty(t, entries)
	assert.Equal(t, "post-rotate", entries[0].Event)

	al.Close()
	info, statErr := os.Stat(path)
	require.NoError(t, statErr)
	assert.NotZero(t, info.Size())
}
