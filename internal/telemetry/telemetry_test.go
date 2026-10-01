package telemetry

import (
	"path/filepath"
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

func TestLoadOrCreateInstallTreatsExistingIDWithoutTimeAsEstablished(t *testing.T) {
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
}
