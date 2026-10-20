package telemetry_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

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
	releaseVersion := testutil.SetTelemetryReleaseEnv(t)
	t.Setenv(telemetry.EnabledEnv, "1")
	t.Setenv(telemetry.GenericEnabledEnv, "1")
	recorder, messages := testutil.NewPostHogRecorder(t)
	stub := httptest.NewTestServer(t, recorder)
	transport := http.DefaultTransport
	http.DefaultTransport = stub.Client().Transport
	defer func() { http.DefaultTransport = transport }()
	db := testutil.OpenTestDB(t)
	server := daemon.NewServer(db, config.DefaultConfig(), "")
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	opts := telemetry.Options{Version: releaseVersion, Database: db, Endpoint: "http://posthog.example.com", DailyClaimsPath: filepath.Join(t.TempDir(), "daily.json")}
	reporter, err := telemetry.NewReporter(opts)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reporter.Close()) })
	server.SetTelemetry(reporter)
	handler := server.Handler()
	// CLI clients stop waiting shortly after exit, so their reports arrive with the connection already closed.
	disconnected, disconnect := context.WithCancel(t.Context())
	disconnect()
	post := func(body string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		ctx := t.Context()
		if strings.Contains(body, `"surface":"cli"`) {
			ctx = disconnected
		}
		request := httptest.NewRequestWithContext(ctx, http.MethodPost, daemon.TelemetryEventsPath, strings.NewReader(body))
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
	for _, event := range []string{"review_viewed", "finding_fixed", "review_closed"} {
		for _, surface := range []string{"web", "tui", "cli"} {
			if event == "finding_fixed" && surface == "web" {
				continue
			}
			body := `{"event":"` + event + `","properties":{"surface":"` + surface + `"}}`
			assert.Equal(t, http.StatusAccepted, post(body).Code)
			assert.Equal(t, http.StatusAccepted, post(body).Code)
		}
	}
	// Review views and screen views have independent daily claims.
	assert.Equal(t, http.StatusAccepted, post(`{"event":"screen_viewed","properties":{"screen":"review","surface":"tui"}}`).Code)
	for range 2 {
		assert.Equal(t, http.StatusAccepted, post(`{"event":"error_shown","properties":{"surface":"tui","kind":"review_closed"}}`).Code)
	}
	require.NoError(t, reporter.Close())
	sent := messages()
	require.Len(t, sent, 11)
	assert.Equal(t, []string{
		"review_viewed:web", "review_viewed:tui", "review_viewed:cli",
		"finding_fixed:tui", "finding_fixed:cli",
		"review_closed:web", "review_closed:tui", "review_closed:cli",
		"screen_viewed:tui",
		"error_shown:tui", "error_shown:tui",
	}, surfacesOf(sent))
	assert.Equal(t, "review_closed", sent[9].Properties["kind"])
	assert.Equal(t, "review_closed", sent[10].Properties["kind"])
}
