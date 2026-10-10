package telemetry

import (
	"bufio"
	"context"
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

func TestScreenViewedFailedReservationRetriesAfterRestart(t *testing.T) {
	t.Setenv(EnabledEnv, "1")
	t.Setenv(GenericEnabledEnv, "1")
	path := filepath.Join(t.TempDir(), "daily.json")
	require.NoError(t, os.Mkdir(path, 0o700))
	reporter, messages := newPostHogStubReporter(t, path)
	body := `{"event":"screen_viewed","properties":{"screen":"queue","surface":"tui"}}`
	limiter := &AppOpenedLimiter{}
	assert.Equal(t, http.StatusInternalServerError, postThroughLimiter(limiter, reporter, body).Code)
	require.NoError(t, reporter.Close())
	assert.Empty(t, messages())
	require.NoError(t, os.Remove(path))
	reporter, messages = newPostHogStubReporter(t, path)
	limiter = &AppOpenedLimiter{}
	assert.Equal(t, http.StatusAccepted, postThroughLimiter(limiter, reporter, body).Code)
	require.NoError(t, reporter.Close())
	assert.Len(t, messages(), 1)
}

func TestScreenViewedAcceptsWithClosedDatabase(t *testing.T) {
	t.Setenv(EnabledEnv, "1")
	t.Setenv(GenericEnabledEnv, "1")
	db := testutil.OpenTestDB(t)
	endpoint, messages := testutil.NewPostHogStub(t)
	reporter, err := NewReporter(Options{Database: db, Endpoint: endpoint, DailyClaimsPath: filepath.Join(t.TempDir(), "daily.json")})
	require.NoError(t, err)
	require.NoError(t, db.Close())
	assert.Equal(t, http.StatusAccepted, postThroughLimiter(&AppOpenedLimiter{}, reporter, `{"event":"screen_viewed","properties":{"screen":"queue","surface":"tui"}}`).Code)
	require.NoError(t, reporter.Close())
	assert.Len(t, messages(), 1)
}
