package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/update"
	"go.kenn.io/roborev/internal/version"
	roborevclient "go.kenn.io/roborev/pkg/client"
)

func stubUpdateCommand(t *testing.T) *update.UpdateInfo {
	t.Helper()
	stubServiceRecovery(t)
	findDaemonService = func(context.Context, int) (*daemonService, error) { return nil, nil }
	getAnyRunningDaemonForStart = func(context.Context) (*daemon.RuntimeInfo, error) { return getAnyRunningDaemon() }
	originalCheck := checkForUpdateForCommand
	originalPerform := performUpdateForCommand
	originalPrepare := prepareUpdateDaemonForCommand
	originalRestart := restartUpdatedDaemonForCommand
	originalHooks := repairHooksForUpdateCommand
	originalSkills := updateSkillsForUpdateCommand
	originalSkillsInstalled := installedSkillsForUpdateCommand
	originalWaitLegacy := waitLegacyDaemonExitForCommand
	originalDiscover := getAnyRunningDaemon
	originalList := listAllRuntimes
	originalVerbose := verbose
	t.Cleanup(func() {
		checkForUpdateForCommand = originalCheck
		performUpdateForCommand = originalPerform
		prepareUpdateDaemonForCommand = originalPrepare
		restartUpdatedDaemonForCommand = originalRestart
		repairHooksForUpdateCommand = originalHooks
		updateSkillsForUpdateCommand = originalSkills
		installedSkillsForUpdateCommand = originalSkillsInstalled
		waitLegacyDaemonExitForCommand = originalWaitLegacy
		getAnyRunningDaemon = originalDiscover
		listAllRuntimes = originalList
		verbose = originalVerbose
	})
	verbose = false
	info := &update.UpdateInfo{
		CurrentVersion: "v1.0.0",
		LatestVersion:  "v1.1.0",
		AssetName:      "roborev_1.1.0_darwin_arm64.tar.gz",
		Size:           100,
		Checksum:       strings.Repeat("a", 64),
	}
	checkForUpdateForCommand = func(bool) (*update.UpdateInfo, error) { return info, nil }
	performUpdateForCommand = func(ctx context.Context, got *update.UpdateInfo, reporter update.Reporter) error {
		require.Same(t, info, got)
		reporter.Progress(got.Size, got.Size)
		return nil
	}
	repairHooksForUpdateCommand = func(context.Context, string, repairHookRunner) error { return nil }
	updateSkillsForUpdateCommand = func(context.Context, string) error { return nil }
	installedSkillsForUpdateCommand = func() bool { return true }
	waitLegacyDaemonExitForCommand = func(context.Context, int) error { return nil }
	listAllRuntimes = func() ([]*daemon.RuntimeInfo, error) { return nil, nil }
	return info
}

func TestUpdateRunningFlag(t *testing.T) {
	cmd := updateCmd()
	flag := cmd.Flags().Lookup("running")
	require.NotNil(t, flag)
	assert.Empty(t, flag.DefValue)
	assert.Contains(t, flag.Usage, "wait, interrupt, or abort")
}

func TestParseRunningReviewPolicy(t *testing.T) {
	for _, value := range []string{"", "wait", "interrupt", "abort"} {
		policy, err := parseRunningReviewPolicy(value)
		require.NoError(t, err)
		assert.Equal(t, runningReviewPolicy(value), policy)
	}
	_, err := parseRunningReviewPolicy("cancel")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "wait, interrupt, or abort")
}

func TestChooseRunningReviewPolicy(t *testing.T) {
	for _, tc := range []struct {
		answer    string
		want      runningReviewPolicy
		confirmed bool
	}{
		{answer: "w\n", want: policyWait, confirmed: true},
		{answer: "u\n", want: policyInterrupt, confirmed: true},
		{answer: "a\n"},
		{answer: "\n"},
	} {
		t.Run(strings.TrimSpace(tc.answer), func(t *testing.T) {
			var out bytes.Buffer
			policy, confirmed, err := chooseRunningReviewPolicy(
				strings.NewReader(tc.answer), &out, 3,
			)
			require.NoError(t, err)
			assert.Equal(t, tc.want, policy)
			assert.Equal(t, tc.confirmed, confirmed)
			assert.Contains(t, out.String(), "3 reviews are currently running")
			assert.Contains(t, out.String(), "[u] Update now")
		})
	}
}

