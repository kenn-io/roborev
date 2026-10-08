package telemetry

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
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
		for _, message := range sent {
			assert.Equal("anonymous-install-id", message.DistinctID)
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
	for _, body := range []string{
		`{"event":"app_opened","properties":{"surface":"cli"}}`,
		`{"event":"screen_viewed","properties":{"screen":"queue","surface":"tui"}}`,
	} {
		t.Run(body, func(t *testing.T) {
			reporter, messages := newPostHogStubReporter(t)
			limiter := &AppOpenedLimiter{Database: testutil.OpenTestDB(t)}
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
		})
	}
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

func TestScreenViewedLimiterPersistsAcrossInterfacesAndRestarts(t *testing.T) {
	t.Setenv(EnabledEnv, "1")
	t.Setenv(GenericEnabledEnv, "1")
	db := testutil.OpenTestDB(t)
	reporter, messages := newPostHogStubReporter(t)
	now := time.Date(2026, 3, 1, 23, 59, 0, 0, time.UTC)
	limiter := &AppOpenedLimiter{Database: db, now: func() time.Time { return now }}
	reviews := `{"event":"screen_viewed","properties":{"screen":"reviews","surface":"web","path":"/private"}}`
	require.Equal(t, http.StatusAccepted, postThroughLimiter(limiter, reporter, reviews).Code)
	limiter = &AppOpenedLimiter{Database: db, now: func() time.Time { return now }}
	for _, body := range []string{
		reviews,
		`{"event":"screen_viewed","properties":{"screen":"reviews","surface":"tui"}}`,
		`{"event":"screen_viewed","properties":{"screen":"analytics","surface":"web"}}`,
		`{"event":"app_opened","properties":{"surface":"web"}}`,
		`{"event":"screen_viewed"}`,
		`{"event":"screen_viewed","properties":{"screen":123}}`,
		`{"event":"screen_viewed","properties":{"screen":"unknown"}}`,
	} {
		assert.Equal(t, http.StatusAccepted, postThroughLimiter(limiter, reporter, body).Code)
	}
	now = now.Add(time.Minute)
	assert.Equal(t, http.StatusAccepted, postThroughLimiter(limiter, reporter, reviews).Code)
	require.NoError(t, reporter.Close())
	got := messages()
	require.Len(t, got, 4)
	assert.Equal(t, "reviews", got[0].Properties[PropertyScreen])
	assert.Equal(t, "analytics", got[1].Properties[PropertyScreen])
	assert.Equal(t, EventAppOpened, got[2].Event)
	assert.Equal(t, "reviews", got[3].Properties[PropertyScreen])
	assert.NotContains(t, got[0].Properties, "path")
	for i := range 8 {
		t.Run(fmt.Sprint("padded aliases ", i), func(t *testing.T) {
			reporter, messages := newPostHogStubReporter(t)
			db := testutil.OpenTestDB(t)
			limiter := &AppOpenedLimiter{Database: db, now: func() time.Time { return now }}
			body := `{"event":"screen_viewed","properties":{"screen":"queue"," screen ":"review","surface":"web"," surface ":"tui","path":"/private"}}`
			skip, finish, canonical := limiter.alreadySentToday(reporter, []byte(body))
			require.False(t, skip)
			require.NotNil(t, finish)
			var normalized struct {
				Properties map[string]any `json:"properties"`
			}
			require.NoError(t, json.Unmarshal(canonical, &normalized))
			assert.NotContains(t, normalized.Properties, " screen ")
			assert.NotContains(t, normalized.Properties, " surface ")
			assert.NotContains(t, normalized.Properties, "path")
			stored, err := db.GetSyncState("telemetry.screen." + normalized.Properties[PropertyScreen].(string))
			require.NoError(t, err)
			assert.Empty(t, stored)
			finish(false)
			assert.Equal(t, http.StatusAccepted, postThroughLimiter(limiter, reporter, body).Code)
			claimed := "review"
			queueDay, err := db.GetSyncState("telemetry.screen.queue")
			require.NoError(t, err)
			if queueDay == now.UTC().Format(time.DateOnly) {
				claimed = "queue"
			}
			for _, screen := range []string{"queue", "review"} {
				ordinary := fmt.Sprintf(`{"event":"screen_viewed","properties":{"screen":%q,"surface":"tui"}}`, screen)
				assert.Equal(t, http.StatusAccepted, postThroughLimiter(limiter, reporter, ordinary).Code)
			}
			require.NoError(t, reporter.Close())
			got := messages()
			require.Len(t, got, 2)
			assert.Equal(t, claimed, got[0].Properties[PropertyScreen])
			assert.ElementsMatch(t, []any{"queue", "review"}, []any{got[0].Properties[PropertyScreen], got[1].Properties[PropertyScreen]})
		})
	}
}

func TestScreenViewedClaimsFinishingOutOfOrder(t *testing.T) {
	t.Setenv(EnabledEnv, "1")
	t.Setenv(GenericEnabledEnv, "1")
	db := testutil.OpenTestDB(t)
	reporter, _ := newPostHogStubReporter(t)
	t.Cleanup(func() { require.NoError(t, reporter.Close()) })
	now := time.Date(2026, 3, 1, 23, 59, 0, 0, time.UTC)
	limiter := &AppOpenedLimiter{Database: db, now: func() time.Time { return now }}
	body := []byte(`{"event":"screen_viewed","properties":{"screen":"queue","surface":"tui"}}`)
	skip, earlier, _ := limiter.alreadySentToday(reporter, body)
	require.False(t, skip)
	require.NotNil(t, earlier)
	now = now.Add(time.Minute)
	skip, later, _ := limiter.alreadySentToday(reporter, body)
	require.False(t, skip)
	require.NotNil(t, later)
	later(true)
	earlier(true)
	stored, err := db.GetSyncState("telemetry.screen.queue")
	require.NoError(t, err)
	assert.Equal(t, now.Format(time.DateOnly), stored)
	skip, _, _ = limiter.alreadySentToday(reporter, body)
	assert.True(t, skip)
	limiter = &AppOpenedLimiter{Database: db, now: func() time.Time { return now }}
	skip, _, _ = limiter.alreadySentToday(reporter, body)
	assert.True(t, skip)
	now = now.Add(-time.Minute)
	skip, _, _ = limiter.alreadySentToday(reporter, body)
	assert.True(t, skip)
}

func TestScreenViewedUnacceptedClaims(t *testing.T) {
	t.Setenv(EnabledEnv, "1")
	t.Setenv(GenericEnabledEnv, "1")
	for _, failure := range []string{"read", "write", "enqueue"} {
		t.Run(failure, func(t *testing.T) {
			reporter, messages := newPostHogStubReporter(t)
			db := testutil.OpenTestDB(t)
			limiter := &AppOpenedLimiter{Database: db}
			body := `{"event":"screen_viewed","properties":{"screen":"queue","surface":"tui"}}`
			switch failure {
			case "read":
				require.NoError(t, db.Close())
				assert.Equal(t, http.StatusAccepted, postThroughLimiter(limiter, reporter, body).Code)
			case "write":
				_, err := db.Exec(`CREATE TRIGGER reject_screen_claim BEFORE INSERT ON sync_state BEGIN SELECT RAISE(FAIL, 'metadata unavailable'); END`)
				require.NoError(t, err)
				assert.Equal(t, http.StatusAccepted, postThroughLimiter(limiter, reporter, body).Code)
				assert.Equal(t, time.Now().UTC().Format(time.DateOnly), limiter.sent["telemetry.screen.queue"])
				_, err = db.Exec(`DROP TRIGGER reject_screen_claim`)
				require.NoError(t, err)
				assert.Equal(t, http.StatusAccepted, postThroughLimiter(limiter, reporter, body).Code)
			case "enqueue":
				require.NoError(t, db.SetSyncState("telemetry.screen.queue", "2026-01-01"))
				skip, finish, _ := limiter.alreadySentToday(reporter, []byte(body))
				require.False(t, skip)
				require.NotNil(t, finish)
				finish(false)
				stored, err := db.GetSyncState("telemetry.screen.queue")
				require.NoError(t, err)
				assert.Equal(t, "2026-01-01", stored)
				assert.Equal(t, http.StatusAccepted, postThroughLimiter(limiter, reporter, body).Code)
			}
			require.NoError(t, reporter.Close())
			if failure == "read" {
				assert.Empty(t, messages())
			} else {
				assert.Len(t, messages(), 1)
			}
		})
	}
}
