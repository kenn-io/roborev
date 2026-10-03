package telemetry

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kittelemetry "go.kenn.io/kit/telemetry"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func TestEnabledFromEnvHonorsRoborevAndGenericOptOut(t *testing.T) {
	t.Setenv(EnabledEnv, "0")
	assert.False(t, EnabledFromEnv())

	t.Setenv(EnabledEnv, "1")
	assert.True(t, EnabledFromEnv())

	t.Setenv(GenericEnabledEnv, "0")
	assert.False(t, EnabledFromEnv())
}

func TestNewReporterDisabledByEnvDoesNotCreateInstallID(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	t.Setenv(EnabledEnv, "0")
	database := openTestDB(t)

	reporter, err := NewReporter(Options{Database: database})
	require.NoError(err)

	assert.False(reporter.Enabled())
	value, err := database.GetSyncState(installIDMetadataKey)
	require.NoError(err)
	assert.Empty(value)
	installedAt, err := database.GetSyncState(installedAtKey)
	require.NoError(err)
	assert.Empty(installedAt)
}

func TestLoadOrCreateInstallIsStableAndAnonymous(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := openTestDB(t)

	first, _, err := loadOrCreateInstall(database)
	require.NoError(err)
	second, _, err := loadOrCreateInstall(database)
	require.NoError(err)

	assert.Len(first, 32)
	assert.Equal(first, second)

	stored, err := database.GetSyncState(installIDMetadataKey)
	require.NoError(err)
	assert.Equal(first, stored)
}

func TestLoadOrCreateInstallRecordsCreationTimeForNewID(t *testing.T) {
	require := require.New(t)

	database := openTestDB(t)

	before := time.Now()
	_, installedAt, err := loadOrCreateInstall(database)
	require.NoError(err)

	assert.WithinRange(t, installedAt, before.Add(-time.Second), time.Now().Add(time.Second))
}

func TestLoadOrCreateInstallLeavesExistingIDWithoutTimeUnaged(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := openTestDB(t)
	require.NoError(database.SetSyncState(installIDMetadataKey, "existing-install-id"))

	id, installedAt, err := loadOrCreateInstall(database)
	require.NoError(err)

	assert.Equal("existing-install-id", id)
	assert.True(installedAt.IsZero())
	stored, err := database.GetSyncState(installedAtKey)
	require.NoError(err)
	assert.Empty(stored)
}

func TestLoadOrCreateInstallKeepsCreationTimeAcrossRestart(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database, dir := testutil.OpenTestDBWithDir(t)
	firstID, firstInstalledAt, err := loadOrCreateInstall(database)
	require.NoError(err)
	require.NoError(database.Close())

	reopened, err := storage.Open(filepath.Join(dir, "test.db"))
	require.NoError(err)
	t.Cleanup(func() { require.NoError(reopened.Close()) })
	secondID, secondInstalledAt, err := loadOrCreateInstall(reopened)
	require.NoError(err)

	assert.Equal(firstID, secondID)
	assert.Equal(firstInstalledAt, secondInstalledAt)
	assert.False(secondInstalledAt.IsZero())
}

func TestAllowedEventOptionsConfigureRoborevDaemonEvents(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	t.Setenv(EnabledEnv, "1")
	t.Setenv(GenericEnabledEnv, "1")

	reporter, err := kittelemetry.NewPostHogReporter(kittelemetry.PostHogOptions{
		APIKey:      "test-posthog-api-key",
		Application: "roborev",
		EnvPrefix:   "ROBOREV",
		DistinctID:  "anonymous-install-id",
		Version:     "test-version",
		Source:      "daemon",
	}, allowedEventOptions()...)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(reporter.Close()) })

	assert.True(reporter.EventAllowed(EventDaemonStarted))
	assert.True(reporter.EventAllowed(EventDaemonActive))
	assert.True(reporter.EventAllowed(EventAppOpened))
	assert.False(reporter.EventAllowed("repo_opened"))

	props, err := reporter.SanitizeProperties(EventDaemonActive, map[string]any{
		"repo_count":              3,
		"review_count":            7,
		"sync_enabled":            true,
		"worker_count":            4,
		"$process_person_profile": true,
		"$geoip_disable":          false,
		"application":             "caller-app",
	})
	require.NoError(err)

	assert.Equal(3, props["repo_count"])
	assert.Equal(7, props["review_count"])
	assert.Equal(true, props["sync_enabled"])
	assert.NotContains(props, "worker_count")
	assert.Equal("roborev", props["application"])
	assert.Equal("test-version", props["version"])
	assert.Equal("daemon", props["source"])
	assert.False(props["$process_person_profile"].(bool))
	assert.True(props["$geoip_disable"].(bool))

	props, err = reporter.SanitizeProperties(EventAppOpened, map[string]any{"surface": "tui", "repo_count": 3})
	require.NoError(err)
	assert.Equal("tui", props["surface"])
	assert.NotContains(props, "repo_count")
}