func TestCommandUpdateReporterTerminatesProgressLine(t *testing.T) {
	var out bytes.Buffer
	reporter := &commandUpdateReporter{out: &out}
	reporter.Progress(50, 100)
	reporter.Finish(100, true)
	printUpdatePhase(&out, "Installing", "done")

	assert.Contains(t, out.String(), "\rDownloading  50%")
	assert.Contains(t, out.String(), "\nInstalling   done\n")
}

func TestPrintUpdateSummaryHidesVerboseFields(t *testing.T) {
	info := &update.UpdateInfo{
		CurrentVersion: "v1.0.0",
		LatestVersion:  "v1.1.0",
		AssetName:      "roborev_1.1.0_darwin_arm64.tar.gz",
		Size:           1024,
		DownloadURL:    "https://downloads.example/roborev.tar.gz",
		Checksum:       strings.Repeat("a", 64),
	}
	var out bytes.Buffer
	printUpdateSummary(&out, info, "/tmp/bin")

	assert.Contains(t, out.String(), "Version  v1.0.0 -> v1.1.0")
	assert.NotContains(t, out.String(), "SHA256")
	assert.NotContains(t, out.String(), "https://")
}

func TestUpdateCommandNoDaemonPrintsStablePhaseOrder(t *testing.T) {
	info := stubUpdateCommand(t)
	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) {
		return nil, errors.New("not running")
	}
	var out bytes.Buffer
	cmd := updateCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--yes"})

	require.NoError(t, cmd.Execute())
	output := out.String()
	assert.Contains(t, output, "Downloading  100%")
	assert.Contains(t, output, "Installing   done")
	assert.Contains(t, output, "Daemon       not running")
	assert.Contains(t, output, "Updated roborev to "+info.LatestVersion)
	assert.Less(t, strings.Index(output, "Daemon       not running"), strings.Index(output, "Git hooks"))
	assert.Less(t, strings.Index(output, "Git hooks"), strings.Index(output, "Skills"))
}

func TestUpdateCommandInteractiveAbortReturnsSuccess(t *testing.T) {
	stubUpdateCommand(t)
	var prepareCalls atomic.Int32
	endpoint := updateTestEndpoint(t, updateDaemonHandler(2, &prepareCalls, nil, nil))
	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) {
		return &daemon.RuntimeInfo{PID: 42, Network: endpoint.Network, Address: endpoint.Address}, nil
	}
	var out bytes.Buffer
	cmd := updateCmd()
	cmd.SetIn(strings.NewReader("a\n"))
	cmd.SetOut(&out)
	cmd.SetErr(&out)

	require.NoError(t, cmd.Execute())
	assert.Contains(t, out.String(), "Update cancelled")
	assert.Equal(t, int32(0), prepareCalls.Load())
}

func TestUpdateCommandExplicitAbortBusyIsNonzero(t *testing.T) {
	stubUpdateCommand(t)
	var prepareCalls atomic.Int32
	endpoint := updateTestEndpoint(t, updateDaemonHandler(2, &prepareCalls, nil, nil))
	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) {
		return &daemon.RuntimeInfo{PID: 42, Network: endpoint.Network, Address: endpoint.Address}, nil
	}
	cmd := updateCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--yes", "--running=abort"})

	err := cmd.Execute()

	require.Error(t, err)
	require.ErrorIs(t, err, errUpdateReviewsRunning)
	assert.Equal(t, int32(1), prepareCalls.Load())
}

