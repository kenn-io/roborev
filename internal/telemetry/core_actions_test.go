package telemetry_test

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/telemetry"
	"go.kenn.io/roborev/internal/testenv"
	"go.kenn.io/roborev/internal/testutil"
)

func TestCoreActionDailyClaims(t *testing.T) {
	testenv.SetDataDir(t)
	t.Setenv(telemetry.EnabledEnv, "1")
	t.Setenv(telemetry.GenericEnabledEnv, "1")
	synctest.Test(t, func(t *testing.T) {
		recorder, messages := testutil.NewPostHogRecorder(t)
		stub := httptest.NewTestServer(t, recorder)
		transport := http.DefaultTransport
		http.DefaultTransport = stub.Client().Transport
		defer func() { http.DefaultTransport = transport }()
		db := testutil.OpenTestDB(t)
		server := daemon.NewServer(db, config.DefaultConfig(), "")
		t.Cleanup(func() { require.NoError(t, server.Close()) })
		opts := telemetry.Options{Database: db, Endpoint: "http://posthog.example.com", DailyClaimsPath: filepath.Join(t.TempDir(), "daily.json")}
		var handler http.Handler
		post := func(body string) *httptest.ResponseRecorder {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, daemon.TelemetryEventsPath, strings.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			handler.ServeHTTP(recorder, request)
			return recorder
		}
		surfacesOf := func(messages []testutil.PostHogMessage) []string {
			var result []string
			for _, message := range messages {
				result = append(result, message.Event+":"+message.Properties["surface"].(string))
			}
			return result
		}
		view := `{"event":"review_viewed","properties":{"surface":"tui"}}`
		for restart := range 2 {
			reporter, err := telemetry.NewReporter(opts)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, reporter.Close()) })
			server.SetTelemetry(reporter)
			handler = server.Handler()
			if restart == 0 {
				for _, event := range []string{"review_viewed", "finding_fixed", "review_closed"} {
					for _, surface := range []string{"web", "tui", "cli"} {
						body := `{"event":"` + event + `","properties":{"surface":"` + surface + `","job_id":42,"output":"private review"}}`
						status := http.StatusAccepted
						if event == "finding_fixed" && surface == "web" {
							status = http.StatusBadRequest
						}
						assert.Equal(t, status, post(body).Code)
						assert.Equal(t, status, post(body).Code)
					}
				}
				for _, properties := range []string{`{}`, `{"surface":"unknown"}`, `{"surface":42}`} {
					for _, event := range []string{"review_viewed", "finding_fixed", "review_closed"} {
						assert.Equal(t, http.StatusBadRequest, post(`{"event":"`+event+`","properties":`+properties+`}`).Code)
					}
				}
			}
			assert.Equal(t, http.StatusAccepted, post(view).Code)
			// Review views and screen views have independent daily claims.
			assert.Equal(t, http.StatusAccepted, post(`{"event":"screen_viewed","properties":{"screen":"review","surface":"tui"}}`).Code)
			require.NoError(t, reporter.Close())
		}
		sent := messages()
		require.Len(t, sent, 14)
		assert.Equal(t, []string{"review_viewed:web", "review_viewed:tui", "review_viewed:cli"}, surfacesOf(sent[:3]))
		for _, message := range sent {
			assert.NotContains(t, message.Properties, "job_id")
			assert.NotContains(t, message.Properties, "output")
		}
		assert.Equal(t, []string{"finding_fixed:tui", "finding_fixed:tui", "finding_fixed:cli", "finding_fixed:cli"}, surfacesOf(sent[3:7]))
		assert.Equal(t, []string{"review_closed:web", "review_closed:web", "review_closed:tui", "review_closed:tui", "review_closed:cli", "review_closed:cli"}, surfacesOf(sent[7:13]))
		assert.Equal(t, "screen_viewed:tui", surfacesOf(sent[13:])[0])
		time.Sleep(time.Until(time.Now().UTC().Truncate(24 * time.Hour).Add(24 * time.Hour)))
		reporter, err := telemetry.NewReporter(opts)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, reporter.Close()) })
		server.SetTelemetry(reporter)
		handler = server.Handler()
		assert.Equal(t, http.StatusAccepted, post(view).Code)
		require.NoError(t, reporter.Close())
		sent = messages()
		require.Len(t, sent, 15)
		assert.Equal(t, "review_viewed:tui", surfacesOf(sent[14:])[0])
	})
}
