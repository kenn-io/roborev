package telemetry

import (
	"context"
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

func surfacesOf(messages []postHogWireMessage) []string {
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
			`{"event":"app_opened","properties":{"surface":"tui"}}`,
			`{"event":"app_opened","properties":{"surface":"web"}}`,
			`{"event":"app_opened","properties":{"surface":"tui"}}`,
			"{\"event\":\"app_opened\",\"properties\":{\"surface\":\"cli\"}}  \n\t",
			`{"event":"app_opened"}`,
			`{"event":"app_opened"}`,
		} {
			recorder := postThroughLimiter(limiter, reporter, body)
			assert.Equal(http.StatusAccepted, recorder.Code, body)
			assert.JSONEq(`{"status":"queued"}`, recorder.Body.String(), body)
		}
		require.NoError(t, reporter.Close())
		assert.Equal([]string{"app_opened:cli", "app_opened:tui", "app_opened:web", "app_opened:"}, surfacesOf(messages()))
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

	t.Setenv(EnabledEnv, "1")
	t.Setenv(GenericEnabledEnv, "1")
	reporter, messages := newPostHogStubReporter(t)
	assert.Equal(http.StatusBadRequest, postThroughLimiter(limiter, reporter, `{"event":"page_viewed"}`).Code)
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
	daemonStarted := `{"event":"daemon_started","properties":{"repo_count":1}}`
	assert.Equal(http.StatusAccepted, postThroughLimiter(limiter, reporter, daemonStarted).Code)
	assert.Equal(http.StatusAccepted, postThroughLimiter(limiter, reporter, daemonStarted).Code)
	require.NoError(t, reporter.Close())

	assert.Equal([]string{"app_opened:cli", "daemon_started:", "daemon_started:"}, surfacesOf(messages()))
}

func TestAppOpenedLimiterConcurrentFirstRequestsSendOnce(t *testing.T) {
	t.Setenv(EnabledEnv, "1")
	t.Setenv(GenericEnabledEnv, "1")
	reporter, messages := newPostHogStubReporter(t)
	limiter := &AppOpenedLimiter{}

	var wg sync.WaitGroup
	codes := make([]int, 8)
	for i := range codes {
		wg.Go(func() {
			codes[i] = postThroughLimiter(limiter, reporter, `{"event":"app_opened","properties":{"surface":"cli"}}`).Code
		})
	}
	wg.Wait()
	require.NoError(t, reporter.Close())

	for _, code := range codes {
		assert.Equal(t, http.StatusAccepted, code)
	}
	assert.Equal(t, []string{"app_opened:cli"}, surfacesOf(messages()))
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
}

func TestScreenViewedLimiterConcurrentRequestsSendOnce(t *testing.T) {
	t.Setenv(EnabledEnv, "1")
	t.Setenv(GenericEnabledEnv, "1")
	reporter, messages := newPostHogStubReporter(t)
	limiter := &AppOpenedLimiter{Database: testutil.OpenTestDB(t)}
	var wg sync.WaitGroup
	codes := make([]int, 8)
	for i := range codes {
		wg.Go(func() {
			codes[i] = postThroughLimiter(limiter, reporter, `{"event":"screen_viewed","properties":{"screen":"queue","surface":"tui"}}`).Code
		})
	}
	wg.Wait()
	require.NoError(t, reporter.Close())
	for _, code := range codes {
		assert.Equal(t, http.StatusAccepted, code)
	}
	assert.Len(t, messages(), 1)
}

func TestScreenViewedRejectedClaimCanRetry(t *testing.T) {
	t.Setenv(EnabledEnv, "1")
	t.Setenv(GenericEnabledEnv, "1")
	reporter, messages := newPostHogStubReporter(t)
	limiter := &AppOpenedLimiter{Database: testutil.OpenTestDB(t)}
	body := []byte(`{"event":"screen_viewed","properties":{"screen":"queue","surface":"tui"}}`)
	skip, finish := limiter.alreadySentToday(reporter, body)
	require.False(t, skip)
	require.NotNil(t, finish)
	finish(false)
	assert.Equal(t, http.StatusAccepted, postThroughLimiter(limiter, reporter, string(body)).Code)
	require.NoError(t, reporter.Close())
	assert.Len(t, messages(), 1)
}

func TestScreenViewedMetadataFailureSendsNothing(t *testing.T) {
	t.Setenv(EnabledEnv, "1")
	t.Setenv(GenericEnabledEnv, "1")
	reporter, messages := newPostHogStubReporter(t)
	db := testutil.OpenTestDB(t)
	require.NoError(t, db.Close())
	limiter := &AppOpenedLimiter{Database: db}
	assert.Equal(t, http.StatusAccepted, postThroughLimiter(limiter, reporter, `{"event":"screen_viewed","properties":{"screen":"queue"}}`).Code)
	require.NoError(t, reporter.Close())
	assert.Empty(t, limiter.sent)
	assert.Empty(t, messages())
}

func TestScreenViewedMetadataWriteFailureSendsNothing(t *testing.T) {
	t.Setenv(EnabledEnv, "1")
	t.Setenv(GenericEnabledEnv, "1")
	reporter, messages := newPostHogStubReporter(t)
	db := testutil.OpenTestDB(t)
	_, err := db.Exec(`CREATE TRIGGER reject_screen_claim BEFORE INSERT ON sync_state BEGIN SELECT RAISE(FAIL, 'metadata unavailable'); END`)
	require.NoError(t, err)
	limiter := &AppOpenedLimiter{Database: db}
	body := `{"event":"screen_viewed","properties":{"screen":"queue"}}`
	assert.Equal(t, http.StatusAccepted, postThroughLimiter(limiter, reporter, body).Code)
	assert.Empty(t, limiter.sent)
	_, err = db.Exec(`DROP TRIGGER reject_screen_claim`)
	require.NoError(t, err)
	assert.Equal(t, http.StatusAccepted, postThroughLimiter(limiter, reporter, body).Code)
	require.NoError(t, reporter.Close())
	assert.Len(t, messages(), 1)
}