func TestUpdateCommandReportsRunningReviewWait(t *testing.T) {
	stubUpdateCommand(t)
	oldPoll := updateDrainPollInterval
	updateDrainPollInterval = time.Millisecond
	t.Cleanup(func() { updateDrainPollInterval = oldPoll })
	var renewCalls atomic.Int32
	endpoint := updateTestEndpoint(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/status":
			_, _ = io.WriteString(w, `{"running_jobs":1}`)
		case "/api/update/prepare":
			_, _ = io.WriteString(w, `{"lease_token":"lease-1","policy":"wait","expires_at":"2030-01-01T00:00:00Z","running_jobs":1,"targeted_running_jobs":1,"active_workers":1,"recovering":false}`)
		case "/api/update/renew":
			running := 1
			if renewCalls.Add(1) > 1 {
				running = 0
			}
			_, _ = fmt.Fprintf(w, `{"lease_token":"lease-1","policy":"wait","expires_at":"2030-01-01T00:00:00Z","running_jobs":%d,"targeted_running_jobs":%d,"active_workers":%d,"recovering":false}`, running, running, running)
		case "/api/shutdown":
			_, _ = io.WriteString(w, `{"status":"shutting down"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) {
		return &daemon.RuntimeInfo{PID: 42, Network: endpoint.Network, Address: endpoint.Address}, nil
	}
	restartUpdatedDaemonForCommand = func(
		context.Context, string, string, *daemon.RuntimeInfo, *daemonService,
	) error {
		return nil
	}
	var out bytes.Buffer
	cmd := updateCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--yes"})

	require.NoError(t, cmd.Execute())
	assert.Contains(t, out.String(), "Daemon       waiting for 1 running review")
	assert.Contains(t, out.String(), "running review\n")
}

func TestUpdateCommandCoordinatesDaemonThatAppearsBeforeInstall(t *testing.T) {
	stubUpdateCommand(t)
	var discoveries atomic.Int32
	var prepareCalls atomic.Int32
	var restartCalls atomic.Int32
	endpoint := updateTestEndpoint(t, updateDaemonHandler(0, &prepareCalls, nil, nil))
	runtimeInfo := &daemon.RuntimeInfo{PID: 42, Network: endpoint.Network, Address: endpoint.Address}
	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) {
		if discoveries.Add(1) == 1 {
			return nil, errors.New("not running")
		}
		return runtimeInfo, nil
	}
	restartUpdatedDaemonForCommand = func(
		_ context.Context, _ string, _ string, previous *daemon.RuntimeInfo, _ *daemonService,
	) error {
		restartCalls.Add(1)
		assert.Same(t, runtimeInfo, previous)
		return nil
	}
	cmd := updateCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--yes"})

	require.NoError(t, cmd.Execute())
	assert.Equal(t, int32(1), prepareCalls.Load())
	assert.Equal(t, int32(1), restartCalls.Load())
}

func TestUpdateCommandCoordinatesDaemonThatAppearsAfterInstall(t *testing.T) {
	stubUpdateCommand(t)
	var discoveries atomic.Int32
	var prepareCalls atomic.Int32
	var restartCalls atomic.Int32
	endpoint := updateTestEndpoint(t, updateDaemonHandler(0, &prepareCalls, nil, nil))
	runtimeInfo := &daemon.RuntimeInfo{PID: 42, Network: endpoint.Network, Address: endpoint.Address}
	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) {
		if discoveries.Add(1) < 3 {
			return nil, errors.New("not running")
		}
		return runtimeInfo, nil
	}
	restartUpdatedDaemonForCommand = func(
		_ context.Context, _ string, _ string, previous *daemon.RuntimeInfo, _ *daemonService,
	) error {
		restartCalls.Add(1)
		assert.Same(t, runtimeInfo, previous)
		return nil
	}
	cmd := updateCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--yes"})

	require.NoError(t, cmd.Execute())
	assert.Equal(t, int32(1), prepareCalls.Load())
	assert.Equal(t, int32(1), restartCalls.Load())
}

func TestUpdateCommandCancelDuringHookRepairSuppressesSuccess(t *testing.T) {
	stubUpdateCommand(t)
	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) {
		return nil, errors.New("not running")
	}
	ctx, cancel := context.WithCancel(t.Context())
	repairHooksForUpdateCommand = func(context.Context, string, repairHookRunner) error {
		cancel()
		return nil
	}
	var skillCalls atomic.Int32
	updateSkillsForUpdateCommand = func(context.Context, string) error {
		skillCalls.Add(1)
		return nil
	}
	var out bytes.Buffer
	cmd := updateCmd()
	cmd.SetContext(ctx)
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--yes"})

	err := cmd.Execute()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "binary installed")
	assert.Equal(t, int32(0), skillCalls.Load())
	assert.NotContains(t, out.String(), "Updated roborev to")
}

func TestDiscoverDaemonForUpdateRemovesDeadRuntime(t *testing.T) {
	stubUpdateCommand(t)
	oldAlive := isPIDAliveForUpdate
	t.Cleanup(func() { isPIDAliveForUpdate = oldAlive })
	runtimePath := filepath.Join(t.TempDir(), "daemon.json")
	require.NoError(t, os.WriteFile(runtimePath, []byte("{}"), 0o600))
	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) {
		return nil, errors.New("probe failed")
	}
	listAllRuntimes = func() ([]*daemon.RuntimeInfo, error) {
		return []*daemon.RuntimeInfo{{PID: 42, SourcePath: runtimePath}}, nil
	}
	isPIDAliveForUpdate = func(int) bool { return false }

	info, err := discoverDaemonForUpdate()

	require.NoError(t, err)
	assert.Nil(t, info)
	_, err = os.Stat(runtimePath)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestUpdateCommandCancelAfterInstallReleasesLease(t *testing.T) {
	stubUpdateCommand(t)
	var releaseCalls atomic.Int32
	var shutdownCalls atomic.Int32
	endpoint := updateTestEndpoint(t, updateDaemonHandler(0, nil, &releaseCalls, &shutdownCalls))
	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) {
		return &daemon.RuntimeInfo{PID: 42, Network: endpoint.Network, Address: endpoint.Address}, nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	performUpdateForCommand = func(context.Context, *update.UpdateInfo, update.Reporter) error {
		cancel()
		return nil
	}
	var out bytes.Buffer
	cmd := updateCmd()
	cmd.SetContext(ctx)
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--yes"})

	err := cmd.Execute()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "binary installed; daemon still running old version — run roborev daemon restart")
	assert.Equal(t, int32(1), releaseCalls.Load())
	assert.Equal(t, int32(0), shutdownCalls.Load())
	assert.NotContains(t, out.String(), "Updated roborev to")
}

func TestUpdateCommandCancelAfterShutdownReportsFinishingState(t *testing.T) {
	stubUpdateCommand(t)
	var releaseCalls atomic.Int32
	var shutdownCalls atomic.Int32
	endpoint := updateTestEndpoint(t, updateDaemonHandler(0, nil, &releaseCalls, &shutdownCalls))
	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) {
		return &daemon.RuntimeInfo{PID: 42, Network: endpoint.Network, Address: endpoint.Address}, nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	restartUpdatedDaemonForCommand = func(
		context.Context, string, string, *daemon.RuntimeInfo, *daemonService,
	) error {
		cancel()
		return context.Canceled
	}
	var out bytes.Buffer
	cmd := updateCmd()
	cmd.SetContext(ctx)
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--yes"})

	err := cmd.Execute()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "binary installed; daemon is finishing shutdown — run roborev daemon restart")
	assert.Equal(t, int32(0), releaseCalls.Load())
	assert.Equal(t, int32(1), shutdownCalls.Load())
	assert.NotContains(t, out.String(), "Updated roborev to")
}

func TestUpdateCommandRestartVerificationFailureSuppressesSuccess(t *testing.T) {
	stubUpdateCommand(t)
	endpoint := updateTestEndpoint(t, updateDaemonHandler(0, nil, nil, nil))
	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) {
		return &daemon.RuntimeInfo{PID: 42, Network: endpoint.Network, Address: endpoint.Address}, nil
	}
	restartUpdatedDaemonForCommand = func(
		context.Context, string, string, *daemon.RuntimeInfo, *daemonService,
	) error {
		return errors.New("daemon version mismatch")
	}
	var out bytes.Buffer
	cmd := updateCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--yes"})

	err := cmd.Execute()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "daemon version mismatch")
	assert.NotContains(t, out.String(), "Updated roborev to")
}

func TestUpdateCommandLegacyWaitStillVerifiesReplacement(t *testing.T) {
	info := stubUpdateCommand(t)
	var restartCalls atomic.Int32
	var waitCalls atomic.Int32
	endpoint := updateTestEndpoint(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/status":
			_, _ = io.WriteString(w, `{"running_jobs":0}`)
		case "/api/update/prepare":
			http.NotFound(w, r)
		case "/api/shutdown":
			_, _ = io.WriteString(w, `{"status":"shutting down"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	runtimeInfo := &daemon.RuntimeInfo{PID: 42, Network: endpoint.Network, Address: endpoint.Address}
	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) { return runtimeInfo, nil }
	waitLegacyDaemonExitForCommand = func(_ context.Context, pid int) error {
		waitCalls.Add(1)
		assert.Equal(t, runtimeInfo.PID, pid)
		return nil
	}
	restartUpdatedDaemonForCommand = func(
		_ context.Context, _ string, version string, previous *daemon.RuntimeInfo, _ *daemonService,
	) error {
		restartCalls.Add(1)
		assert.Equal(t, info.LatestVersion, version)
		assert.Same(t, runtimeInfo, previous)
		return nil
	}
	var out bytes.Buffer
	cmd := updateCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--yes", "--running=wait"})

	require.NoError(t, cmd.Execute())
	assert.Equal(t, int32(1), waitCalls.Load())
	assert.Equal(t, int32(1), restartCalls.Load())
	assert.Contains(t, out.String(), "compatibility mode")
	assert.Contains(t, out.String(), "Daemon       restarted ("+info.LatestVersion+")")
}

func TestUpdateNoRestartSkipsDaemonDiscovery(t *testing.T) {
	stubUpdateCommand(t)
	var discoverCalls atomic.Int32
	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) {
		discoverCalls.Add(1)
		return nil, errors.New("not running")
	}
	var out bytes.Buffer
	cmd := updateCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--yes", "--no-restart"})

	require.NoError(t, cmd.Execute())
	assert.Equal(t, int32(0), discoverCalls.Load())
	assert.Contains(t, out.String(), "Daemon       skipped (--no-restart)")
}

func TestUpdateCheckReturnsBeforeDaemonDiscovery(t *testing.T) {
	stubUpdateCommand(t)
	var discoverCalls atomic.Int32
	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) {
		discoverCalls.Add(1)
		return nil, errors.New("not running")
	}
	cmd := updateCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--check"})

	require.NoError(t, cmd.Execute())
	assert.Equal(t, int32(0), discoverCalls.Load())
}

