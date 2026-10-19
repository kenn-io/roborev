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

// AppOpenedLimiter limits daily app opens.
type AppOpenedLimiter struct {
	Database *storage.DB
	mu       sync.Mutex
	sent     map[string]string // surface -> last accepted UTC date
	now      func() time.Time  // nil means time.Now; tests set it
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
		var canonical []byte
		if len(body) <= appOpenedMaxBodyBytes {
			var event struct {
				Event string `json:"event"`
			}
			if json.Unmarshal(body, &event) == nil {
				switch strings.TrimSpace(event.Event) {
				case EventAppOpened, EventScreenViewed:
					skip, canonical, err = l.alreadySentToday(reporter, body)
					if err != nil {
						http.Error(w, "read telemetry claim", http.StatusInternalServerError)
						return
					}
				case EventSessionEnded, EventReviewViewed, EventFindingFixed, EventReviewClosed, EventErrorShown:
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
		if canonical != nil {
			r.Body = readCloser{Reader: bytes.NewReader(canonical), Closer: r.Body}
			r.ContentLength = int64(len(canonical))
		}
		capture.ServeHTTP(w, r)
	})
}

type readCloser struct {
	io.Reader
	io.Closer
}

func (l *AppOpenedLimiter) alreadySentToday(reporter *Reporter, body []byte) (bool, []byte, error) {
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
	if !reporter.Enabled() {
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
		req.Properties = props
		canonical, err := json.Marshal(req)
		if err != nil {
			return false, nil, err
		}
		if l.Database == nil {
			return false, canonical, nil
		}
		screen, _ := props[PropertyScreen].(string)
		claimed, err := l.Database.GetSyncState("telemetry.screen." + screen)
		return claimed >= day, canonical, err
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
	_, _ = posthog.PostEvent(ctx, client, url, EventAppOpened, map[string]any{PropertySurface: surface})
}

// PostSessionEnded reports one completed interface lifetime.
func PostSessionEnded(ctx context.Context, client *http.Client, url, surface string, elapsed time.Duration) {
	_, _ = posthog.PostEvent(ctx, client, url, EventSessionEnded, map[string]any{PropertySurface: surface, PropertyDurationBucket: DurationBucket(elapsed)})
}

func PostScreenViewed(ctx context.Context, client *http.Client, url, screen, surface string) error {
	_, err := posthog.PostEvent(ctx, client, url, EventScreenViewed, map[string]any{PropertyScreen: screen, PropertySurface: surface})
	return err
}

// PostReviewEvent reports a completed review action through the daemon.
func PostReviewEvent(ctx context.Context, client *http.Client, url, event, surface string) (posthog.Status, error) {
	return posthog.PostEvent(ctx, client, url, event, map[string]any{PropertySurface: surface})
}

// PostErrorShown reports a core action failure displayed to the user.
func PostErrorShown(ctx context.Context, client *http.Client, url, surface, kind string) (posthog.Status, error) {
	return posthog.PostEvent(ctx, client, url, EventErrorShown, map[string]any{PropertySurface: surface, PropertyKind: kind})
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
