package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"go.kenn.io/roborev/internal/daemon"
)

func stubLaunchd(t *testing.T) {
	t.Helper()
	oldRun, oldArgs, oldProcess, oldEUID := runLaunchctl, readLaunchdProcArgs, readLaunchdProcess, launchdEUID
	t.Cleanup(func() {
		runLaunchctl, readLaunchdProcArgs, readLaunchdProcess, launchdEUID = oldRun, oldArgs, oldProcess, oldEUID
	})
}

func TestLookupLaunchdService(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		gui, user, system     int
		parent                int
		wantDomain, wantError string
	}{
		{name: "main", gui: 42, wantDomain: "gui"},
		{name: "wrapper child", gui: 40, parent: 40, wantDomain: "gui"},
		{name: "inherited detached", gui: 40},
		{name: "unloaded label", gui: -1, user: -1, system: -1},
		{name: "user after wrong gui", gui: 77, user: 42, wantDomain: "user"},
		{name: "system after inactive gui", system: 42, wantDomain: "system"},
		{name: "user after unreadable gui", gui: -2, user: 42, wantDomain: "user"},
		{name: "unknown domain", gui: -2, wantError: "print rejected"},
		{name: "malformed output", gui: -3, wantError: "state or program unavailable"},
		{name: "vanished process", parent: -1, wantError: "file does not exist"},
		{name: "parent cycle", gui: 40, parent: 42, wantError: "cycle"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubLaunchd(t)
			readLaunchdProcArgs = func(int) ([]byte, error) {
				return procArgsFixture("/current-image", []string{"tool"}, []string{"XPC_SERVICE_NAME=com.example.worker"}), nil
			}
			readLaunchdProcess = func(pid int) (launchdProcess, error) {
				if tc.parent == -1 {
					return launchdProcess{}, os.ErrNotExist
				}
				parent := 1
				if pid == 42 && tc.parent != 0 {
					parent = tc.parent
				}
				return launchdProcess{parent: parent, started: unix.Timeval{Sec: int64(pid)}}, nil
			}
			var commands []string
			runLaunchctl = func(_ context.Context, args ...string) ([]byte, int, error) {
				commands = append(commands, strings.Join(args, " "))
				require.Equal(t, "print", args[0], "lookup must never start or kill a job")
				pid := tc.system
				if strings.HasPrefix(args[1], "gui/") {
					pid = tc.gui
				}
				if strings.HasPrefix(args[1], "user/") {
					pid = tc.user
				}
				switch pid {
				case -1:
					return nil, 113, errors.New("no service")
				case -2:
					return nil, 1, errors.New("print rejected")
				case -3:
					return []byte("unknown output"), 0, nil
				}
				body := "\tstate = waiting\n\tprogram = /configured wrapper\n"
				if pid > 0 {
					body += fmt.Sprintf("\tpid = %d\n", pid)
				}
				return []byte(body), 0, nil
			}
			owner, err := lookupDaemonService(t.Context(), 42)
			if tc.wantError != "" {
				require.ErrorContains(t, err, tc.wantError)
				return
			}
			require.NoError(t, err)
			if tc.wantDomain == "" {
				assert.Nil(t, owner)
				return
			}
			require.NotNil(t, owner)
			assert.True(t, strings.HasPrefix(owner.unit, tc.wantDomain+"/"))
			assert.Equal(t, "/configured wrapper", owner.executable, "loaded program, not the execed image")
			assert.Equal(t, "simple", owner.serviceType)
			assert.NotEmpty(t, commands)
		})
	}
}

func TestLookupLaunchdRejectsChangedProcess(t *testing.T) {
	stubLaunchd(t)
	reads := 0
	readLaunchdProcess = func(int) (launchdProcess, error) {
		reads++
		return launchdProcess{parent: 1, started: unix.Timeval{Sec: int64(reads)}}, nil
	}
	readLaunchdProcArgs = func(int) ([]byte, error) {
		return procArgsFixture("/tool", []string{"tool"}, []string{"XPC_SERVICE_NAME=com.example.worker"}), nil
	}
	runLaunchctl = func(context.Context, ...string) ([]byte, int, error) {
		return []byte("\tstate = running\n\tprogram = /tool\n\tpid = 42\n"), 0, nil
	}
	_, err := lookupDaemonService(t.Context(), 42)
	require.ErrorContains(t, err, "changed")
}

