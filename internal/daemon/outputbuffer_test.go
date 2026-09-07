package daemon

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// simpleNormalizer matches the common pattern used in Writer tests.
func simpleNormalizer(line string) *OutputLine {
	return &OutputLine{Text: line, Type: "text"}
}

// assertLines verifies the count and content of lines for a job.
func assertLines(t *testing.T, lines []OutputLine, expected ...OutputLine) {
	t.Helper()
	assert := assert.New(t)
	require := require.New(t)

	require.Len(lines, len(expected))
	for i, want := range expected {
		actual := lines[i]
		actual.Timestamp = time.Time{}
		assert.Equal(want, actual, "line[%d]", i)
	}
}

func TestOutputBuffer_Append(t *testing.T) {
	ob := NewOutputBuffer(1024, 4096)

	ob.Append(1, OutputLine{Text: "line 1", Type: "text"})
	ob.Append(1, OutputLine{Text: "line 2", Type: "tool"})

	lines := ob.GetLines(1)
	assertLines(t, lines, OutputLine{Text: "line 1", Type: "text"}, OutputLine{Text: "line 2", Type: "tool"})
}

func TestOutputBuffer_GetLinesEmpty(t *testing.T) {
	ob := NewOutputBuffer(1024, 4096)

	lines := ob.GetLines(999)
	assert.Nil(t, lines)
}

