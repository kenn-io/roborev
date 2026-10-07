package tui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/telemetry"
	"go.kenn.io/roborev/internal/testutil"
)

// The daemon package disables PostHog process-wide in its tests, so the enabled route is exercised here for both surfaces.
func TestSessionEndedCapturedThroughDaemonRoute(t *testing.T) {
	tests := []struct {
		surface string
		bucket  string
		send    func(t *testing.T, url string, messages func() []testutil.PostHogMessage)
	}{
		{telemetry.SurfaceTUI, telemetry.Duration1To5m, func(t *testing.T, url string, messages func() []testutil.PostHogMessage) {
			m := newModel(testEndpointFromURL(url), withExternalIODisabled())
			m.reportSessionEnded(2 * time.Minute)
			assert.Empty(t, messages(), "no PostHog message before readiness")
			close(m.ready)
			t.Setenv(telemetry.EnabledEnv, "0")
			m.reportSessionEnded(time.Minute)
			assert.Empty(t, messages(), "no PostHog message after opt-out")
			enableTelemetryEnv(t)
			m.reportSessionEnded(2 * time.Minute)
		}},
		{telemetry.SurfaceWeb, telemetry.DurationUnder1m, func(t *testing.T, url string, _ func() []testutil.PostHogMessage) {
			body := `{"event":"session_ended","properties":{"surface":"web","duration_bucket":"under_1m"}}`
			response, err := http.Post(url+daemon.TelemetryEventsPath, "application/json", strings.NewReader(body))
			require.NoError(t, err)
			require.NoError(t, response.Body.Close())
			assert.Equal(t, http.StatusAccepted, response.StatusCode)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.surface, func(t *testing.T) {
			enableTelemetryEnv(t)
			t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
			db := testutil.OpenTestDB(t)
			endpoint, messages := testutil.NewPostHogStub(t)
			reporter, err := telemetry.NewReporter(telemetry.Options{Database: db, Endpoint: endpoint})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, reporter.Close()) })
			server := daemon.NewServer(db, &config.Config{}, "")
			t.Cleanup(func() { require.NoError(t, server.Close()) })
			server.SetTelemetry(reporter)
			srv := httptest.NewServer(server.Handler())
			t.Cleanup(srv.Close)
			tt.send(t, srv.URL, messages)
			require.NoError(t, reporter.Close())
			sent := messages()
			require.Len(t, sent, 1)
			assert.Equal(t, telemetry.EventSessionEnded, sent[0].Event)
			assert.Equal(t, tt.surface, sent[0].Properties[telemetry.PropertySurface])
			assert.Equal(t, tt.bucket, sent[0].Properties[telemetry.PropertyDurationBucket])
		})
	}
}