func TestLaunchdCommandsAndPrivileges(t *testing.T) {
	stubLaunchd(t)
	var commands []string
	runLaunchctl = func(_ context.Context, args ...string) ([]byte, int, error) {
		commands = append(commands, strings.Join(args, " "))
		return []byte("\tstate = running\n\tprogram = /tool\n\tpid = 42\n"), 0, nil
	}
	pid, err := userServiceMainPID(t.Context(), "gui/123/com.example.worker")
	require.NoError(t, err)
	assert.Equal(t, 42, pid)
	require.NoError(t, startUserService(t.Context(), "gui/123/com.example.worker"))
	assert.Equal(t, []string{"print gui/123/com.example.worker", "kickstart gui/123/com.example.worker"}, commands)
	launchdEUID = func() int { return 501 }
	require.ErrorContains(t, servicePermissionError("system/com.example.worker"), "sudo launchctl kickstart -k system/com.example.worker")
	assert.Equal(t, "sudo launchctl bootout system/com.example.worker", serviceStopHint("system/com.example.worker"))
	launchdEUID = func() int { return 0 }
	require.NoError(t, servicePermissionError("system/com.example.worker"))
}

func TestLookupLaunchdWithoutInheritedLabel(t *testing.T) {
	for _, outcome := range []string{"wrapper", "unmanaged", "unreadable domain", "malformed domain"} {
		t.Run(outcome, func(t *testing.T) {
			stubLaunchd(t)
			readLaunchdProcArgs = func(int) ([]byte, error) {
				return procArgsFixture("/tool", []string{"tool"}, []string{"PATH=/bin"}), nil
			}
			readLaunchdProcess = func(pid int) (launchdProcess, error) {
				parent := 1
				if pid == 42 {
					parent = 40
				}
				return launchdProcess{parent: parent, started: unix.Timeval{Sec: int64(pid)}}, nil
			}
			runLaunchctl = func(_ context.Context, args ...string) ([]byte, int, error) {
				require.Equal(t, "print", args[0])
				if strings.HasSuffix(args[1], "/com.example.wrapper") {
					return []byte("\tstate = running\n\tprogram = /bin/sh\n\tpid = 40\n"), 0, nil
				}
				if outcome == "unreadable domain" {
					return nil, 1, errors.New("domain unavailable")
				}
				if outcome == "malformed domain" {
					return []byte("unknown output"), 0, nil
				}
				pid := 77
				if outcome == "wrapper" {
					pid = 40
				}
				return fmt.Appendf(nil, "domain = {\n\tservices = {\n\t\t%d 0 com.example.wrapper\n\t}\n}\n", pid), 0, nil
			}
			owner, err := lookupDaemonService(t.Context(), 42)
			if outcome == "unreadable domain" || outcome == "malformed domain" {
				require.Error(t, err)
				assert.Nil(t, owner)
				return
			}
			require.NoError(t, err)
			if outcome == "unmanaged" {
				assert.Nil(t, owner)
				return
			}
			require.NotNil(t, owner)
			assert.Equal(t, 40, owner.mainPID)
			assert.Equal(t, "/bin/sh", owner.executable)
		})
	}
}

func TestLaunchdMissingLabelDoesNotProveUnmanagedBeforeProbe(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		isolateDaemonSelection(t)
		stubServiceRecovery(t)
		stubLaunchd(t)
		daemonServiceUnit = processServiceUnit
		findDaemonService = lookupDaemonService
		t.Setenv("ROBOREV_SKIP_VERSION_CHECK", "")
		info := &daemon.RuntimeInfo{PID: 42, Version: "v-old", Network: "tcp", Address: "127.0.0.1:1"}
		getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) { return info, nil }
		getAnyRunningDaemonForStart = func(context.Context) (*daemon.RuntimeInfo, error) { return nil, os.ErrNotExist }
		readLaunchdProcArgs = func(int) ([]byte, error) {
			return procArgsFixture("/tool", []string{"tool"}, []string{"PATH=/bin"}), nil
		}
		exited := false
		readLaunchdProcess = func(int) (launchdProcess, error) {
			if exited {
				return launchdProcess{}, os.ErrNotExist
			}
			return launchdProcess{parent: 1}, nil
		}
		runLaunchctl = func(context.Context, ...string) ([]byte, int, error) { return nil, 0, nil }
		probeDaemonForEnsure = func(daemon.DaemonEndpoint, time.Duration) (*daemon.PingInfo, error) {
			exited = true
			return nil, errors.New("daemon exited during probe")
		}
		detached := 0
		startDaemonForEnsure = func() error { detached++; return nil }
		err := ensureDaemon()
		require.ErrorContains(t, err, "ownership uncertain")
		assert.Zero(t, detached, "wait for launchd rather than assuming an absent label proves no owner")
	})
}
