package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/fslink"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/telemetry"
	"go.kenn.io/roborev/internal/testutil"
	"go.kenn.io/roborev/internal/version"
)

func TestDaemonTelemetryScreenViewedUsesDatabaseClaimsPath(t *testing.T) {
	// Process-wide telemetry opt-out requires a separate test process.
	if os.Getenv("ROBOREV_TEST_DAEMON_TELEMETRY") != "1" {
		executable, err := os.Executable()
		require.NoError(t, err)
		cmd := exec.Command(executable, "-test.run=^TestDaemonTelemetryScreenViewedUsesDatabaseClaimsPath$")
		cmd.Env = append(os.Environ(), "ROBOREV_TEST_DAEMON_TELEMETRY=1")
		output, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", output)
		return
	}
	t.Setenv(telemetry.EnabledEnv, "1")
	t.Setenv(telemetry.GenericEnabledEnv, "1")
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	db, dir := testutil.OpenTestDBWithDir(t)
	owner, err := lockDaemonDatabase(filepath.Join(dir, "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, owner.Close()) })
	endpoint, _ := testutil.NewPostHogStub(t)
	reporter := telemetry.NewReporterOrDisabled(telemetry.Options{
		Database:        db,
		Version:         version.Version,
		DailyClaimsPath: telemetry.DailyClaimsPath(owner.path),
		Endpoint:        endpoint,
	})
	t.Cleanup(func() { require.NoError(t, reporter.Close()) })
	require.True(t, reporter.Enabled())
	server := daemon.NewServer(db, config.DefaultConfig(), "")
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	server.SetTelemetry(reporter)
	request := httptest.NewRequest(http.MethodPost, daemon.TelemetryEventsPath, strings.NewReader(`{"event":"screen_viewed","properties":{"screen":"queue","surface":"tui"}}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	assert.Equal(t, http.StatusAccepted, response.Code)
	assert.JSONEq(t, `{"status":"queued"}`, response.Body.String())
	assert.FileExists(t, telemetry.DailyClaimsPath(owner.path))
}

func TestDaemonDatabaseHasOneOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reviews.db")
	owner, err := lockDaemonDatabase(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, owner.Close()) })
	_, err = lockDaemonDatabase(path)
	require.ErrorContains(t, err, "already owned by a daemon")
	require.NoError(t, owner.Close())
	next, err := lockDaemonDatabase(path)
	require.NoError(t, err)
	require.NoError(t, next.Close())
}

func TestDaemonRunRejectsOtherOwnerBeforeMigration(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	path := storage.DefaultDBPath()
	db, err := storage.Open(path)
	require.NoError(t, err)
	_, err = db.Exec("DELETE FROM sync_state WHERE key = 'repo_names_from_identity'")
	require.NoError(t, err)
	require.NoError(t, db.Close())
	owner, err := lockDaemonDatabase(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, owner.Close()) })
	cmd := daemonRunCmd()
	cmd.SetArgs([]string{"--db", path})
	require.ErrorContains(t, cmd.Execute(), "already owned by a daemon")
	read, err := storage.OpenReadOnly(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, read.Close()) })
	marker, err := read.GetSyncState("repo_names_from_identity")
	require.NoError(t, err)
	require.Empty(t, marker, "rejected startup must not run the pending migration")
}

func TestDaemonDatabaseAliasesHaveOneOwner(t *testing.T) {
	for _, relative := range []bool{false, true} {
		t.Run(fmt.Sprint(relative), func(t *testing.T) {
			dir := t.TempDir()
			target := filepath.Join(dir, "reviews.db")
			alias := filepath.Join(t.TempDir(), "alias.db")
			destination := target
			if relative {
				var err error
				destination, err = filepath.Rel(filepath.Dir(alias), target)
				require.NoError(t, err)
			}
			if err := os.Symlink(destination, alias); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			owner, err := lockDaemonDatabase(alias)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, owner.Close()) })
			// macOS temp dirs sit behind the /tmp -> /private/tmp link.
			resolvedDir, err := filepath.EvalSymlinks(dir)
			require.NoError(t, err)
			require.Equal(t, telemetry.DailyClaimsPath(filepath.Join(resolvedDir, "reviews.db")), telemetry.DailyClaimsPath(owner.path))
			for _, created := range []bool{false, true} {
				if created {
					db, err := storage.Open(alias)
					require.NoError(t, err)
					require.NoError(t, db.Close())
				}
				other, err := lockDaemonDatabase(target)
				if other != nil {
					require.NoError(t, other.Close())
				}
				require.ErrorContains(t, err, "already owned by a daemon")
			}
		})
	}
}

func TestDaemonDatabaseDirectoryAliasesHaveOneOwner(t *testing.T) {
	dir := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	_, err := fslink.LinkDir(dir, alias)
	require.NoError(t, err)

	owner, err := lockDaemonDatabase(filepath.Join(alias, "reviews.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, owner.Close()) })

	other, err := lockDaemonDatabase(filepath.Join(dir, "reviews.db"))
	if other != nil {
		require.NoError(t, other.Close())
	}
	require.ErrorContains(t, err, "already owned by a daemon")
}