func stubDistribution(t *testing.T, distribution string) {
	t.Helper()
	original := version.Distribution
	version.Distribution = distribution
	t.Cleanup(func() { version.Distribution = original })
}

func TestUpdateCommandRefusesPackageManagedBuild(t *testing.T) {
	for _, args := range [][]string{{"--yes"}, {"--yes", "--force"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			stubUpdateCommand(t)
			stubDistribution(t, "homebrew")
			var checks, installs atomic.Int32
			checkForUpdateForCommand = func(bool) (*update.UpdateInfo, error) {
				checks.Add(1)
				return nil, errors.New("unexpected update check")
			}
			performUpdateForCommand = func(context.Context, *update.UpdateInfo, update.Reporter) error {
				installs.Add(1)
				return errors.New("unexpected install")
			}
			var out bytes.Buffer
			cmd := updateCmd()
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			cmd.SetArgs(args)

			err := cmd.Execute()
			require.EqualError(t, err, "self-update is disabled in this build: Homebrew manages this installation; use 'brew upgrade roborev' to update")
			assert.Equal(t, int32(0), checks.Load())
			assert.Equal(t, int32(0), installs.Load())
		})
	}
}

func TestUpdateCheckInPackageManagedBuildPointsToPackageManager(t *testing.T) {
	stubUpdateCommand(t)
	stubDistribution(t, "homebrew")
	var installs atomic.Int32
	performUpdateForCommand = func(context.Context, *update.UpdateInfo, update.Reporter) error {
		installs.Add(1)
		return errors.New("unexpected install")
	}
	var out bytes.Buffer
	cmd := updateCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--check"})

	require.NoError(t, cmd.Execute())
	assert.Equal(t, int32(0), installs.Load())
	assert.Contains(t, out.String(), "Version  v1.0.0 -> v1.1.0")
	assert.Contains(t, out.String(), "use 'brew upgrade roborev' to update")
	assert.NotContains(t, out.String(), "--force")
}