func TestOutputBuffer_Limits(t *testing.T) {
	type action struct {
		jobID int64
		text  string
	}

	tests := []struct {
		name        string
		jobLimit    int
		globalLimit int
		actions     []action
		expectLines map[int64][]OutputLine
	}{
		{
			name:        "PerJobLimit_Eviction",
			jobLimit:    50,
			globalLimit: 1000,
			actions: []action{
				{1, strings.Repeat("a", 20)}, // 20b
				{1, strings.Repeat("b", 20)}, // 20b
				{1, strings.Repeat("c", 20)}, // 20b - should evict first
			},
			expectLines: map[int64][]OutputLine{
				1: {
					{Text: strings.Repeat("b", 20), Type: "text"},
					{Text: strings.Repeat("c", 20), Type: "text"},
				},
			},
		},
		{
			name:        "GlobalLimit_Drop",
			jobLimit:    30,
			globalLimit: 50,
			actions: []action{
				{1, strings.Repeat("a", 20)}, // 20b
				{2, strings.Repeat("b", 20)}, // 20b
				{3, strings.Repeat("c", 20)}, // 20b - would exceed 50, dropped
			},
			expectLines: map[int64][]OutputLine{
				1: {{Text: strings.Repeat("a", 20), Type: "text"}},
				2: {{Text: strings.Repeat("b", 20), Type: "text"}},
				3: {},
			},
		},
		{
			name:        "OversizedLine_Drop",
			jobLimit:    20,
			globalLimit: 1000,
			actions: []action{
				{1, "this line is way too long to fit in buffer"}, // 43b > 20b
				{1, "short"}, // 5b
			},
			expectLines: map[int64][]OutputLine{
				1: {{Text: "short", Type: "text"}},
			},
		},
		{
			name:        "GlobalLimit_PreservesExisting_WhenEvictionBlocked",
			jobLimit:    50,
			globalLimit: 80,
			actions: []action{
				{1, strings.Repeat("a", 20)}, // job1: 20
				{1, strings.Repeat("b", 20)}, // job1: 40, total: 40
				{2, strings.Repeat("c", 30)}, // job2: 30, total: 70
				// Now try to add 20 bytes to job 1
				// Per-job: 40+20=60 > 50, evicts 20 bytes
				// After eviction: job1=40, total=70-20+20=70 <= 80
				// Succeeds
				{1, strings.Repeat("d", 20)},
				// Now try to add 20 more bytes to job 1
				// Per-job: 40+20=60 > 50, evicts 20 bytes
				// After eviction: total=70-20+20=70 <= 80
				// Succeeds
				{1, strings.Repeat("e", 20)},
				// Now try to add 15 bytes to job 2
				// Per-job: 30+15=45 < 50, no eviction
				// Global: 70+15=85 > 80, REJECTED
				{2, strings.Repeat("f", 15)},
			},
			expectLines: map[int64][]OutputLine{
				1: {
					{Text: strings.Repeat("d", 20), Type: "text"},
					{Text: strings.Repeat("e", 20), Type: "text"},
				},
				2: {
					{Text: strings.Repeat("c", 30), Type: "text"},
				},
			},
		},
		{
			name:        "PerJobEviction_BlockedByGlobal",
			jobLimit:    30,
			globalLimit: 40,
			actions: []action{
				{1, strings.Repeat("a", 20)}, // job1: 20
				{2, strings.Repeat("b", 20)}, // job2: 20, total: 40
				// Try to add 25 bytes to job 1
				// Per-job: 20+25=45 > 30, would evict 20 bytes
				// After eviction: job1=0, total=40-20=20
				// Adding new line: 20+25=45 > 40 (Global Limit) - REJECTED.
				// Existing line preserved
				{1, strings.Repeat("c", 25)},
			},
			expectLines: map[int64][]OutputLine{
				1: {{Text: strings.Repeat("a", 20), Type: "text"}},
				2: {{Text: strings.Repeat("b", 20), Type: "text"}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ob := NewOutputBuffer(tt.jobLimit, tt.globalLimit)
			for _, act := range tt.actions {
				ob.Append(act.jobID, OutputLine{Text: act.text, Type: "text"})
			}

			for jobID, want := range tt.expectLines {
				assertLines(t, ob.GetLines(jobID), want...)
			}
		})
	}
}

func TestOutputBuffer_CloseJob(t *testing.T) {
	assert := assert.New(t)

	ob := NewOutputBuffer(1024, 4096)

	ob.Append(1, OutputLine{Text: "test", Type: "text"})
	assert.True(ob.IsActive(1), "expected job to be active")

	ob.CloseJob(1)
	assert.False(ob.IsActive(1), "expected job to be inactive after close")

	lines := ob.GetLines(1)
	assert.Nil(lines, "expected nil lines after close")
}

func TestOutputBuffer_Subscribe(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ob := NewOutputBuffer(1024, 4096)

	// Add initial line
	ob.Append(1, OutputLine{Text: "initial", Type: "text"})

	// Subscribe
	initial, ch, cancel := ob.Subscribe(1)
	defer cancel()

	require.Len(initial, 1)
	assert.Equal("initial", initial[0].Text)

	// Add more lines after subscription
	go func() {
		ob.Append(1, OutputLine{Text: "new", Type: "text"})
	}()

	select {
	case line := <-ch:
		assert.Equal("new", line.Text)
	case <-time.After(100 * time.Millisecond):
		require.Condition(func() bool { return false }, "timeout waiting for subscribed line")
	}
}

func TestOutputBuffer_SubscribeCancel(t *testing.T) {
	assert := assert.New(t)

	ob := NewOutputBuffer(1024, 4096)

	_, ch, cancel := ob.Subscribe(1)
	cancel()

	// Channel should be closed
	select {
	case _, ok := <-ch:
		assert.False(ok, "expected channel to be closed")
	default:
		// Channel closed, as expected
	}
}

func TestOutputBuffer_CloseJobClosesSubscribers(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ob := NewOutputBuffer(1024, 4096)

	ob.Append(1, OutputLine{Text: "test", Type: "text"})
	_, ch, _ := ob.Subscribe(1)

	ob.CloseJob(1)

	// Channel should be closed
	select {
	case _, ok := <-ch:
		assert.False(ok, "expected channel to be closed after CloseJob")
	case <-time.After(100 * time.Millisecond):
		require.Condition(func() bool { return false }, "channel not closed after CloseJob")
	}
}

func TestOutputWriter_Write(t *testing.T) {
	ob := NewOutputBuffer(1024, 4096)
	w := ob.Writer(1, simpleNormalizer)

	// Write with newline
	w.Write([]byte("hello\n"))
	w.Write([]byte("world\n"))

	assertLines(t, ob.GetLines(1), OutputLine{Text: "hello", Type: "text"}, OutputLine{Text: "world", Type: "text"})
}

func TestOutputWriter_WritePartialLines(t *testing.T) {
	ob := NewOutputBuffer(1024, 4096)
	w := ob.Writer(1, simpleNormalizer)

	// Write partial line
	w.Write([]byte("hel"))
	w.Write([]byte("lo\nwor"))
	w.Write([]byte("ld\n"))

	assertLines(t, ob.GetLines(1), OutputLine{Text: "hello", Type: "text"}, OutputLine{Text: "world", Type: "text"})
}

func TestOutputWriter_Flush(t *testing.T) {
	ob := NewOutputBuffer(1024, 4096)
	w := ob.Writer(1, simpleNormalizer)

	// Write without newline
	w.Write([]byte("incomplete"))

	// Should not appear yet
	assertLines(t, ob.GetLines(1))

	// Flush should process remaining
	w.Flush()

	assertLines(t, ob.GetLines(1), OutputLine{Text: "incomplete", Type: "text"})
}

func TestOutputWriter_NormalizeFilters(t *testing.T) {
	require := require.New(t)

	ob := NewOutputBuffer(1024, 4096)
	// Normalizer that filters out empty lines
	normalize := func(line string) *OutputLine {
		if line == "" {
			return nil
		}
		return &OutputLine{Text: line, Type: "text"}
	}

	w := ob.Writer(1, normalize)

	w.Write([]byte("keep\n\nskip empty\n"))

	lines := ob.GetLines(1)
	require.Len(lines, 2, "expected 2 lines (empty filtered)")
}

func TestOutputWriterNormalizesCompleteLargeRecords(t *testing.T) {
	ob := NewOutputBuffer(50, 1000)
	_, updates, cancel := ob.Subscribe(1)
	defer cancel()
	content := strings.Repeat("x", 200)
	var normalized string
	w := ob.Writer(1, func(line string) *OutputLine {
		normalized = line
		return &OutputLine{Text: line, Type: "text"}
	})
	for range 4 {
		_, err := w.Write([]byte(strings.Repeat("x", 50)))
		require.NoError(t, err)
	}
	assert.Empty(t, normalized)
	_, err := w.Write([]byte("\n"))
	require.NoError(t, err)
	assert.Equal(t, content, normalized)
	require.Len(t, updates, 1)
	assert.Equal(t, content, (<-updates).Text)
}

func TestOutputBuffer_Concurrent(t *testing.T) {
	assert := assert.New(t)

	ob := NewOutputBuffer(10240, 40960)

	var wg sync.WaitGroup
	for i := range 10 {
		wg.Add(1)
		go func(jobID int64) {
			defer wg.Done()
			for range 100 {
				ob.Append(jobID, OutputLine{Text: "test", Type: "text"})
			}
		}(int64(i))
	}

	wg.Wait()

	// All jobs should have lines
	for i := range 10 {
		lines := ob.GetLines(int64(i))
		assert.NotEmpty(lines, "job %d has no lines", i)
	}
}
