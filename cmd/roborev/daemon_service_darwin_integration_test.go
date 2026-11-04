//go:build darwin && integration

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/daemon"
)

type launchdFixture struct {
	binary, unit string
	wrapper      bool
}

func newLaunchdFixture(t *testing.T, binary string, keepAlive, wrapper bool) launchdFixture {
	t.Helper()
	if os.Getenv("ROBOREV_TEST_LAUNCHD") != "1" {
		t.Skip("set ROBOREV_TEST_LAUNCHD=1 to bootstrap isolated LaunchAgents")
	}
	home := t.TempDir()
	data := filepath.Join(home, "data")
	require.NoError(t, os.MkdirAll(data, 0o700))
	t.Setenv("HOME", home)
	t.Setenv("ROBOREV_DATA_DIR", data)
	installed := filepath.Join(home, "roborev")
	installLaunchdFixtureBinary(t, binary, installed)
	label := "com.example.roborev-test-" + rand.Text()
	uid := strconv.Itoa(os.Getuid())
	domain := "gui/" + uid
	if _, _, err := runLaunchctl(t.Context(), "print", domain); err != nil {
		domain = "user/" + uid
	}
	escape := func(value string) string {
		var b bytes.Buffer
		require.NoError(t, xml.EscapeText(&b, []byte(value)))
		return b.String()
	}
	program, keep := "", ""
	arguments := "<string>" + escape(installed) + "</string><string>daemon</string><string>run</string><string>--addr</string><string>127.0.0.1:0</string>"
	if keepAlive {
		program = "<key>Program</key><string>" + escape(installed) + "</string>"
		keep = "<key>KeepAlive</key><true/>"
	}
	if wrapper {
		// System shells can remove launchd identity from their environment. This
		// fixture deliberately preserves it to test inherited-label ownership.
		program = ""
		arguments = "<string>/bin/sh</string><string>-c</string><string>" + escape(`env -u XPC_SERVICE_NAME "$1" daemon run --addr 127.0.0.1:0 & wait`) + "</string><string>fixture-wrapper</string><string>" + escape(installed) + "</string><string>" + escape(label) + "</string>"
	}
	// The non-KeepAlive fixture deliberately uses ProgramArguments alone.
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>%s</string>%s%s
<key>RunAtLoad</key><true/>
<key>ProgramArguments</key><array>%s</array>
<key>EnvironmentVariables</key><dict><key>HOME</key><string>%s</string><key>ROBOREV_DATA_DIR</key><string>%s</string><key>PATH</key><string>%s</string></dict>
<key>StandardOutPath</key><string>%s</string><key>StandardErrorPath</key><string>%s</string>
</dict></plist>`, escape(label), program, keep, arguments, escape(home), escape(data), escape(os.Getenv("PATH")), escape(filepath.Join(home, "stdout")), escape(filepath.Join(home, "stderr")))
	plistPath := filepath.Join(home, "test.plist")
	require.NoError(t, os.WriteFile(plistPath, []byte(plist), 0o600))
	f := launchdFixture{binary: installed, unit: domain + "/" + label, wrapper: wrapper}
	bootstrapped := false
	t.Cleanup(func() {
		// launchd and its subprocesses outlive the canceled test context.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 30*time.Second)
		defer cancel()
		if bootstrapped {
			_, _, err := runLaunchctl(ctx, "bootout", f.unit)
			assert.NoError(t, err)
		}
		// Use this fixture's store explicitly: t.Setenv restores the outer
		// fixture after nested tests, and no live runtime store is consulted.
		records, err := daemon.ListJudgedRuntimes()
		assert.NoError(t, err)
		for _, record := range records {
			if record.PID > 0 && !record.Stale() {
				process, err := os.FindProcess(record.PID)
				if err == nil {
					_ = process.Kill()
				}
			}
		}
	})
	out, _, err := runLaunchctl(t.Context(), "bootstrap", domain, plistPath)
	require.NoError(t, err, "isolated LaunchAgent bootstrap: %s", out)
	bootstrapped = true
	f.runtime(t)
	return f
}

func installLaunchdFixtureBinary(t *testing.T, source, destination string) {
	t.Helper()
	data, err := os.ReadFile(source)
	require.NoError(t, err)
	stage := destination + ".next"
	require.NoError(t, os.WriteFile(stage, data, 0o755))
	require.NoError(t, os.Rename(stage, destination))
}

func (f launchdFixture) runtime(t *testing.T) *daemon.RuntimeInfo {
	t.Helper()
	var ready *daemon.RuntimeInfo
	var lastError error
	defer func() {
		if t.Failed() {
			t.Logf("last readiness error: %v", lastError)
		}
	}()
	// Wall-clock wait: launchd subprocess publication and HTTP readiness.
	require.Eventually(t, func() bool {
		info, err := daemon.GetAnyRunningDaemonContext(t.Context())
		if err != nil {
			lastError = err
			return false
		}
		pid, err := userServiceMainPID(t.Context(), f.unit)
		if err != nil {
			lastError = err
			return false
		}
		if f.wrapper {
			owner, err := lookupDaemonService(t.Context(), info.PID)
			if err != nil || owner == nil || owner.unit != f.unit {
				lastError = fmt.Errorf("ownership: owner=%+v, err=%v", owner, err)
				return false
			}
		} else if info.PID != pid {
			return false
		}
		ready = info
		return true
	}, 30*time.Second, 50*time.Millisecond)
	return ready
}

func (f launchdFixture) command(t *testing.T, args ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, f.binary, args...)
	cmd.Env = filterDaemonEnv(os.Environ())
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "fixture command: %s", out)
	return out
}

func TestLaunchdRecoveryIntegration(t *testing.T) {
	if os.Getenv("ROBOREV_TEST_LAUNCHD") != "1" {
		t.Skip("set ROBOREV_TEST_LAUNCHD=1 to bootstrap isolated LaunchAgents")
	}
	oldBinary, newBinary := versionedUpdateDaemonBinaries(t)
	for _, keepAlive := range []bool{false, true} {
		t.Run(fmt.Sprintf("keep-alive-%t", keepAlive), func(t *testing.T) {
			f := newLaunchdFixture(t, oldBinary, keepAlive, false)
			old := f.runtime(t)
			owner, err := lookupDaemonService(t.Context(), old.PID)
			require.NoError(t, err)
			require.NotNil(t, owner)
			assert.Equal(t, f.unit, owner.unit)
			assert.Equal(t, f.binary, owner.executable)
			installLaunchdFixtureBinary(t, newBinary, f.binary)
			f.command(t, "status", "--json")
			replacement := f.runtime(t)
			assert.NotEqual(t, old.PID, replacement.PID)
			assert.Equal(t, "v-new", replacement.Version)
			f.command(t, "daemon", "restart")
			restarted := f.runtime(t)
			assert.NotEqual(t, replacement.PID, restarted.PID)

			// Update handoff uses the captured job rather than detached startup.
			session, err := prepareUpdateDaemon(t.Context(), restarted.Endpoint(), "fixture-owner", policyWait, io.Discard)
			require.NoError(t, err)
			owner, err = lookupDaemonService(t.Context(), restarted.PID)
			require.NoError(t, err)
			require.NoError(t, session.shutdown(t.Context()))
			require.NoError(t, restartAndVerifyUpdatedDaemon(t.Context(), filepath.Dir(f.binary), "v-new", restarted, owner))
			updated := f.runtime(t)
			assert.NotEqual(t, restarted.PID, updated.PID)
			records, err := daemon.ListJudgedRuntimes()
			require.NoError(t, err)
			var live []int
			for _, record := range records {
				if !record.Stale() {
					live = append(live, record.PID)
				}
			}
			assert.Equal(t, []int{updated.PID}, live)
		})
	}
}

func TestLaunchdInheritedLabelIntegration(t *testing.T) {
	if os.Getenv("ROBOREV_TEST_LAUNCHD") != "1" {
		t.Skip("set ROBOREV_TEST_LAUNCHD=1 to bootstrap isolated LaunchAgents")
	}
	oldBinary, newBinary := versionedUpdateDaemonBinaries(t)
	f := newLaunchdFixture(t, oldBinary, false, true)
	old := f.runtime(t)
	owner, err := lookupDaemonService(t.Context(), old.PID)
	require.NoError(t, err)
	require.NotNil(t, owner)
	assert.NotEqual(t, old.PID, owner.mainPID)
	installLaunchdFixtureBinary(t, newBinary, f.binary)
	f.command(t, "status", "--json")
	assert.Equal(t, old.PID, f.runtime(t).PID, "responsive wrapper child stays in use")
	t.Run("detached", func(t *testing.T) {
		data := t.TempDir()
		t.Setenv("HOME", data)
		t.Setenv("ROBOREV_DATA_DIR", data)
		t.Setenv("XPC_SERVICE_NAME", filepath.Base(f.unit))
		configPath := filepath.Join(data, "config.toml")
		require.NoError(t, os.WriteFile(configPath, []byte("max_workers = 1\n"), 0o600))
		detached := startIsolatedUpdateDaemon(t, newBinary, data, filepath.Join(data, "reviews.db"), configPath)
		owner, err := lookupDaemonService(t.Context(), detached.cmd.Process.Pid)
		require.NoError(t, err)
		assert.Nil(t, owner, "an inherited label without job ancestry is unmanaged")
		shutdownIsolatedUpdateDaemon(t, detached)
	})
}