// Official direct downloads ("kenn") update themselves, like builds with no
// Distribution.
func TestUpdateCommandInstallsSelfUpdatingBuild(t *testing.T) {
	for _, distribution := range []string{"", "kenn"} {
		t.Run("distribution="+distribution, func(t *testing.T) {
			info := stubUpdateCommand(t)
			stubDistribution(t, distribution)
			getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) {
				return nil, errors.New("not running")
			}
			var installs atomic.Int32
			performUpdateForCommand = func(_ context.Context, got *update.UpdateInfo, reporter update.Reporter) error {
				installs.Add(1)
				reporter.Progress(got.Size, got.Size)
				return nil
			}
			var out bytes.Buffer
			cmd := updateCmd()
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			cmd.SetArgs([]string{"--yes"})

			require.NoError(t, cmd.Execute())
			assert.Equal(t, int32(1), installs.Load())
			assert.Contains(t, out.String(), "Updated roborev to "+info.LatestVersion)
		})
	}
}

func TestUpdateCheckInSelfUpdatingBuildShowsNoPackageGuidance(t *testing.T) {
	for _, distribution := range []string{"", "kenn"} {
		t.Run("distribution="+distribution, func(t *testing.T) {
			stubUpdateCommand(t)
			stubDistribution(t, distribution)
			var out bytes.Buffer
			cmd := updateCmd()
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			cmd.SetArgs([]string{"--check"})

			require.NoError(t, cmd.Execute())
			assert.Contains(t, out.String(), "Version  v1.0.0 -> v1.1.0")
			assert.Contains(t, out.String(), "Install  ")
			assert.NotContains(t, out.String(), "brew")
			assert.NotContains(t, out.String(), "manages this installation")
		})
	}
}

