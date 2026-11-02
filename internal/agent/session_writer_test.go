package agent

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSessionCaptureWriter_LongLineChunkingCost(t *testing.T) {
	require := require.New(t)
	payload := bytes.Repeat([]byte("x"), 16<<20)
	var elapsed [2]time.Duration
	for i, chunkSize := range []int{4096, 65536} {
		w := NewSessionCaptureWriter(io.Discard, nil)
		w.buf.Grow(len(payload))
		for trial := range 3 {
			w.buf.Reset()
			start := time.Now()
			for offset := 0; offset < len(payload); offset += chunkSize {
				_, _ = w.Write(payload[offset : offset+chunkSize])
			}
			duration := time.Since(start)
			if trial == 0 || duration < elapsed[i] {
				elapsed[i] = duration
			}
		}
		require.Equal(payload, w.buf.Bytes())
	}
	// Both cases copy the same bytes. Rescanning the unfinished line costs
	// about 16 times as much with 16 times as many chunks. Allow an eightfold
	// difference for lock overhead and timing noise, using the fastest of
	// three trials to reduce interference. Preallocation isolates scanning.
	require.Less(elapsed[0], 8*elapsed[1], "4KiB and 64KiB chunks: %v", elapsed)
}

func TestSessionCaptureWriter_Chunking(t *testing.T) {
	const payload = "plain text\r\n\n{\"session_id\":\"session-123\"}\r\n{\"session_id\":\"session-456\"}\ntrailing"
	for chunkSize := 1; chunkSize <= len(payload); chunkSize++ {
		t.Run(fmt.Sprintf("chunk=%d", chunkSize), func(t *testing.T) {
			require := require.New(t)
			var dst bytes.Buffer
			var captured []string
			w := NewSessionCaptureWriter(&dst, func(sessionID string) {
				captured = append(captured, sessionID)
			})
			for offset := 0; offset < len(payload); offset += chunkSize {
				chunk := []byte(payload[offset:min(offset+chunkSize, len(payload))])
				n, err := w.Write(chunk)
				require.NoError(err)
				require.Equal(len(chunk), n)
			}
			w.Flush()
			require.Equal("session-123", w.SessionID())
			require.Equal([]string{"session-123"}, captured)
			require.Equal(payload, dst.String())
		})
	}
}

func TestSessionCaptureWriter_StopsBufferingAfterCapture(t *testing.T) {
	var dst bytes.Buffer
	var captured []string

	w := NewSessionCaptureWriter(&dst, func(sessionID string) {
		captured = append(captured, sessionID)
	})

	first := `{"type":"system","subtype":"init","session_id":"session-123"}` + "\n"
	_, err := w.Write([]byte(first))
	require.NoError(t, err)
	require.Equal(t, "session-123", w.SessionID())
	require.Equal(t, []string{"session-123"}, captured)
	require.Equal(t, 0, w.buf.Len())

	trailing := strings.Repeat("later output\n", 128)
	_, err = w.Write([]byte(trailing))
	require.NoError(t, err)
	require.Len(t, captured, 1, "callback fires only once")
	require.Equal(t, 0, w.buf.Len(), "buffer is not retained after capture")
	require.Equal(t, first+trailing, dst.String(), "destination receives full stream")
}

func TestSessionCaptureWriter_FlushCapturesPartialLine(t *testing.T) {
	var dst bytes.Buffer
	var captured string

	w := NewSessionCaptureWriter(&dst, func(sessionID string) {
		captured = sessionID
	})

	line := `{"type":"thread.started","thread_id":"thread-456"}`
	_, err := w.Write([]byte(line))
	require.NoError(t, err)
	require.Empty(t, w.SessionID(), "no newline yet, no capture")

	w.Flush()

	require.Equal(t, "thread-456", w.SessionID())
	require.Equal(t, "thread-456", captured)
}

func TestSessionCaptureWriter_NoSessionEvent(t *testing.T) {
	var dst bytes.Buffer
	w := NewSessionCaptureWriter(&dst, nil)

	payload := "plain text output\nwith multiple lines\nbut no session\n"
	_, err := w.Write([]byte(payload))
	require.NoError(t, err)
	w.Flush()

	require.Empty(t, w.SessionID())
	require.Equal(t, payload, dst.String())
}
