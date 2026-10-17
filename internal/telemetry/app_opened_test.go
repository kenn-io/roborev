package telemetry

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/testutil"
)

// postThroughLimiter builds the handler per request, as the daemon route does, so only the limiter carries state.
func postThroughLimiter(limiter *AppOpenedLimiter, reporter *Reporter, body string) *httptest.ResponseRecorder {
	return postCaptureEvent(limiter.Handler(reporter), body)
}

func surfacesOf(messages []testutil.PostHogMessage) []string {
	surfaces := make([]string, 0, len(messages))
	for _, message := range messages {
		surface, _ := message.Properties[PropertySurface].(string)
		surfaces = append(surfaces, message.Event+":"+surface)
	}
	return surfaces
}

func TestAppOpenedLimiterSendsOncePerSurfacePerDay(t *testing.T) {
	t.Setenv(EnabledEnv, "1")
	t.Setenv(GenericEnabledEnv, "1")

	t.Run("same UTC day", func(t *testing.T) {
		assert := assert.New(t)
		reporter, messages := newPostHogStubReporter(t)
		// 23:59 UTC: the last minute of the day still shares the day's claims.
		limiter := &AppOpenedLimiter{now: func() time.Time { return time.Date(2026, 3, 1, 23, 59, 0, 0, time.UTC) }}

		for _, body := range []string{
			`{"event":"app_opened","properties":{"surface":"cli"}}`,
			`{"event":"app_opened","properties":{"surface":"cli"}}`,
			`{"event":"app_opened","properties":{"surface":"tui","source":"web"}}`,
			`{"event":"app_opened","properties":{"surface":"web"}}`,
			`{"event":"app_opened","properties":{"surface":"tui"}}`,
			"{\"event\":\"app_opened\",\"properties\":{\"surface\":\"cli\"}}  \n\t",
			`{"event":"app_opened","properties":{"surface":["tui"]}}`,
			`{"event":"app_opened"}`,
		} {
			recorder := postThroughLimiter(limiter, reporter, body)
			assert.Equal(http.StatusAccepted, recorder.Code, body)
			assert.JSONEq(`{"status":"queued"}`, recorder.Body.String(), body)
		}
		require.NoError(t, reporter.Close())
		sent := messages()
		assert.Equal([]string{"app_opened:cli", "app_opened:tui", "app_opened:web", "app_opened:"}, surfacesOf(sent))
		require.Len(t, sent, 4)
		assert.NotEmpty(sent[0].DistinctID)
		for _, message := range sent {
			assert.Equal(sent[0].DistinctID, message.DistinctID)
			assert.Equal("roborev", message.Properties["application"])
			assert.Equal("daemon", message.Properties["source"])
		}
		assert.NotContains(sent[3].Properties, PropertySurface)
	})

	t.Run("next UTC day", func(t *testing.T) {
		assert := assert.New(t)
		reporter, messages := newPostHogStubReporter(t)
		now := time.Date(2026, 3, 1, 23, 59, 0, 0, time.UTC)
		limiter := &AppOpenedLimiter{now: func() time.Time { return now }}
		body := `{"event":"app_opened","properties":{"surface":"cli"}}`

		assert.Equal(http.StatusAccepted, postThroughLimiter(limiter, reporter, body).Code)
		assert.Equal(http.StatusAccepted, postThroughLimiter(limiter, reporter, body).Code)
		// 00:00 UTC on the next day starts a new claim.
		now = time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)
		assert.Equal(http.StatusAccepted, postThroughLimiter(limiter, reporter, body).Code)
		require.NoError(t, reporter.Close())
		assert.Equal([]string{"app_opened:cli", "app_opened:cli"}, surfacesOf(messages()))
	})
}

func TestSessionEndedBypassesDailyLimiter(t *testing.T) {
	t.Setenv(EnabledEnv, "1")
	t.Setenv(GenericEnabledEnv, "1")
	reporter, messages := newPostHogStubReporter(t)
	limiter := &AppOpenedLimiter{}
	for range 2 {
		rec := postThroughLimiter(limiter, reporter, `{"event":"session_ended","properties":{"surface":"web","duration_bucket":"1_to_5m"}}`)
		assert.Equal(t, http.StatusAccepted, rec.Code)
	}
	require.NoError(t, reporter.Close())
	assert.Equal(t, []string{"session_ended:web", "session_ended:web"}, surfacesOf(messages()))
}