func updateDaemonHandler(
	running int,
	prepareCalls, releaseCalls, shutdownCalls *atomic.Int32,
) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/status":
			_, _ = io.WriteString(w, `{"running_jobs":`+strconv.Itoa(running)+`}`)
		case "/api/update/prepare":
			if prepareCalls != nil {
				prepareCalls.Add(1)
			}
			if running > 0 && strings.Contains(readRequestBody(r), `"policy":"abort"`) {
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(http.StatusConflict)
				_, _ = io.WriteString(w, `{"title":"Conflict","status":409,"detail":"reviews are running"}`)
				return
			}
			_, _ = io.WriteString(w, `{"lease_token":"lease-1","policy":"wait","expires_at":"2030-01-01T00:00:00Z","running_jobs":0,"targeted_running_jobs":0,"active_workers":0,"recovering":false}`)
		case "/api/update/renew":
			_, _ = io.WriteString(w, `{"lease_token":"lease-1","policy":"wait","expires_at":"2030-01-01T00:00:00Z","running_jobs":0,"targeted_running_jobs":0,"active_workers":0,"recovering":false}`)
		case "/api/update/release":
			if releaseCalls != nil {
				releaseCalls.Add(1)
			}
			_, _ = io.WriteString(w, `{"released":true}`)
		case "/api/shutdown":
			if shutdownCalls != nil {
				shutdownCalls.Add(1)
			}
			_, _ = io.WriteString(w, `{"status":"shutting down"}`)
		default:
			http.NotFound(w, r)
		}
	})
}

