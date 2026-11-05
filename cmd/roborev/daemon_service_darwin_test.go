package main

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/version"
)

func TestRecoverLaunchdDaemonKickstartsAfterDrainThenPrints(t *testing.T) {
	check := assert.New(t)
	stubServiceRecovery(t)
	oldRun := runLaunchctl
	t.Cleanup(func() { runLaunchctl = oldRun })
	path, err := os.Executable()
	require.NoError(t, err)
	owner := &daemonService{unit: "gui/501/com.example.daemon", executable: path, mainPID: 42, serviceType: "simple"}
	findDaemonService = func(context.Context, int) (*daemonService, error) { return owner, nil }
	current := &daemon.RuntimeInfo{PID: 42, Version: "v-old"}
	getAnyRunningDaemonForStart = func(context.Context) (*daemon.RuntimeInfo, error) { return current, nil }
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
	check.Equal(1, drains)
	check.Equal([][]string{{"kickstart", owner.unit}, {"print", owner.unit}}, calls)
}

func TestLaunchdDiscoveryUnsupportedServiceUnitIsUnknown(t *testing.T) {
	unit, err := processServiceUnit(os.Getpid())
	check := assert.New(t)
	check.Empty(unit)
	require.ErrorIs(t, err, errors.ErrUnsupported)
	check.False(discoveredOutsideService(os.Getpid()))
}