func TestAppOpenedLimiterIgnoresRequestsThatSendNothing(t *testing.T) {
	assert := assert.New(t)
	limiter := &AppOpenedLimiter{}
	cli := `{"event":"app_opened","properties":{"surface":"cli"}}`

	t.Setenv(EnabledEnv, "0")
	optedOut, err := NewReporter(Options{})
	require.NoError(t, err)
	recorder := postThroughLimiter(limiter, optedOut, cli)
	assert.Equal(http.StatusAccepted, recorder.Code)
	assert.JSONEq(`{"status":"disabled"}`, recorder.Body.String())

	for _, body := range []string{
		`{"event":"App_Opened"}`,
		`{"event":""}`,
		`{"event":"agent_active"}`,
		`{"event":"setup_finished"}`,
	} {
		recorder := postThroughLimiter(limiter, optedOut, body)
		assert.Equal(http.StatusBadRequest, recorder.Code)
		assert.Contains(recorder.Body.String(), ErrUnsupportedEvent.Error())
	}

	t.Setenv(EnabledEnv, "1")
	t.Setenv(GenericEnabledEnv, "1")
	reporter, messages := newPostHogStubReporter(t)
	agentBody := `{"event":"agent_active","properties":{"call_count_bucket":"1-10"}}`
	broken, err := http.ReadRequest(bufio.NewReader(strings.NewReader(fmt.Sprintf("POST /api/telemetry/events HTTP/1.1\r\nHost: localhost\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(agentBody)+1, agentBody))))
	require.NoError(t, err)
	brokenRecorder := httptest.NewRecorder()
	limiter.Handler(reporter).ServeHTTP(brokenRecorder, broken)
	assert.Equal(http.StatusBadRequest, brokenRecorder.Code)
	assert.Equal(http.StatusBadRequest, postThroughLimiter(limiter, reporter, `{"event":`).Code)
	assert.Equal(http.StatusBadRequest, postThroughLimiter(limiter, reporter, cli+`{"event":"app_opened"}`).Code)
	oversized := `{"event":"app_opened","properties":{"surface":"cli"}}` + strings.Repeat(" ", appOpenedMaxBodyBytes)
	assert.Equal(http.StatusRequestEntityTooLarge, postThroughLimiter(limiter, reporter, oversized).Code)
	plain := httptest.NewRequest(http.MethodPost, "/api/telemetry/events", strings.NewReader(cli))
	plain.Header.Set("Content-Type", "text/plain")
	plainRecorder := httptest.NewRecorder()
	limiter.Handler(reporter).ServeHTTP(plainRecorder, plain)
	assert.Equal(http.StatusUnsupportedMediaType, plainRecorder.Code)
	recorder = postThroughLimiter(limiter, reporter, cli)
	assert.Equal(http.StatusAccepted, recorder.Code)
	assert.JSONEq(`{"status":"queued"}`, recorder.Body.String())
	require.NoError(t, reporter.Close())

	assert.Equal([]string{"app_opened:cli"}, surfacesOf(messages()))
}

func TestAppOpenedLimiterConcurrentFirstRequestsSendOnce(t *testing.T) {
	t.Setenv(EnabledEnv, "1")
	t.Setenv(GenericEnabledEnv, "1")
	body := `{"event":"app_opened","properties":{"surface":"cli"}}`
	reporter, messages := newPostHogStubReporter(t)
	limiter := &AppOpenedLimiter{}
	var wg sync.WaitGroup
	codes := make([]int, 8)
	for i := range codes {
		wg.Go(func() { codes[i] = postThroughLimiter(limiter, reporter, body).Code })
	}
	wg.Wait()
	require.NoError(t, reporter.Close())
	for _, code := range codes {
		assert.Equal(t, http.StatusAccepted, code)
	}
	assert.Len(t, messages(), 1)
}

func TestPostAppOpened(t *testing.T) {
	t.Run("posts the app_opened body", func(t *testing.T) {
		assert := assert.New(t)
		var method, path, contentType, body string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			method, path, contentType, body = r.Method, r.URL.Path, r.Header.Get("Content-Type"), string(raw)
			w.WriteHeader(http.StatusAccepted)
		}))
		t.Cleanup(srv.Close)

		PostAppOpened(context.Background(), srv.Client(), srv.URL+"/api/telemetry/events", SurfaceCLI)
		assert.Equal(http.MethodPost, method)
		assert.Equal("/api/telemetry/events", path)
		assert.Equal("application/json", contentType)
		assert.JSONEq(`{"event":"app_opened","properties":{"surface":"cli"}}`, body)
	})

	t.Run("ignores a 404 and a closed server", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		t.Cleanup(srv.Close)
		assert.NotPanics(t, func() {
			PostAppOpened(context.Background(), srv.Client(), srv.URL+"/api/telemetry/events", SurfaceCLI)
		})

		ln, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		closedURL := "http://" + ln.Addr().String() + "/api/telemetry/events"
		require.NoError(t, ln.Close())
		assert.NotPanics(t, func() {
			PostAppOpened(context.Background(), http.DefaultClient, closedURL, SurfaceCLI)
		})
	})

	t.Run("cancelled context returns without a request", func(t *testing.T) {
		var hits int
		var mu sync.Mutex
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			mu.Lock()
			hits++
			mu.Unlock()
		}))
		t.Cleanup(srv.Close)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		PostAppOpened(ctx, srv.Client(), srv.URL+"/api/telemetry/events", SurfaceCLI)
		mu.Lock()
		defer mu.Unlock()
		assert.Zero(t, hits)
	})
}

