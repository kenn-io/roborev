//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func TestSystemdCLIRecoveryIntegration(t *testing.T) {
	if _, err := exec.LookPath("systemctl"); err != nil {
		t.Skip("systemctl unavailable")
	}
	if err := exec.Command("systemctl", "--user", "show-environment").Run(); err != nil {
		t.Skip("user manager unavailable")
	}
	buildDir := t.TempDir()
	oldBinary := buildVersionedUpdateDaemon(t, buildDir, "roborev-old", "v-old")
	newBinary := buildVersionedUpdateDaemon(t, buildDir, "roborev-new", "v-new")
	for _, scenario := range []string{"shared", "foreign", "selection advances"} {
		t.Run(scenario, func(t *testing.T) {
			dataDir := t.TempDir()
			t.Setenv("ROBOREV_DATA_DIR", dataDir)
			t.Setenv("ROBOREV_SKIP_VERSION_CHECK", "")
			link := filepath.Join(dataDir, "roborev")
			require.NoError(t, os.Symlink(oldBinary, link))
			script := writeSlowUpdateAgent(t, dataDir)
			require.NoError(t, os.WriteFile(filepath.Join(dataDir, "config.toml"), []byte("max_workers = 1\ncodex_cmd = "+strconv.Quote(script)+"\n"), 0o600))
			unit := fmt.Sprintf("roborev-test-%d-%d.service", os.Getpid(), time.Now().UnixNano())
			unitDir := filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), "systemd", "user")
			require.True(t, filepath.IsAbs(unitDir))
			require.NoError(t, os.MkdirAll(unitDir, 0o700))
			unitPath := filepath.Join(unitDir, unit)
			body := fmt.Sprintf("[Service]\nType=exec\nEnvironment=%s\nEnvironment=%s\nEnvironment=GIT_CONFIG_NOSYSTEM=1\nExecStart=%s daemon run --addr 127.0.0.1:0\n", strconv.Quote("ROBOREV_DATA_DIR="+dataDir), strconv.Quote("GIT_CONFIG_GLOBAL="+os.Getenv("GIT_CONFIG_GLOBAL")), strconv.Quote(link))
			require.NoError(t, os.WriteFile(unitPath, []byte(body), 0o600))
			t.Cleanup(func() {
				output, err := exec.Command("systemctl", "--user", "stop", unit).CombinedOutput()
				assert.NoError(t, err, "%s", output)
				if info, err := daemon.GetAnyRunningDaemon(); err == nil {
					assert.NoError(t, daemon.KillDaemon(info))
				}
				assert.NoError(t, os.Remove(unitPath))
				assert.NoError(t, exec.Command("systemctl", "--user", "daemon-reload").Run())
			})
			require.NoError(t, exec.Command("systemctl", "--user", "daemon-reload").Run())
			output, err := exec.Command("systemctl", "--user", "start", unit).CombinedOutput()
			require.NoError(t, err, "%s", output)
			var original *daemon.RuntimeInfo
			require.Eventually(t, func() bool {
				original, err = daemon.GetAnyRunningDaemon()
				return err == nil
			}, 15*time.Second, 50*time.Millisecond)
			owner, err := lookupDaemonService(context.Background(), original.PID)
			require.NoError(t, err)
			require.NotNil(t, owner)
			assert.Equal(t, unit, owner.unit)
			var active storage.ReviewJob
			if scenario == "selection advances" {
				repo := testutil.NewGitRepo(t)
				sha := repo.CommitFile("first.txt", "first\n", "first")
				active = enqueueUpdateJob(t, original.Endpoint(), repo.Path(), sha)
				waitForUpdateJobStatus(t, original.Endpoint(), active.ID, storage.JobStatusRunning, 10*time.Second)
			}
			if scenario != "foreign" {
				next := link + ".next"
				require.NoError(t, os.Symlink(newBinary, next))
				require.NoError(t, os.Rename(next, link))
			}
			cli := exec.Command(newBinary, "status", "--json")
			var stdout, stderr bytes.Buffer
			cli.Stdout, cli.Stderr = &stdout, &stderr
			if scenario == "selection advances" {
				// Observe graceful shutdown before advancing the shared selection again.
				require.NoError(t, cli.Start())
				require.Eventually(t, func() bool {
					resp, err := original.Endpoint().APIClient(time.Second).PauseQueueRaw(context.Background())
					if err != nil {
						return false
					}
					defer resp.Body.Close()
					return resp.StatusCode == http.StatusConflict
				}, time.Second, 10*time.Millisecond)
				next := link + ".next"
				require.NoError(t, os.Symlink(oldBinary, next))
				require.NoError(t, os.Rename(next, link))
				require.NoError(t, cli.Wait(), "%s", stderr.String())
			} else {
				err = cli.Run()
				require.NoError(t, err, "%s", stderr.String())
				if scenario == "foreign" {
					assert.Contains(t, stderr.String(), "different installation")
				}
			}
			var result statusJSONOutput
			require.NoError(t, json.Unmarshal(stdout.Bytes(), &result))
			assert.True(t, result.Running)
			replacement, err := daemon.GetAnyRunningDaemon()
			require.NoError(t, err)
			owner, err = lookupDaemonService(context.Background(), replacement.PID)
			require.NoError(t, err)
			require.NotNil(t, owner)
			assert.Equal(t, unit, owner.unit)
			if scenario == "foreign" {
				assert.Equal(t, original.PID, replacement.PID)
			} else {
				assert.NotEqual(t, original.PID, replacement.PID)
			}
			expectedVersion := "v-new"
			if scenario != "shared" {
				expectedVersion = "v-old"
			}
			assertDaemonUpdateVersion(t, replacement.Endpoint(), expectedVersion)
			if scenario == "selection advances" {
				waitForUpdateJobStatus(t, replacement.Endpoint(), active.ID, storage.JobStatusDone, time.Second)
			}
			output, err = exec.Command("systemctl", "--user", "start", unit).CombinedOutput()
			require.NoError(t, err, "%s", output)
			output, err = exec.Command("systemctl", "--user", "show", "--property=MainPID", "--value", unit).CombinedOutput()
			require.NoError(t, err)
			assert.Equal(t, strconv.Itoa(replacement.PID), strings.TrimSpace(string(output)))
		})
	}
}
