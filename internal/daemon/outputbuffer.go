package daemon

import (
	"bytes"
	"strings"
	"sync"
	"time"
)

// OutputLine represents a single line of normalized output
type OutputLine struct {
	Timestamp time.Time `json:"ts"`
	Text      string    `json:"text"`
	Type      string    `json:"line_type"` // "text", "tool", "thinking", "error"
}

// JobOutput stores output for a single job
type JobOutput struct {
	mu         sync.RWMutex
	lines      []OutputLine
	totalBytes int
	startTime  time.Time
	closed     bool
	subs       []chan OutputLine // Subscribers for streaming
}

// OutputBuffer stores streaming output for running jobs with memory limits.
type OutputBuffer struct {
	mu         sync.RWMutex
	buffers    map[int64]*JobOutput
	maxPerJob  int // max bytes per job
	maxTotal   int // max total bytes across all jobs
	totalBytes int
}

// NewOutputBuffer creates a new output buffer with the given limits.
func NewOutputBuffer(maxPerJob, maxTotal int) *OutputBuffer {
	return &OutputBuffer{
		buffers:   make(map[int64]*JobOutput),
		maxPerJob: maxPerJob,
		maxTotal:  maxTotal,
	}
}

// getOrCreate returns the JobOutput for a job, creating if needed.
func (ob *OutputBuffer) getOrCreate(jobID int64) *JobOutput {
	ob.mu.Lock()
	defer ob.mu.Unlock()

	if jo, ok := ob.buffers[jobID]; ok {
		return jo
	}

	jo := &JobOutput{
		lines:     make([]OutputLine, 0, 100),
		startTime: time.Now(),
	}
	ob.buffers[jobID] = jo
	return jo
}

// Append adds a line to the job's output buffer.
func (ob *OutputBuffer) Append(jobID int64, line OutputLine) {
	jo := ob.getOrCreate(jobID)

	jo.mu.Lock()
	defer jo.mu.Unlock()

	if jo.closed {
		return
	}

	// Notify subscribers
	for _, ch := range jo.subs {
		select {
		case ch <- line:
		default:
			// Drop if subscriber is slow
		}
	}

	lineBytes := len(line.Text)

	// Drop oversized lines that exceed per-job limit on their own
	if lineBytes > ob.maxPerJob {
		return
	}

	// Calculate how many bytes would be evicted for per-job limit
	evictBytes := 0
	tempTotal := jo.totalBytes
	evictCount := 0
	for tempTotal+lineBytes > ob.maxPerJob && evictCount < len(jo.lines) {
		evictBytes += len(jo.lines[evictCount].Text)
		tempTotal -= len(jo.lines[evictCount].Text)
		evictCount++
	}

	// Check global memory limit BEFORE evicting - if we'd still exceed
	// maxTotal after eviction, reject without losing existing lines
	ob.mu.Lock()
	if ob.totalBytes-evictBytes+lineBytes > ob.maxTotal {
		ob.mu.Unlock()
		return // Drop line to enforce global memory limit
	}

	// Perform the eviction and add BEFORE updating global total
	// to keep ob.totalBytes in sync with actual memory usage
	if evictCount > 0 {
		jo.lines = jo.lines[evictCount:]
		jo.totalBytes -= evictBytes
	}

	// Add the line
	jo.lines = append(jo.lines, line)
	jo.totalBytes += lineBytes

	// Update global total after eviction/add - now reflects actual state
	ob.totalBytes = ob.totalBytes - evictBytes + lineBytes
	ob.mu.Unlock()
}

// GetLines returns all lines for a job.
func (ob *OutputBuffer) GetLines(jobID int64) []OutputLine {
	ob.mu.RLock()
	jo, ok := ob.buffers[jobID]
	ob.mu.RUnlock()

	if !ok {
		return nil
	}

	jo.mu.RLock()
	defer jo.mu.RUnlock()

	result := make([]OutputLine, len(jo.lines))
	copy(result, jo.lines)
	return result
}

// Subscribe returns existing lines and a channel for new lines.
// Call the returned cancel function when done.
func (ob *OutputBuffer) Subscribe(jobID int64) ([]OutputLine, <-chan OutputLine, func()) {
	jo := ob.getOrCreate(jobID)

	jo.mu.Lock()
	defer jo.mu.Unlock()

	// Copy existing lines
	initial := make([]OutputLine, len(jo.lines))
	copy(initial, jo.lines)

	// Create subscription channel
	ch := make(chan OutputLine, 100)
	jo.subs = append(jo.subs, ch)

	// Return cancel function
	cancel := func() {
		jo.mu.Lock()
		defer jo.mu.Unlock()
		for i, sub := range jo.subs {
			if sub == ch {
				jo.subs = append(jo.subs[:i], jo.subs[i+1:]...)
				close(ch)
				break
			}
		}
	}

	return initial, ch, cancel
}

// CloseJob marks a job as complete and removes its buffer.
func (ob *OutputBuffer) CloseJob(jobID int64) {
	ob.mu.Lock()
	jo, ok := ob.buffers[jobID]
	if !ok {
		ob.mu.Unlock()
		return
	}
	delete(ob.buffers, jobID)
	ob.mu.Unlock()

	jo.mu.Lock()
	defer jo.mu.Unlock()

	jo.closed = true
	ob.mu.Lock()
	ob.totalBytes -= jo.totalBytes
	ob.mu.Unlock()

	// Close all subscriber channels
	for _, ch := range jo.subs {
		close(ch)
	}
	jo.subs = nil
}

// IsActive returns true if there's an active buffer for this job.
func (ob *OutputBuffer) IsActive(jobID int64) bool {
	ob.mu.RLock()
	defer ob.mu.RUnlock()
	_, ok := ob.buffers[jobID]
	return ok
}

// OutputNormalizer converts agent-specific output to normalized OutputLines.
type OutputNormalizer func(line string) *OutputLine

// outputWriter implements io.Writer and normalizes output to the buffer.
type outputWriter struct {
	buffer    *OutputBuffer
	jobID     int64
	normalize OutputNormalizer
	lineBuf   bytes.Buffer
}

func (w *outputWriter) Write(p []byte) (n int, err error) {
	w.lineBuf.Write(p)

	// Process complete lines
	for {
		data := w.lineBuf.String()
		idx := strings.Index(data, "\n")

		if idx < 0 {
			break
		}
		// Extract line and update buffer
		line := data[:idx]
		w.lineBuf.Reset()
		if idx+1 < len(data) {
			w.lineBuf.WriteString(data[idx+1:])
		}
		line = strings.TrimSuffix(line, "\r")
		if normalized := w.normalize(line); normalized != nil {
			normalized.Timestamp = time.Now()
			w.buffer.Append(w.jobID, *normalized)
		}
	}
	return len(p), nil
}

// Flush processes any remaining buffered content.
func (w *outputWriter) Flush() {
	if w.lineBuf.Len() > 0 {
		line := w.lineBuf.String()
		w.lineBuf.Reset()
		if normalized := w.normalize(line); normalized != nil {
			normalized.Timestamp = time.Now()
			w.buffer.Append(w.jobID, *normalized)
		}
	}
}

// Writer returns an io.Writer that normalizes and stores output for a job.
// Complete records are normalized before applying the history retention policy.
func (ob *OutputBuffer) Writer(jobID int64, normalize OutputNormalizer) *outputWriter {
	return &outputWriter{
		buffer:    ob,
		jobID:     jobID,
		normalize: normalize,
	}
}