func TestScreenViewedDailyClaims(t *testing.T) {
	t.Setenv(EnabledEnv, "1")
	t.Setenv(GenericEnabledEnv, "1")
	for _, tc := range []struct {
		offset  int
		aliases bool
		recover bool
	}{{-1, false, false}, {0, false, false}, {1, false, false}, {0, true, false}, {-1, false, true}} {
		t.Run(fmt.Sprint(tc), func(t *testing.T) {
			db := testutil.OpenTestDB(t)
			now := time.Now().UTC()
			day := now.AddDate(0, 0, tc.offset).Format(time.DateOnly)
			require.NoError(t, db.SetSyncState("telemetry.screen.queue", day))
			endpoint, messages := testutil.NewPostHogStub(t)
			opts := Options{Database: db, Endpoint: endpoint, DailyClaimsPath: filepath.Join(t.TempDir(), "daily.json")}
			if tc.recover {
				require.NoError(t, os.Mkdir(opts.DailyClaimsPath, 0o700))
			}
			body := `{"event":"screen_viewed","properties":{"screen":"queue","surface":"tui"}}`
			attempts := 1
			if tc.aliases {
				body = `{"event":"screen_viewed","properties":{"screen":"queue"," screen ":"review","surface":"web"," surface ":"tui","path":"/private"}}`
				attempts = 64
			}
			var installID string
			for pass := range 2 {
				reporter, err := NewReporter(opts)
				require.NoError(t, err)
				limiter := &AppOpenedLimiter{Database: db, now: func() time.Time { return now }}
				if tc.recover {
					if pass == 0 {
						installID, err = db.GetSyncState(installIDMetadataKey)
						require.NoError(t, err)
						assert.Equal(t, http.StatusInternalServerError, postThroughLimiter(limiter, reporter, body).Code)
						require.NoError(t, reporter.Close())
						require.NoError(t, os.Remove(opts.DailyClaimsPath))
						continue
					}
					_, err = db.Exec("ALTER TABLE sync_state RENAME TO unavailable_sync_state")
					require.NoError(t, err)
					assert.Equal(t, http.StatusInternalServerError, postThroughLimiter(limiter, reporter, body).Code)
					_, err = db.Exec("ALTER TABLE unavailable_sync_state RENAME TO sync_state")
					require.NoError(t, err)
				}
				for range attempts {
					if tc.aliases {
						skip, canonical, err := limiter.alreadySentToday(reporter, []byte(body))
						require.NoError(t, err)
						var normalized struct {
							Properties map[string]any `json:"properties"`
						}
						require.NoError(t, json.Unmarshal(canonical, &normalized))
						assert.Equal(t, normalized.Properties[PropertyScreen] == "queue", skip)
						assert.NotContains(t, normalized.Properties, " screen ")
						assert.NotContains(t, normalized.Properties, " surface ")
					}
					assert.Equal(t, http.StatusAccepted, postThroughLimiter(limiter, reporter, body).Code)
				}
				require.NoError(t, reporter.Close())
			}
			if tc.aliases {
				for _, message := range messages() {
					assert.Equal(t, "review", message.Properties[PropertyScreen])
				}
			} else if tc.offset < 0 {
				require.Len(t, messages(), 1)
				if tc.recover {
					assert.Equal(t, installID, messages()[0].DistinctID)
				}
			} else {
				assert.Empty(t, messages())
			}
			stored, err := db.GetSyncState("telemetry.screen.queue")
			require.NoError(t, err)
			assert.Equal(t, day, stored)
		})
	}
}