func readRequestBody(r *http.Request) string {
	body, _ := io.ReadAll(r.Body)
	return string(body)
}

func TestUpdateCommandChecksServiceBeforeShutdown(t *testing.T) {
	for _, outcome := range []string{"managed", "lookup before install", "lookup after install", "owner lost", "owner changed", "child", "foreign", "unsupported"} {
		t.Run(outcome, func(t *testing.T) {
			info := stubUpdateCommand(t)
			binDir := t.TempDir()
			target := updatedDaemonPath(binDir)
			require.NoError(t, os.WriteFile(target, []byte("old binary"), 0o700))
			var releaseCalls, shutdownCalls atomic.Int32
			endpoint := updateTestEndpoint(t, updateDaemonHandler(0, nil, &releaseCalls, &shutdownCalls))
			current := &daemon.RuntimeInfo{PID: 42, Network: endpoint.Network, Address: endpoint.Address}
			getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) { return current, nil }
			owner := &daemonService{unit: "fixture.service", executable: target, mainPID: 42, serviceType: "simple"}
			switch outcome {
			case "child":
				owner.mainPID = 41
			case "foreign":
				owner.executable = filepath.Join(t.TempDir(), "other")
				require.NoError(t, os.WriteFile(owner.executable, nil, 0o700))
			case "unsupported":
				owner.remainAfterExit = true
			}
			installed, lookups, restarts := false, 0, 0
			findDaemonService = func(context.Context, int) (*daemonService, error) {
				lookups++
				if outcome == "lookup before install" || outcome == "lookup after install" && installed {
					return nil, errors.New("owner unavailable")
				}
				if installed && outcome == "owner lost" {
					return nil, nil
				}
				if installed && outcome == "owner changed" {
					changed := *owner
					changed.unit = "replacement.service"
					return &changed, nil
				}
				return owner, nil
			}
			performUpdateForCommand = func(context.Context, *update.UpdateInfo, update.Reporter) error { installed = true; return nil }
			restartUpdatedDaemonForCommand = func(_ context.Context, _ string, _ string, previous *daemon.RuntimeInfo, got *daemonService) error {
				restarts++
				assert.Same(t, current, previous)
				assert.Same(t, owner, got)
				return nil
			}
			var out bytes.Buffer
			err := runControlledUpdate(t.Context(), strings.NewReader(""), &out, info, binDir, policyWait, true, false)
			if outcome == "managed" {
				require.NoError(t, err)
				assert.True(t, installed)
				assert.Equal(t, 2, lookups)
				assert.Equal(t, int32(1), shutdownCalls.Load())
				assert.Equal(t, 1, restarts)
			} else {
				require.Error(t, err)
				assert.Zero(t, shutdownCalls.Load())
				assert.Zero(t, restarts)
				if outcome == "unsupported" {
					assert.EqualError(t, err, serviceDescription(owner.unit)+": RemainAfterExit=yes requires manual restart; run "+serviceRestartHint(owner.unit))
				}
				if outcome == "lookup after install" || outcome == "owner lost" || outcome == "owner changed" {
					assert.True(t, installed)
					assert.Equal(t, int32(1), releaseCalls.Load())
				} else {
					assert.False(t, installed)
				}
			}
		})
	}
}

