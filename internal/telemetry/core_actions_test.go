package telemetry_test

import (
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

func TestCoreActionCapture(t *testing.T) {
	testenv.SetDataDir(t)
	t.Setenv(telemetry.EnabledEnv, "1")
	t.Setenv(telemetry.GenericEnabledEnv, "1")
	db := testutil.OpenTestDB(t)
	server := daemon.NewServer(db, config.DefaultConfig(), "")
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	endpoint, messages := testutil.NewPostHogStub(t)
	reporter, err := telemetry.NewReporter(telemetry.Options{Database: db, Endpoint: endpoint, DailyClaimsPath: filepath.Join(t.TempDir(), "daily.json")})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reporter.Close()) })
	server.SetTelemetry(reporter)
	handler := server.Handler()
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
	assert.Equal(t, http.StatusAccepted, post(`{"event":"app_opened","properties":{"surface":"cli"}}`).Code)
	for _, event := range []string{"review_viewed", "finding_fixed", "review_closed"} {
		for _, surface := range []string{"web", "tui", "cli"} {
			body := `{"event":"` + event + `","properties":{"surface":"` + surface + `","job_id":42,"output":"private review"}}`
			for range 2 {
				status := http.StatusAccepted
				if event == "finding_fixed" && surface == "web" {
					status = http.StatusBadRequest
				}
				assert.Equal(t, status, post(body).Code)
			}
		}
	}
	for _, properties := range []string{`{}`, `{"surface":"unknown"}`, `{"surface":42}`} {
		for _, event := range []string{"review_viewed", "finding_fixed", "review_closed"} {
			assert.Equal(t, http.StatusBadRequest, post(`{"event":"`+event+`","properties":`+properties+`}`).Code)
		}
	}
	require.NoError(t, reporter.Close())
	sent := messages()
	require.Len(t, sent, 14)
	assert.Equal(t, "app_opened", sent[0].Event)
	sent = sent[1:]
	assert.Equal(t, []string{"review_viewed:web", "review_viewed:tui", "review_viewed:cli"}, surfacesOf(sent[:3]))
	for _, message := range sent {
		assert.NotContains(t, message.Properties, "job_id")
		assert.NotContains(t, message.Properties, "output")
	}
	assert.Equal(t, []string{"finding_fixed:tui", "finding_fixed:tui", "finding_fixed:cli", "finding_fixed:cli"}, surfacesOf(sent[3:7]))
	assert.Equal(t, []string{"review_closed:web", "review_closed:web", "review_closed:tui", "review_closed:tui", "review_closed:cli", "review_closed:cli"}, surfacesOf(sent[7:]))
}
