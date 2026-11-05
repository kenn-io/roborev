package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/version"
)

func TestRecoverLaunchdDaemon(t *testing.T) {
	for _, name := range []string{"direct", "wrapper child", "another installation", "unreadable job"} {
		t.Run(name, func(t *testing.T) {
			check := assert.New(t)
			stubServiceRecovery(t)
			oldRun := runLaunchctl
			t.Cleanup(func() { runLaunchctl = oldRun })
			path, err := os.Executable()
			require.NoError(t, err)
			owner := &daemonService{unit: "gui/501/com.example.daemon", executable: path, mainPID: 42, serviceType: "simple"}
			switch name {
			case "wrapper child":
				owner.mainPID = 41
			case "another installation":
				owner.executable = filepath.Join(t.TempDir(), "roborev")
				require.NoError(t, os.WriteFile(owner.executable, []byte("foreign"), 0o700))
			}
			unreadable := errors.New("job unreadable")
			findDaemonService = func(context.Context, int) (*daemonService, error) {
				if name == "unreadable job" {
					return nil, &managedRecoveryError{owner.unit, unreadable}
				}
				return owner, nil
			}
			current := &daemon.RuntimeInfo{PID: 42, Version: "v-old"}
			getAnyRunningDaemonForStart = func(context.Context) (*daemon.RuntimeInfo, error) { return current, nil }
			var output bytes.Buffer
			lifecycleOut = &output
			drains, detached := 0, 0
			drainManagedDaemon = func(info *daemon.RuntimeInfo) error { check.Equal(42, info.PID); drains++; return nil }
			startDaemonForEnsure = func() error { detached++; return nil }
			var calls [][]string
			runLaunchctl = func(ctx context.Context, args ...string) ([]byte, error) {
				calls = append(calls, args)
				if args[0] == "kickstart" {
					check.Equal(1, drains)
					current = &daemon.RuntimeInfo{PID: 43, Version: version.Version}
					return nil, nil
				}
				return []byte("\ttype = LaunchAgent\n\tpid = 43\n"), nil
			}
			startDaemonService = startUserService
			serviceMainPID = userServiceMainPID
			require.NoError(t, recoverDaemon(current, nil, false))
			check.Zero(detached)
			if name == "direct" {
				check.Equal(1, drains)
				check.Equal([][]string{{"kickstart", owner.unit}, {"print", owner.unit}}, calls)
			} else {
				check.Zero(drains)
				check.Empty(calls)
				check.Contains(output.String(), "recovery skipped")
				if name == "unreadable job" {
					check.Contains(output.String(), unreadable.Error())
				}
			}
		})
	}
}

func TestLaunchdDiscoveryDoesNotRunLaunchctl(t *testing.T) {
	oldRun := runLaunchctl
	t.Cleanup(func() { runLaunchctl = oldRun })
	calls := 0
	runLaunchctl = func(context.Context, ...string) ([]byte, error) { calls++; return nil, nil }
	unit, err := processServiceUnit(os.Getpid())
	check := assert.New(t)
	check.Empty(unit)
	check.ErrorIs(err, errors.ErrUnsupported)
	check.False(discoveredOutsideService(os.Getpid()))
	check.Zero(calls)
}