// updateCommandTransport runs the protocol in process so synctest controls all
// lease timers without a real socket keeping virtual time from advancing.
type updateCommandTransport struct{ handler http.Handler }

func (transport updateCommandTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	response := httptest.NewRecorder()
	transport.handler.ServeHTTP(response, r)
	return response.Result(), nil
}

func TestUpdateKeepsLeaseDuringFinalOwnershipLookup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		info := stubUpdateCommand(t)
		binDir := t.TempDir()
		target := updatedDaemonPath(binDir)
		require.NoError(t, os.WriteFile(target, nil, 0o700))
		current := &daemon.RuntimeInfo{PID: 42, Network: "tcp", Address: "127.0.0.1:12345"}
		getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) { return current, nil }
		var installed atomic.Bool
		var lastRenew atomic.Int64
		lastRenew.Store(time.Now().UnixNano())
		var renewalsDuringLookup atomic.Int32
		baseHandler := updateDaemonHandler(0, nil, nil, nil)
		transport := updateCommandTransport{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/update/renew" {
				lastRenew.Store(time.Now().UnixNano())
				if installed.Load() {
					renewalsDuringLookup.Add(1)
				}
			}
			baseHandler.ServeHTTP(w, r)
		})}
		oldAPI := newUpdateAPI
		t.Cleanup(func() { newUpdateAPI = oldAPI })
		newUpdateAPI = func(endpoint daemon.DaemonEndpoint) (*roborevclient.Client, error) {
			return roborevclient.NewWithHTTPClient(endpoint.BaseURL(), &http.Client{Transport: transport})
		}
		owner := &daemonService{unit: "fixture.service", executable: target, mainPID: current.PID, serviceType: "simple"}
		findDaemonService = func(context.Context, int) (*daemonService, error) {
			if installed.Load() {
				// The daemon expires its update lease after 60 seconds. This
				// lookup exceeds that lifetime without changing the lookup budget.
				time.Sleep(61 * time.Second)
				assert.Less(t, time.Since(time.Unix(0, lastRenew.Load())), 60*time.Second, "claim gate must remain held")
				assert.Positive(t, renewalsDuringLookup.Load())
			}
			return owner, nil
		}
		performUpdateForCommand = func(context.Context, *update.UpdateInfo, update.Reporter) error {
			installed.Store(true)
			return nil
		}
		restartUpdatedDaemonForCommand = func(context.Context, string, string, *daemon.RuntimeInfo, *daemonService) error { return nil }
		err := runControlledUpdate(t.Context(), strings.NewReader(""), io.Discard, info, binDir, policyWait, true, false)
		require.NoError(t, err)
	})
}

func TestUpdateReleasesRecoveryLockBeforeHookRepair(t *testing.T) {
	info := stubUpdateCommand(t)
	endpoint := updateTestEndpoint(t, updateDaemonHandler(0, nil, nil, nil))
	current := &daemon.RuntimeInfo{PID: 42, Network: endpoint.Network, Address: endpoint.Address}
	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) { return current, nil }
	restartUpdatedDaemonForCommand = func(context.Context, string, string, *daemon.RuntimeInfo, *daemonService) error { return nil }
	hookCalls := 0
	repairHooksForUpdateCommand = func(ctx context.Context, _ string, _ repairHookRunner) error {
		hookCalls++
		// Hook repair can ensure/recover the daemon in its subprocess. Its
		// parent must relinquish the start lock before waiting for that work.
		release, acquired, err := daemon.RuntimeStore().TryAcquireStartLock(ctx)
		require.NoError(t, err)
		assert.True(t, acquired)
		if acquired {
			release()
		}
		return nil
	}
	require.NoError(t, runControlledUpdate(t.Context(), strings.NewReader(""), io.Discard, info, t.TempDir(), policyWait, true, false))
	assert.Equal(t, 1, hookCalls)
}
