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

	"go.kenn.io/roborev/internal/storage"
)

// AppOpenedLimiter limits daily app opens by surface and screen views by screen; screens require Database.
type AppOpenedLimiter struct {
	Database *storage.DB
	mu       sync.Mutex
	sent     map[string]string // surface or screen metadata key -> last accepted UTC date
	now      func() time.Time  // nil means time.Now; tests set it
}

// appOpenedMaxBodyBytes matches kit's maxPostHogCaptureBodyBytes at the pinned version; a larger body goes to kit for its 413.
const appOpenedMaxBodyBytes = 64 << 10

// Handler accepts app_opened, session_ended, and screen_viewed. It limits app_opened and screen_viewed to one send per UTC day.
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
		var finish func(bool)
		var canonical []byte
		if len(body) <= appOpenedMaxBodyBytes {
			var event struct {
				Event string `json:"event"`
			}
			if json.Unmarshal(body, &event) == nil {
				switch strings.TrimSpace(event.Event) {
				case EventAppOpened, EventSessionEnded, EventScreenViewed:
				default:
					http.Error(w, ErrUnsupportedEvent.Error(), http.StatusBadRequest)
					return
				}
			}
			skip, finish, canonical = l.alreadySentToday(reporter, body)
		}
		if skip {
			// Same answer kit gives a queued capture, so clients cannot tell a deduped open from a sent one.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, "{\"status\":\"queued\"}\n")
			return
		}
		if finish != nil {
			r.Body = readCloser{Reader: bytes.NewReader(canonical), Closer: r.Body}
			r.ContentLength = int64(len(canonical))
			response := &captureResponse{ResponseWriter: w}
			defer func() { finish(response.status == http.StatusAccepted) }()
			capture.ServeHTTP(response, r)
			return
		}
		capture.ServeHTTP(w, r)
	})
}

type readCloser struct {
	io.Reader
	io.Closer
}

// alreadySentToday claims the request's daily key; screen claims finish after kit answers.
func (l *AppOpenedLimiter) alreadySentToday(reporter *Reporter, body []byte) (bool, func(bool), []byte) {
	var req struct {
		Event      string         `json:"event"`
		Properties map[string]any `json:"properties"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&req); err != nil {
		return false, nil, nil
	}
	// Kit rejects data after the JSON object; leave that answer to kit.
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return false, nil, nil
	}
	req.Event = strings.TrimSpace(req.Event)
	if (req.Event != EventAppOpened && req.Event != EventScreenViewed) || !reporter.Enabled() {
		return false, nil, nil
	}
	props, err := reporter.SanitizeProperties(req.Event, req.Properties)
	if err != nil {
		return false, nil, nil
	}
	surface, _ := props[PropertySurface].(string)
	clock := l.now
	if clock == nil {
		clock = time.Now
	}
	day := clock().UTC().Format(time.DateOnly)
	if req.Event == EventScreenViewed {
		screen, _ := props[PropertyScreen].(string)
		if screen == "" {
			return true, nil, nil
		}
		req.Properties = props
		canonical, err := json.Marshal(req)
		if err != nil {
			return true, nil, nil
		}
		skip, finish := l.claimScreen(screen, day)
		return skip, finish, canonical
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.sent[surface] == day {
		return true, nil, nil
	}
	if l.sent == nil {
		l.sent = make(map[string]string)
	}
	l.sent[surface] = day
	return false, nil, nil
}

// PostAppOpened posts app_opened for surface to a daemon capture URL and discards the response; callers gate on EnabledFromEnv.
func PostAppOpened(ctx context.Context, client *http.Client, url, surface string) {
	postEvent(ctx, client, url, EventAppOpened, map[string]string{PropertySurface: surface})
}

// PostSessionEnded reports one completed interface lifetime.
func PostSessionEnded(ctx context.Context, client *http.Client, url, surface string, elapsed time.Duration) {
	postEvent(ctx, client, url, EventSessionEnded, map[string]string{PropertySurface: surface, PropertyDurationBucket: DurationBucket(elapsed)})
}

func PostScreenViewed(ctx context.Context, client *http.Client, url, screen, surface string) {
	postEvent(ctx, client, url, EventScreenViewed, map[string]string{PropertyScreen: screen, PropertySurface: surface})
}

func postEvent(ctx context.Context, client *http.Client, url, event string, properties map[string]string) {
	body, err := json.Marshal(map[string]any{
		"event":      event,
		"properties": properties,
	})
	if err != nil {
		return
	}
	postTelemetry(ctx, client, url, body)
}

// PostAgentCall notifies the daemon of one call; callers gate on EnabledFromEnv.
func PostAgentCall(ctx context.Context, client *http.Client, url string) {
	postTelemetry(ctx, client, url, nil)
}

func postTelemetry(ctx context.Context, client *http.Client, url string, body []byte) {
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

// claimScreen reserves in memory and persists only accepted captures.
func (l *AppOpenedLimiter) claimScreen(screen, day string) (bool, func(bool)) {
	l.mu.Lock()
	key := "telemetry.screen." + screen
	if l.Database == nil || l.sent[key] >= day {
		l.mu.Unlock()
		return true, nil
	}
	if l.sent == nil {
		l.sent = make(map[string]string)
	}
	l.sent[key] = day
	l.mu.Unlock()
	release := func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.sent[key] == day {
			delete(l.sent, key)
		}
	}
	stored, err := l.Database.GetSyncState(key)
	if err != nil {
		release()
		return true, nil
	}
	if stored >= day {
		return true, nil
	}
	return false, func(accepted bool) {
		if !accepted {
			release()
			return
		}
		_ = l.Database.AdvanceSyncState(key, day)
	}
}

type captureResponse struct {
	http.ResponseWriter
	status int
}

func (w *captureResponse) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}
