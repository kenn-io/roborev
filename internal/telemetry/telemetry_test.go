package telemetry

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/posthog/posthog-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
)

type fakePostHogClient struct {
	message posthog.Message
}

func (f *fakePostHogClient) Enqueue(message posthog.Message) error {
	f.message = message
	return nil
}

func (f *fakePostHogClient) Close() error { return nil }

func openTestDB(t *testing.T) *storage.DB {
	t.Helper()
	db, err := storage.Open(filepath.Join(t.TempDir(), "reviews.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return db
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
}

func TestNewReporterDisabledInTestProcessEvenWhenEnvEnablesTelemetry(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	t.Setenv(EnabledEnv, "1")
	t.Setenv(GenericEnabledEnv, "1")
	database := openTestDB(t)

	reporter, err := NewReporter(Options{Database: database})
	require.NoError(err)

	assert.False(reporter.Enabled())
	value, err := database.GetSyncState(installIDMetadataKey)
	require.NoError(err)
	assert.Empty(value)
}

func TestLoadOrCreateInstallIDIsStableAndAnonymous(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := openTestDB(t)

	first, err := loadOrCreateInstallID(database)
	require.NoError(err)
	second, err := loadOrCreateInstallID(database)
	require.NoError(err)

	assert.Len(first, 32)
	assert.Equal(first, second)

	stored, err := database.GetSyncState(installIDMetadataKey)
	require.NoError(err)
	assert.Equal(first, stored)
}

func TestReporterCaptureUsesAnonymousDistinctID(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	reporter := &Reporter{
		distinctID: "anonymous-install-id",
		version:    "test-version",
		enabled:    true,
	}

	capture, err := reporter.captureMessage(EventDaemonStarted, map[string]any{
		"$process_person_profile": true,
		"$geoip_disable":          false,
		"application":             "caller-app",
		"distinct_id":             "user-provided",
		"repo":                    "owner/name",
		"version":                 "caller-version",
		"repo_count":              3,
		"review_count":            7,
		"sync_enabled":            true,
	}, time.Now().UTC())
	require.NoError(err)

	assert.Equal("anonymous-install-id", capture.DistinctId)
	assert.Equal(EventDaemonStarted, capture.Event)
	assert.Equal(3, capture.Properties["repo_count"])
	assert.Equal(7, capture.Properties["review_count"])
	assert.Equal(true, capture.Properties["sync_enabled"])
	assert.NotContains(capture.Properties, "distinct_id")
	assert.NotContains(capture.Properties, "repo")
	assert.Equal("roborev", capture.Properties["application"])
	assert.Equal("test-version", capture.Properties["version"])
	assert.Equal("daemon", capture.Properties["source"])
	assert.NotEmpty(capture.Properties["goos"])
	assert.NotEmpty(capture.Properties["goarch"])
	assert.False(capture.Properties["$process_person_profile"].(bool))
	assert.True(capture.Properties["$geoip_disable"].(bool))
}

func TestReporterCaptureRejectsUnsupportedEvents(t *testing.T) {
	require := require.New(t)

	client := &fakePostHogClient{}
	reporter := &Reporter{
		client:     client,
		distinctID: "anonymous-install-id",
		enabled:    true,
	}

	err := reporter.Capture("repo_opened", map[string]any{"repo_count": 1})
	require.ErrorIs(err, ErrUnsupportedEvent)
}

func TestReporterCaptureDoesNotEnqueueInTestProcess(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	client := &fakePostHogClient{}
	reporter := &Reporter{
		client:     client,
		distinctID: "anonymous-install-id",
		version:    "test-version",
		enabled:    true,
	}

	err := reporter.Capture(EventDaemonActive, map[string]any{"repo_count": 1})
	require.NoError(err)
	assert.Nil(client.message)
}

func TestReporterCaptureAllowsDaemonActiveEvent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	reporter := &Reporter{
		distinctID: "anonymous-install-id",
		version:    "test-version",
		enabled:    true,
	}

	capture, err := reporter.captureMessage(EventDaemonActive, map[string]any{
		"repo_count":              3,
		"review_count":            7,
		"sync_enabled":            true,
		"worker_count":            4,
		"unknown_count":           5,
		"$process_person_profile": true,
		"$geoip_disable":          false,
		"application":             "caller-app",
		"version":                 "caller-version",
	}, time.Now().UTC())
	require.NoError(err)

	assert.Equal(EventDaemonActive, capture.Event)
	assert.Equal(3, capture.Properties["repo_count"])
	assert.Equal(7, capture.Properties["review_count"])
	assert.Equal(true, capture.Properties["sync_enabled"])
	assert.NotContains(capture.Properties, "worker_count")
	assert.NotContains(capture.Properties, "unknown_count")
	assert.Equal("roborev", capture.Properties["application"])
	assert.Equal("test-version", capture.Properties["version"])
	assert.Equal("daemon", capture.Properties["source"])
	assert.NotEmpty(capture.Properties["goos"])
	assert.NotEmpty(capture.Properties["goarch"])
	assert.False(capture.Properties["$process_person_profile"].(bool))
	assert.True(capture.Properties["$geoip_disable"].(bool))
}

func TestReporterCaptureDropsUnsafePropertyValues(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	reporter := &Reporter{
		distinctID: "anonymous-install-id",
		version:    "test-version",
		enabled:    true,
	}

	capture, err := reporter.captureMessage(EventDaemonStarted, map[string]any{
		"repo_count":              "owner/repo",
		"review_count":            "all of them",
		"sync_enabled":            "yes",
		"worker_count":            4,
		"$process_person_profile": true,
		"$geoip_disable":          false,
		"application":             "caller-app",
		"version":                 "caller-version",
	}, time.Now().UTC())
	require.NoError(err)

	assert.NotContains(capture.Properties, "repo_count")
	assert.NotContains(capture.Properties, "review_count")
	assert.NotContains(capture.Properties, "sync_enabled")
	assert.NotContains(capture.Properties, "worker_count")
	assert.Equal("roborev", capture.Properties["application"])
	assert.Equal("test-version", capture.Properties["version"])
	assert.False(capture.Properties["$process_person_profile"].(bool))
	assert.True(capture.Properties["$geoip_disable"].(bool))
}