func TestAppOpenedSurfaceAcceptsOnlyFixedValues(t *testing.T) {
	t.Setenv(EnabledEnv, "0")
	reporter, err := NewReporter(Options{})
	require.NoError(t, err)

	tests := []struct {
		name  string
		value any
		want  string
	}{
		{name: "tui", value: "tui", want: "tui"},
		{name: "web", value: "web", want: "web"},
		{name: "cli", value: "cli", want: "cli"},
		{name: "padded tui", value: " tui ", want: "tui"},
		{name: "upper case", value: "TUI"},
		{name: "unknown surface", value: "desktop"},
		{name: "empty", value: ""},
		{name: "comma list", value: "tui,web"},
		{name: "number", value: 1.0},
		{name: "bool", value: true},
		{name: "nil", value: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			props, err := reporter.SanitizeProperties(EventAppOpened, map[string]any{PropertySurface: tt.value})
			require.NoError(t, err)
			if tt.want == "" {
				assert.NotContains(t, props, PropertySurface)
				return
			}
			assert.Equal(t, tt.want, props[PropertySurface])
		})
	}
}

func TestNewReporterOptedOutKeepsAllowlist(t *testing.T) {
	for _, env := range []string{EnabledEnv, GenericEnabledEnv} {
		t.Run(env, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			t.Setenv(env, "0")
			database := openTestDB(t)

			reporter, err := NewReporter(Options{Database: database})
			require.NoError(err)

			assert.False(reporter.Enabled())
			assert.True(reporter.EventAllowed(EventAppOpened))
			assert.True(reporter.EventAllowed(EventDaemonStarted))
			assert.True(reporter.EventAllowed(EventDaemonActive))
			assert.False(reporter.EventAllowed("search_run"))
			assert.False(reporter.EventAllowed("App_Opened"))
			installID, err := database.GetSyncState(installIDMetadataKey)
			require.NoError(err)
			assert.Empty(installID)
		})
	}
}

func TestCaptureHandlerOptedOutAnswersDisabled(t *testing.T) {
	t.Setenv(EnabledEnv, "0")
	reporter, err := NewReporter(Options{})
	require.NoError(t, err)
	handler := NewCaptureHandler(reporter)

	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantBody   string
	}{
		{name: "allowed event", body: `{"event":"app_opened"}`, wantStatus: http.StatusAccepted, wantBody: `{"status":"disabled"}`},
		{name: "wrong case", body: `{"event":"App_Opened"}`, wantStatus: http.StatusBadRequest, wantBody: ErrUnsupportedEvent.Error()},
		{name: "blank event", body: `{"event":""}`, wantStatus: http.StatusBadRequest, wantBody: ErrUnsupportedEvent.Error()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := postCaptureEvent(handler, tt.body)
			assert.Equal(t, tt.wantStatus, recorder.Code)
			assert.Contains(t, recorder.Body.String(), tt.wantBody)
		})
	}
}

func TestCaptureHandlerSendsAppOpenedWithOnlyAllowedProperties(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	t.Setenv(EnabledEnv, "1")
	t.Setenv(GenericEnabledEnv, "1")

	reporter, messages := newPostHogStubReporter(t)

	recorder := postCaptureEvent(NewCaptureHandler(reporter), `{"event":"app_opened","properties":{"pad":"x","repo_count":3}}`)
	assert.Equal(http.StatusAccepted, recorder.Code)
	assert.JSONEq(`{"status":"queued"}`, recorder.Body.String())
	require.NoError(reporter.Close())

	sent := messages()
	require.Len(sent, 1)
	message := sent[0]
	assert.Equal("app_opened", message.Event)
	assert.Equal("anonymous-install-id", message.DistinctID)
	assert.Equal("roborev", message.Properties["application"])
	assert.NotContains(message.Properties, "pad")
	assert.NotContains(message.Properties, "repo_count")
}

