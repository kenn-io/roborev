//go:build darwin && integration

package main

import (
	"bytes"
	"context"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/daemon"
)

func TestLaunchctlCLIRecoveryIntegration(t *testing.T) {
	if os.Getenv("ROBOREV_TEST_LAUNCHD") != "1" {
		t.Skip("set ROBOREV_TEST_LAUNCHD=1 to test LaunchAgents")
	}
	oldBinary, newBinary := versionedUpdateDaemonBinaries(t)
	for _, wrapper := range []bool{false, true} {
		t.Run(fmt.Sprintf("wrapper=%t", wrapper), func(t *testing.T) {
			check := assert.New(t)
			stubServiceRecovery(t)
			dataDir := t.TempDir()
			t.Setenv("ROBOREV_DATA_DIR", dataDir)
			t.Setenv("ROBOREV_SKIP_VERSION_CHECK", "")
			link := filepath.Join(dataDir, "roborev")
			installRoborev(t, oldBinary, link)
			require.NoError(t, os.WriteFile(filepath.Join(dataDir, "config.toml"), []byte("server_addr = \"127.0.0.1:0\"\n"), 0o600))
			label := fmt.Sprintf("com.example.roborev-test-%d-%d", os.Getpid(), time.Now().UnixNano())
			domain := "gui/" + strconv.Itoa(os.Getuid())
			// Headless runners can bootstrap LaunchAgents in their user domain.
			if _, err := runLaunchctl(t.Context(), "print", domain); err != nil {
				domain = "user/" + strconv.Itoa(os.Getuid())
			}
			unit := domain + "/" + label
			args := "<string>" + html.EscapeString(link) + "</string><string>daemon</string><string>run</string>"
			if wrapper {
				// Waiting after the child exits prevents sh from replacing itself with it.
				args = "<string>/bin/sh</string><string>-c</string><string>&quot;$1&quot; daemon run &amp; wait; sleep 1</string><string>sh</string><string>" + html.EscapeString(link) + "</string>"
			}
			plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
<key>Label</key><string>%s</string>
<key>ProgramArguments</key><array>%s</array>
<key>KeepAlive</key><true/>
<key>EnvironmentVariables</key><dict>
<key>ROBOREV_DATA_DIR</key><string>%s</string>
<key>GIT_CONFIG_GLOBAL</key><string>%s</string>
<key>GIT_CONFIG_NOSYSTEM</key><string>1</string>
<key>PATH</key><string>%s</string>
</dict>
<key>StandardOutPath</key><string>%s</string>
<key>StandardErrorPath</key><string>%s</string>
</dict></plist>`, label, args, html.EscapeString(dataDir), html.EscapeString(os.Getenv("GIT_CONFIG_GLOBAL")), html.EscapeString(os.Getenv("PATH")), html.EscapeString(filepath.Join(dataDir, "daemon.log")), html.EscapeString(filepath.Join(dataDir, "daemon.log")))
			plistPath := filepath.Join(dataDir, "daemon.plist")
			require.NoError(t, os.WriteFile(plistPath, []byte(plist), 0o600))
			t.Cleanup(func() {
				ctx := context.WithoutCancel(t.Context())
				_, err := runLaunchctl(ctx, "bootout", unit)
				check.NoError(err)
				// A wrapper child can outlive bootout; stop only our isolated daemon.
				if info, err := daemon.GetAnyRunningDaemonContext(ctx); err == nil {
					check.NoError(daemon.KillDaemon(info))
				}
			})
			_, err := runLaunchctl(t.Context(), "bootstrap", domain, plistPath)
			require.NoError(t, err)
			var original *daemon.RuntimeInfo
			// launchd starts a separate process that publishes its runtime record.
			require.Eventually(t, func() bool {
				original, err = daemon.GetAnyRunningDaemonContext(t.Context())
				return err == nil
			}, 15*time.Second, 50*time.Millisecond)
			raw, err := runLaunchctl(t.Context(), "print", unit)
			require.NoError(t, err)
			owner, _, err := parseLaunchctlService(raw)
			require.NoError(t, err, "launchctl print:\n%s", raw)
			if wrapper {
				check.NotEqual(original.PID, owner.mainPID)
			} else {
				check.Equal(original.PID, owner.mainPID)
			}
			installRoborev(t, newBinary, link)
			// The test executable represents the new CLI installation at this seam.
			findDaemonService = func(ctx context.Context, pid int) (*daemonService, error) {
				owner, err := lookupDaemonService(ctx, pid)
				if owner != nil {
					path, pathErr := os.Executable()
					require.NoError(t, pathErr)
					owner.executable = path
				}
				return owner, err
			}
			detached := 0
			startDaemonForEnsure = func() error { detached++; return nil }
			var output bytes.Buffer
			lifecycleOut = &output
			require.NoError(t, recoverDaemon(original, nil, false))
			check.Zero(detached)
			replacement, err := daemon.GetAnyRunningDaemonContext(t.Context())
			require.NoError(t, err)
			if wrapper {
				check.Equal(original.PID, replacement.PID)
				check.Contains(output.String(), "recovery skipped")
				assertDaemonUpdateVersion(t, replacement.Endpoint(), "v-old")
			} else {
				check.NotEqual(original.PID, replacement.PID)
				pid, err := userServiceMainPID(t.Context(), unit)
				require.NoError(t, err)
				check.Equal(replacement.PID, pid)
				assertDaemonUpdateVersion(t, replacement.Endpoint(), "v-new")
				runtimes, err := daemon.ListJudgedRuntimes()
				require.NoError(t, err)
				check.Len(runtimes, 1)
			}
		})
	}
}
