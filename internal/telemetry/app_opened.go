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

	"go.kenn.io/kit/telemetry/posthog"
)

// AppOpenedLimiter limits daily app opens.
type AppOpenedLimiter struct {
	mu   sync.Mutex
	sent map[string]string // surface -> last accepted UTC date
	now  func() time.Time  // nil means time.Now; tests set it
}

// appOpenedMaxBodyBytes matches kit's maxPostHogCaptureBodyBytes at the pinned version; a larger body goes to kit for its 413.
const appOpenedMaxBodyBytes = 64 << 10

// Handler accepts usage events. It limits app_opened to one send per surface per UTC day.
// Build it per request if needed; the state lives on the limiter.
func (l *AppOpenedLimiter) Handler(reporter *Reporter) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capture := posthog.NewCaptureHandler(reporter)
		mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if r.Method != http.MethodPost || err != nil || mediaType != "application/json" {
			capture.ServeHTTP(w, r)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, appOpenedMaxBodyBytes+1))
		if err != nil {
			http.Error(w, "invalid telemetry request", http.StatusBadRequest)
			return
		}
		// Hand kit the bytes already read plus the rest, so its size and decode answers stay its own.
		r.Body = readCloser{Reader: io.MultiReader(bytes.NewReader(body), r.Body), Closer: r.Body}
		var skip bool
		if len(body) <= appOpenedMaxBodyBytes {
			var event struct {
				Event string `json:"event"`
			}
			if json.Unmarshal(body, &event) == nil {
				switch strings.TrimSpace(event.Event) {
				case EventAppOpened:
					skip = l.alreadySentToday(reporter, body)
				case EventScreenViewed, EventSessionEnded, EventReviewViewed, EventFindingFixed, EventReviewClosed:
				default:
					http.Error(w, ErrUnsupportedEvent.Error(), http.StatusBadRequest)
					return
				}
			}
		}
		if skip {
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
	req.Event = strings.TrimSpace(req.Event)
	if !reporter.Enabled() {
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

// PostAppOpened runs the day's app_opened check for surface through the client gate; callers gate on EnabledFromEnv.
func PostAppOpened(ctx context.Context, client *http.Client, scope, url, surface string) {
	DefaultClientGate.Probe(ctx, client, scope, url, surface)
}

// PostSessionEnded reports one completed interface lifetime.
func PostSessionEnded(ctx context.Context, client *http.Client, scope, url, surface string, elapsed time.Duration) {
	_, _ = DefaultClientGate.Post(ctx, client, scope, url, EventSessionEnded, map[string]any{PropertySurface: surface, PropertyDurationBucket: DurationBucket(elapsed)})
}

func PostScreenViewed(ctx context.Context, client *http.Client, scope, url, screen, surface string) {
	_, _ = DefaultClientGate.Post(ctx, client, scope, url, EventScreenViewed, map[string]any{PropertyScreen: screen, PropertySurface: surface})
}

// PostReviewEvent reports a completed review action through the daemon.
func PostReviewEvent(ctx context.Context, client *http.Client, scope, url, event, surface string) {
	_, _ = DefaultClientGate.Post(ctx, client, scope, url, event, map[string]any{PropertySurface: surface})
}

// PostAgentCall notifies the daemon of one call; callers gate on EnabledFromEnv.
func PostAgentCall(ctx context.Context, client *http.Client, scope, url string) {
	_, _ = DefaultClientGate.Post(ctx, client, scope, url, EventAgentActive, nil)
}