func TestCaptureHandlerSendsSurfaceFromFixedList(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	t.Setenv(EnabledEnv, "1")
	t.Setenv(GenericEnabledEnv, "1")

	reporter, messages := newPostHogStubReporter(t)
	handler := NewCaptureHandler(reporter)

	tests := []struct {
		body        string
		wantSurface string
	}{
		{body: `{"event":"app_opened","properties":{"surface":"tui"}}`, wantSurface: "tui"},
		{body: `{"event":"app_opened","properties":{"surface":"web"}}`, wantSurface: "web"},
		{body: `{"event":"app_opened","properties":{"surface":"cli"}}`, wantSurface: "cli"},
		{body: `{"event":"app_opened","properties":{"surface":["tui"]}}`},
		{body: `{"event":"app_opened"}`},
		{body: `{"event":"app_opened","properties":{"surface":"tui","source":"web"}}`, wantSurface: "tui"},
	}
	for _, tt := range tests {
		recorder := postCaptureEvent(handler, tt.body)
		assert.Equal(http.StatusAccepted, recorder.Code, tt.body)
		assert.JSONEq(`{"status":"queued"}`, recorder.Body.String(), tt.body)
	}
	require.NoError(reporter.Close())

	sent := messages()
	require.Len(sent, len(tests))
	for i, message := range sent {
		assert.Equal(EventAppOpened, message.Event)
		assert.Equal("roborev", message.Properties["application"])
		assert.Equal("daemon", message.Properties["source"])
		if tests[i].wantSurface == "" {
			assert.NotContains(message.Properties, PropertySurface, tests[i].body)
			continue
		}
		assert.Equal(tests[i].wantSurface, message.Properties[PropertySurface], tests[i].body)
	}
}

type postHogWireMessage struct {
	Event      string         `json:"event"`
	DistinctID string         `json:"distinct_id"`
	Properties map[string]any `json:"properties"`
}

// newPostHogStubReporter returns an enabled reporter that sends to a loopback PostHog stub, and a reader for the batched messages it received.
func newPostHogStubReporter(t *testing.T) (*Reporter, func() []postHogWireMessage) {
	t.Helper()
	var (
		mu     sync.Mutex
		bodies [][]byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var reader io.Reader = r.Body
		if r.Header.Get("Content-Encoding") == "gzip" {
			gz, err := gzip.NewReader(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			defer gz.Close()
			reader = gz
		}
		body, err := io.ReadAll(reader)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":1}`))
	}))
	t.Cleanup(srv.Close)

	reporter, err := kittelemetry.NewPostHogReporter(kittelemetry.PostHogOptions{
		APIKey:      "test-posthog-api-key",
		Application: "roborev",
		EnvPrefix:   "ROBOREV",
		DistinctID:  "anonymous-install-id",
		Version:     "test-version",
		Source:      "daemon",
		Endpoint:    srv.URL,
	}, allowedEventOptions()...)
	require.NoError(t, err)

	return reporter, func() []postHogWireMessage {
		mu.Lock()
		defer mu.Unlock()
		var messages []postHogWireMessage
		for _, body := range bodies {
			var batch struct {
				Batch []postHogWireMessage `json:"batch"`
			}
			require.NoError(t, json.Unmarshal(body, &batch))
			messages = append(messages, batch.Batch...)
		}
		return messages
	}
}

func TestNewReporterOrDisabledErrorFallbackAdmitsNothing(t *testing.T) {
	t.Setenv(EnabledEnv, "1")
	t.Setenv(GenericEnabledEnv, "1")

	reporter := NewReporterOrDisabled(Options{})

	assert.False(t, reporter.Enabled())
	recorder := postCaptureEvent(NewCaptureHandler(reporter), `{"event":"app_opened"}`)
	assert.Equal(t, http.StatusBadRequest, recorder.Code)
}

func postCaptureEvent(handler http.Handler, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/api/telemetry/events", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}