func TestCoreActionCapture(t *testing.T) {
	t.Setenv(EnabledEnv, "1")
	t.Setenv(GenericEnabledEnv, "1")
	reporter, messages := newPostHogStubReporter(t)
	handler := (&AppOpenedLimiter{}).Handler(reporter)
	for _, event := range []string{"review_viewed", "finding_fixed", "review_closed"} {
		for _, surface := range []string{"web", "tui", "cli"} {
			body := `{"event":"` + event + `","properties":{"surface":"` + surface + `","job_id":42,"output":"private review"}}`
			for range 2 {
				status := http.StatusAccepted
				if event == "finding_fixed" && surface == "web" {
					status = http.StatusBadRequest
				}
				assert.Equal(t, status, postCaptureEvent(handler, body).Code)
			}
		}
	}
	for _, properties := range []string{`{}`, `{"surface":"unknown"}`, `{"surface":42}`} {
		for _, event := range []string{"review_viewed", "finding_fixed", "review_closed"} {
			assert.Equal(t, http.StatusBadRequest, postCaptureEvent(handler, `{"event":"`+event+`","properties":`+properties+`}`).Code)
		}
	}
	assert.Equal(t, http.StatusBadRequest, postCaptureEvent(handler, `{"event":"unknown_action"}`).Code)
	require.NoError(t, reporter.Close())
	sent := messages()
	require.Len(t, sent, 13)
	assert.Equal(t, []string{"review_viewed:web", "review_viewed:tui", "review_viewed:cli"}, surfacesOf(sent[:3]))
	for _, message := range sent {
		assert.NotContains(t, message.Properties, "job_id")
		assert.NotContains(t, message.Properties, "output")
	}
	assert.Equal(t, []string{"finding_fixed:tui", "finding_fixed:tui", "finding_fixed:cli", "finding_fixed:cli"}, surfacesOf(sent[3:7]))
	assert.Equal(t, []string{"review_closed:web", "review_closed:web", "review_closed:tui", "review_closed:tui", "review_closed:cli", "review_closed:cli"}, surfacesOf(sent[7:]))
}

func TestCoreActionDailyClaims(t *testing.T) {
	t.Setenv(EnabledEnv, "1")
	t.Setenv(GenericEnabledEnv, "1")
	synctest.Test(t, func(t *testing.T) {
		var sent []testutil.PostHogMessage
		srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var reader io.Reader = r.Body
			if r.Header.Get("Content-Encoding") == "gzip" {
				gz, err := gzip.NewReader(r.Body)
				if !assert.NoError(t, err) {
					return
				}
				defer gz.Close()
				reader = gz
			}
			var batch struct {
				Batch []testutil.PostHogMessage `json:"batch"`
			}
			assert.NoError(t, json.NewDecoder(reader).Decode(&batch))
			sent = append(sent, batch.Batch...)
			_, _ = io.WriteString(w, `{"status":1}`)
		}))
		// Kit delegates HTTP to DefaultTransport; the in-memory network lets
		// PostHog's drain timer and the UTC rollover use virtual time.
		transport := http.DefaultTransport
		http.DefaultTransport = srv.Client().Transport
		defer func() { http.DefaultTransport = transport }()
		endpoint := "http://posthog.example.com"
		opts := Options{Database: testutil.OpenTestDB(t), Endpoint: endpoint, DailyClaimsPath: filepath.Join(t.TempDir(), "daily.json")}
		view := `{"event":"review_viewed","properties":{"surface":"tui"}}`
		t.Setenv(EnabledEnv, "0")
		disabled, err := NewReporter(opts)
		require.NoError(t, err)
		assert.Equal(t, http.StatusAccepted, postCaptureEvent((&AppOpenedLimiter{}).Handler(disabled), view).Code)
		require.NoError(t, disabled.Close())
		t.Setenv(EnabledEnv, "1")
		for range 2 {
			reporter, err := NewReporter(opts)
			require.NoError(t, err)
			handler := (&AppOpenedLimiter{}).Handler(reporter)
			assert.Equal(t, http.StatusAccepted, postCaptureEvent(handler, view).Code)
			// Review views and screen views are separate daily events on one surface.
			assert.Equal(t, http.StatusAccepted, postCaptureEvent(handler, `{"event":"screen_viewed","properties":{"screen":"review","surface":"tui"}}`).Code)
			require.NoError(t, reporter.Close())
		}
		assert.Equal(t, []string{"review_viewed:tui", "screen_viewed:tui"}, surfacesOf(sent))
		time.Sleep(time.Until(time.Now().UTC().Truncate(24 * time.Hour).Add(24 * time.Hour)))
		reporter, err := NewReporter(opts)
		require.NoError(t, err)
		assert.Equal(t, http.StatusAccepted, postCaptureEvent((&AppOpenedLimiter{}).Handler(reporter), view).Code)
		require.NoError(t, reporter.Close())
		assert.Equal(t, []string{"review_viewed:tui", "screen_viewed:tui", "review_viewed:tui"}, surfacesOf(sent))
	})
}
