package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync"
	"time"
)

// AppOpenedLimiter forwards at most one app_opened per surface per UTC day; the zero value is ready to use.
type AppOpenedLimiter struct {
	mu   sync.Mutex
	sent map[string]string // sanitized surface ("" when absent) -> UTC date of the last forwarded event
	now  func() time.Time  // nil means time.Now; tests set it
}

// appOpenedMaxBodyBytes matches kit's maxPostHogCaptureBodyBytes at the pinned version; a larger body goes to kit for its 413.
const appOpenedMaxBodyBytes = 64 << 10

// Handler wraps NewCaptureHandler(reporter); build it per request if needed, the state lives on the limiter.
func (l *AppOpenedLimiter) Handler(reporter *Reporter) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capture := NewCaptureHandler(reporter)
		mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if r.Method != http.MethodPost || err != nil || mediaType != "application/json" {
			capture.ServeHTTP(w, r)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, appOpenedMaxBodyBytes+1))
		// Hand kit the bytes already read plus the rest, so its size and decode answers stay its own.
		r.Body = readCloser{Reader: io.MultiReader(bytes.NewReader(body), r.Body), Closer: r.Body}
		if err == nil && len(body) <= appOpenedMaxBodyBytes && l.alreadySentToday(reporter, body) {
			// Same answer kit gives a queued capture, so clients cannot tell a deduped open from a sent one.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, "{\"status\":\"queued\"}\n")
			return
		}
		capture.ServeHTTP(w, r)
	})
}

type readCloser struct {
	io.Reader
	io.Closer
}

// alreadySentToday claims today for the request's surface when the reporter would send it, and reports whether today was already claimed.
func (l *AppOpenedLimiter) alreadySentToday(reporter *Reporter, body []byte) bool {
	var req struct {
		Event      string         `json:"event"`
		Properties map[string]any `json:"properties"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&req); err != nil {
		return false
	}
	// Kit rejects data after the JSON object; leave that answer to kit.
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return false
	}
	if strings.TrimSpace(req.Event) != EventAppOpened || !reporter.Enabled() {
		return false
	}
	props, err := reporter.SanitizeProperties(req.Event, req.Properties)
	if err != nil {
		return false
	}
	surface, _ := props[PropertySurface].(string)
	clock := l.now
	if clock == nil {
		clock = time.Now
	}
	day := clock().UTC().Format(time.DateOnly)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.sent[surface] == day {
		return true
	}
	if l.sent == nil {
		l.sent = make(map[string]string)
	}
	l.sent[surface] = day
	return false
}

// PostAppOpened posts app_opened for surface to a daemon capture URL and discards the response; callers gate on EnabledFromEnv.
func PostAppOpened(ctx context.Context, client *http.Client, url, surface string) {
	body, err := json.Marshal(map[string]any{
		"event":      EventAppOpened,
		"properties": map[string]string{PropertySurface: surface},
	})
	if err != nil {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	_ = resp.Body.Close()
}
