package agent

import (
	"bytes"
	"io"
	"sync"
)

// SessionCaptureWriter is a transparent passthrough writer that scans
// the byte stream for the first agent session ID JSON event
// (session_id / sessionId / sessionID / type:session / thread.started)
// and surfaces it via SessionID(). After capture, scanning stops and
// the writer continues to forward bytes unchanged.
//
// Use Flush() once writes are complete so any trailing partial line
// is also examined.
type SessionCaptureWriter struct {
	dst       io.Writer
	onCapture func(string)
	onReset   func() error
	mu        sync.Mutex
	buf       bytes.Buffer
	sessionID string
}

// NewSessionCaptureWriter wraps dst and reports the first captured
// session ID via onCapture (if non-nil). Bytes pass through to dst
// unchanged.
func NewSessionCaptureWriter(dst io.Writer, onCapture func(string), onReset ...func() error) *SessionCaptureWriter {
	w := &SessionCaptureWriter{dst: dst, onCapture: onCapture}
	if len(onReset) > 0 {
		w.onReset = onReset[0]
	}
	return w
}

func (w *SessionCaptureWriter) Write(p []byte) (int, error) {
	n, err := w.dst.Write(p)
	if n > 0 {
		w.capture(p[:n])
	}
	return n, err
}

// Flush examines any buffered partial line. Call this before reading
// SessionID() to ensure capture is complete.
func (w *SessionCaptureWriter) Flush() {
	var captured string

	w.capture(nil)

	w.mu.Lock()
	if w.sessionID == "" && w.buf.Len() > 0 {
		if sessionID := ExtractSessionID(w.buf.String()); sessionID != "" {
			w.sessionID = sessionID
			captured = sessionID
		}
	}
	w.mu.Unlock()

	if captured != "" && w.onCapture != nil {
		w.onCapture(captured)
	}
}

// SessionID returns the first captured session ID, or "" if none.
func (w *SessionCaptureWriter) SessionID() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.sessionID
}

// ResetSessionID clears an abandoned attempt before a fresh one captures its ID.
func (w *SessionCaptureWriter) ResetSessionID() error {
	if w.onReset != nil {
		if err := w.onReset(); err != nil {
			return err
		}
	}
	w.mu.Lock()
	w.sessionID = ""
	w.buf.Reset()
	w.mu.Unlock()
	return nil
}

func (w *SessionCaptureWriter) capture(p []byte) {
	var captured string

	w.mu.Lock()
	if w.sessionID != "" {
		w.mu.Unlock()
		return
	}
	for len(p) > 0 && w.sessionID == "" {
		idx := bytes.IndexByte(p, '\n')
		if idx < 0 {
			_, _ = w.buf.Write(p)
			break
		}
		_, _ = w.buf.Write(p[:idx+1])
		line := w.buf.String()
		w.buf.Reset()
		p = p[idx+1:]
		if sessionID := ExtractSessionID(line); sessionID != "" {
			w.sessionID = sessionID
			captured = sessionID
		}
	}
	w.mu.Unlock()

	if captured != "" && w.onCapture != nil {
		w.onCapture(captured)
	}
}
