//go:build linux && integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

// installRoborev atomically replaces path with a copy of binary, as a package
// upgrade does.
func installRoborev(t *testing.T, binary, path string) {
	t.Helper()
	data, err := os.ReadFile(binary)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path+".next", data, 0o700))
	require.NoError(t, os.Rename(path+".next", path))
}

func serviceMainPIDForTest(t *testing.T, unit string) int {
	t.Helper()
	output, err := exec.Command("systemctl", "--user", "show", "--property=MainPID", "--value", unit).CombinedOutput()
	require.NoError(t, err, "%s", output)
	pid, err := strconv.Atoi(strings.TrimSpace(string(output)))
	require.NoError(t, err)
	return pid
}

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
	for _, scenario := range []string{"path with spaces", "pause", "pause after discovery failure", "restart gap", "remain after exit", "daemon start", "simple", "transient", "missing bus environment", "selection advances", "unrelated ancestor", "wrapper", "socket responsive", "socket vanished pause"} {
		t.Run(scenario, func(t *testing.T) {
			dataDir := t.TempDir()
			t.Setenv("ROBOREV_DATA_DIR", dataDir)
			t.Setenv("ROBOREV_SKIP_VERSION_CHECK", "")
			pausing := strings.HasPrefix(scenario, "pause")
			socketActivated := strings.HasPrefix(scenario, "socket")
			link := filepath.Join(dataDir, "roborev")
			if scenario == "path with spaces" {
				link = filepath.Join(dataDir, "roborev current")
			}
			stableLink := scenario == "path with spaces"
			oldTarget := filepath.Join(dataDir, "roborev-old")
			if stableLink {
				installRoborev(t, oldBinary, oldTarget)
				require.NoError(t, os.Symlink(oldTarget, link))
			} else {
				installRoborev(t, oldBinary, link)
			}
			script := writeSlowUpdateAgent(t, dataDir)
			releaseReview := filepath.Join(dataDir, "finish-review")
			if pausing {
				body, err := os.ReadFile(script)
				require.NoError(t, err)
				body = []byte(strings.Replace(string(body), "sleep 2", "while [ ! -f "+strconv.Quote(releaseReview)+" ]; do sleep 0.05; done", 1))
				require.NoError(t, os.WriteFile(script, body, 0o700))
			}
			require.NoError(t, os.WriteFile(filepath.Join(dataDir, "config.toml"), []byte("server_addr = \"127.0.0.1:0\"\nmax_workers = 1\ncodex_cmd = "+strconv.Quote(script)+"\n"), 0o600))
			name := fmt.Sprintf("roborev-test-%d-%d", os.Getpid(), time.Now().UnixNano())
			unit := name + ".service"
			unitDir := filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), "systemd", "user")
			if !filepath.IsAbs(unitDir) {
				t.Skip("XDG_RUNTIME_DIR unavailable")
			}
			require.NoError(t, os.MkdirAll(unitDir, 0o700))
			unitPath := filepath.Join(unitDir, unit)
			socketPath := filepath.Join(unitDir, name+".socket")
			unmanaged := scenario == "unrelated ancestor"
			start := strconv.Quote(link) + " daemon run --addr 127.0.0.1:0"
			if socketActivated {
				start = strconv.Quote(link) + " daemon run"
			}
			if unmanaged || scenario == "wrapper" {
				wrapper := filepath.Join(dataDir, "start-service")
				body := "#!/bin/sh\n" + start + " &\nwait\n"
				if scenario == "unrelated ancestor" {
					body = "#!/bin/sh\n" + strconv.Quote(link) + " daemon start\nexec sleep infinity\n"
				}
				require.NoError(t, os.WriteFile(wrapper, []byte(body), 0o700))
				start = strconv.Quote(wrapper)
			}
			serviceType := "Type=notify\n"
			if scenario == "restart gap" {
				serviceType += "Restart=always\nRestartSec=1s\n"
			}
			if scenario == "simple" {
				serviceType = ""
			}
			if scenario == "remain after exit" {
				serviceType = "Type=exec\nRemainAfterExit=yes\n"
			}
			if scenario == "daemon start" {
				serviceType = "Type=forking\n"
				start = strconv.Quote(link) + " daemon start"
			}
			if unmanaged || scenario == "wrapper" {
				serviceType = "Type=exec\n"
			}
			body := fmt.Sprintf("[Service]\n%sEnvironment=%s\nEnvironment=%s\nEnvironment=GIT_CONFIG_NOSYSTEM=1\nExecStart=%s\n", serviceType, strconv.Quote("ROBOREV_DATA_DIR="+dataDir), strconv.Quote("GIT_CONFIG_GLOBAL="+os.Getenv("GIT_CONFIG_GLOBAL")), start)
			if scenario != "transient" {
				require.NoError(t, os.WriteFile(unitPath, []byte(body), 0o600))
			}
			// Unix socket paths are limited to 108 bytes, shorter than many test directories.
			socketDir, err := os.MkdirTemp("", "rr")
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, os.RemoveAll(socketDir)) })
			listen := filepath.Join(socketDir, "daemon.sock")
			if socketActivated {
				require.NoError(t, os.WriteFile(socketPath, []byte("[Socket]\nListenStream="+listen+"\nSocketMode=0600\n"), 0o600))
			}
			t.Cleanup(func() {
				// Release our test agent before asking its service to drain.
				assert.NoError(t, os.WriteFile(releaseReview, nil, 0o600))
				units := []string{unit}
				if socketActivated {
					units = append([]string{name + ".socket"}, units...)
				}
				output, err := exec.Command("systemctl", append([]string{"--user", "stop"}, units...)...).CombinedOutput()
				assert.NoError(t, err, "%s", output)
				if info, err := daemon.GetAnyRunningDaemon(); err == nil {
					assert.NoError(t, daemon.KillDaemon(info))
				}
				if scenario != "transient" {
					assert.NoError(t, os.Remove(unitPath))
				}
				if socketActivated {
					assert.NoError(t, os.Remove(socketPath))
				}
				assert.NoError(t, exec.Command("systemctl", "--user", "daemon-reload").Run())
			})
			require.NoError(t, exec.Command("systemctl", "--user", "daemon-reload").Run())
			starter := exec.Command("systemctl", "--user", "start", unit)
			if socketActivated {
				starter = exec.Command("systemctl", "--user", "start", name+".socket", unit)
			}
			if scenario == "transient" {
				starter = exec.Command("systemd-run", "--user", "--unit="+unit, "--property=Type=notify", "--setenv=ROBOREV_DATA_DIR="+dataDir, "--setenv=GIT_CONFIG_NOSYSTEM=1", "--setenv=GIT_CONFIG_GLOBAL="+os.Getenv("GIT_CONFIG_GLOBAL"), link, "daemon", "run", "--addr", "127.0.0.1:0")
			}
			output, err := starter.CombinedOutput()
			require.NoError(t, err, "%s", output)
			var original *daemon.RuntimeInfo
			// The daemon publishes its runtime record from a separate process.
			require.Eventually(t, func() bool {
				original, err = daemon.GetAnyRunningDaemon()
				return err == nil
			}, 15*time.Second, 50*time.Millisecond)
			var active, queued storage.ReviewJob
			if scenario == "selection advances" || pausing || socketActivated {
				repo := testutil.NewGitRepo(t)
				sha := repo.CommitFile("first.txt", "first\n", "first")
				active = enqueueUpdateJob(t, original.Endpoint(), repo.Path(), sha)
				waitForUpdateJobStatus(t, original.Endpoint(), active.ID, storage.JobStatusRunning, 10*time.Second)
				if pausing || socketActivated {
					queued = enqueueUpdateJob(t, original.Endpoint(), repo.Path(), repo.CommitFile("second.txt", "second\n", "second"))
				}
			}
			if socketActivated {
				if scenario == "socket vanished pause" {
					installRoborev(t, newBinary, link)
					assertVanishedSocketPauseRefused(t, original, unit, dataDir, queued)
				} else {
					assertSocketReplacementKept(t, original, unit, listen, queued)
				}
				return
			}
			cliBinary := link
			if stableLink {
				require.NoError(t, os.Symlink(newBinary, link+".next"))
				require.NoError(t, os.Rename(link+".next", link))
				require.NoError(t, os.Remove(oldTarget))
				output, err := exec.Command("systemctl", "--user", "show", "--property=ExecStart", unit).Output()
				require.NoError(t, err)
				assert.Contains(t, string(output), "ExecStart={ path="+link+" ; argv[]=", "systemd must retain the configured link after retargeting")
			} else {
				installRoborev(t, newBinary, link)
			}
			if scenario == "restart gap" {
				command, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", original.PID))
				require.NoError(t, err)
				require.Contains(t, string(command), link)
				process, err := os.FindProcess(original.PID)
				require.NoError(t, err)
				require.NoError(t, process.Kill())
				// Wait for our unit to enter its real RestartSec gap and reap the old PID.
				require.Eventually(t, func() bool {
					state, err := exec.Command("systemctl", "--user", "show", "--property=SubState", "--value", unit).Output()
					_, statErr := os.Stat(fmt.Sprintf("/proc/%d", original.PID))
					return err == nil && strings.TrimSpace(string(state)) == "auto-restart" && os.IsNotExist(statErr)
				}, time.Second, 10*time.Millisecond)
				oldStart := startDaemonForEnsure
				t.Cleanup(func() { startDaemonForEnsure = oldStart })
				detached := 0
				startDaemonForEnsure = func() error { detached++; return nil }
				require.NoError(t, recoverDaemon(original, os.ErrNotExist))
				assert.Zero(t, detached)
				replacement, err := daemon.GetAnyRunningDaemon()
				require.NoError(t, err)
				assert.NotEqual(t, original.PID, replacement.PID)
				assertDaemonUpdateVersion(t, replacement.Endpoint(), "v-new")
				assert.Equal(t, replacement.PID, serviceMainPIDForTest(t, unit))
				return
			}
			cli := exec.Command(cliBinary, "status", "--json")
			if pausing {
				cli = exec.Command(cliBinary, "pause")
			}
			if scenario == "missing bus environment" {
				for _, entry := range os.Environ() {
					if !strings.HasPrefix(entry, "DBUS_SESSION_BUS_ADDRESS=") && !strings.HasPrefix(entry, "XDG_RUNTIME_DIR=") {
						cli.Env = append(cli.Env, entry)
					}
				}
			}
			var stdout bytes.Buffer
			var stderr syncBuffer
			cli.Stdout, cli.Stderr = &stdout, &stderr
			if refusal := map[string]string{
				"daemon start":      "unsupported service Type=forking",
				"remain after exit": "RemainAfterExit=yes",
				"transient":         "transient units",
			}[scenario]; refusal != "" {
				require.Error(t, cli.Run())
				assert.Contains(t, stderr.String(), refusal)
				current, err := daemon.GetAnyRunningDaemon()
				require.NoError(t, err)
				assert.Equal(t, original.PID, current.PID, "refused units keep their daemon")
				return
			}
			if scenario == "wrapper" {
				assert := assert.New(t)
				require.NoError(t, cli.Run(), "%s", stderr.String())
				process, err := os.FindProcess(original.PID)
				require.NoError(t, err)
				require.NoError(t, process.Signal(syscall.SIGSTOP))
				defer process.Signal(syscall.SIGCONT)
				_, probeErr := daemon.ProbeDaemon(original.Endpoint(), time.Second)
				require.Error(t, probeErr)
				assert.ErrorIs(recoverDaemon(original, probeErr), probeErr)
				require.NoError(t, process.Signal(syscall.SIGCONT))
				current, err := daemon.GetAnyRunningDaemon()
				require.NoError(t, err)
				assert.Equal(original.PID, current.PID)
				require.NoError(t, exec.Command("systemctl", "--user", "is-active", unit).Run())
				return
			}
			if pausing {
				var recovery chan error
				if scenario == "pause after discovery failure" {
					oldFind, oldDiscovery := findDaemonService, getAnyRunningDaemonForStart
					oldPause, oldOut := pendingStartPause, lifecycleOut
					t.Cleanup(func() {
						findDaemonService, getAnyRunningDaemonForStart = oldFind, oldDiscovery
						pendingStartPause, lifecycleOut = oldPause, oldOut
					})
					findDaemonService = func(ctx context.Context, pid int) (*daemonService, error) {
						owner, err := lookupDaemonService(ctx, pid)
						if owner != nil {
							// Recovery runs in the test executable; retain the real unit lookup.
							owner.executable = "/proc/self/exe"
						}
						return owner, err
					}
					missed := false
					getAnyRunningDaemonForStart = func(ctx context.Context) (*daemon.RuntimeInfo, error) {
						if !missed {
							missed = true
							return nil, os.ErrNotExist
						}
						return daemon.GetAnyRunningDaemonContext(ctx)
					}
					paused := true
					pendingStartPause, lifecycleOut = &paused, &stderr
					recovery = make(chan error, 1)
					go func() { recovery <- recoverDaemon(original, os.ErrNotExist) }()
					defer func() {
						if recovery != nil {
							assert.NoError(t, os.WriteFile(releaseReview, nil, 0o600))
							assert.NoError(t, <-recovery)
						}
					}()
				} else {
					require.NoError(t, cli.Start())
					defer cli.Process.Kill()
				}
				// The pause CLI persists its state before the old daemon drains.
				require.Eventually(t, func() bool { return strings.Contains(stderr.String(), "Waiting for daemon shutdown") }, 10*time.Second, 10*time.Millisecond)
				status, err := original.Endpoint().APIClient(time.Second).GetStatus(context.Background())
				require.NoError(t, err)
				assert.True(t, status.QueuePaused)
				require.NoError(t, os.WriteFile(releaseReview, nil, 0o600))
				if recovery != nil {
					err := <-recovery
					recovery = nil
					require.NoError(t, err)
				} else {
					require.NoError(t, cli.Wait(), "%s", stderr.String())
				}
			} else if scenario == "selection advances" {
				// Observe graceful shutdown before advancing the shared selection again.
				require.NoError(t, cli.Start())
				require.Eventually(t, func() bool { return strings.Contains(stderr.String(), "Waiting for daemon shutdown") }, 10*time.Second, 10*time.Millisecond)
				installRoborev(t, oldBinary, link)
				second := exec.Command(link, "status", "--json")
				require.NoError(t, second.Start())
				defer second.Process.Kill()
				require.NoError(t, cli.Wait(), "%s", stderr.String())
				require.NoError(t, second.Wait())
			} else {
				err = cli.Run()
				require.NoError(t, err, "%s", stderr.String())
			}
			if !pausing {
				var result statusJSONOutput
				require.NoError(t, json.Unmarshal(stdout.Bytes(), &result))
				assert.True(t, result.Running)
			}
			replacement, err := daemon.GetAnyRunningDaemon()
			require.NoError(t, err)
			assert.NotEqual(t, original.PID, replacement.PID)
			expectedVersion := "v-new"
			if scenario == "selection advances" {
				expectedVersion = "v-old"
			}
			assertDaemonUpdateVersion(t, replacement.Endpoint(), expectedVersion)
			if pausing {
				status, err := replacement.Endpoint().APIClient(time.Second).GetStatus(context.Background())
				require.NoError(t, err)
				assert.True(t, status.QueuePaused)
				assert.Equal(t, storage.JobStatusQueued, getUpdateJob(t, replacement.Endpoint(), queued.ID).Status)
				output, err := exec.Command(link, "unpause").CombinedOutput()
				require.NoError(t, err, "%s", output)
				waitForUpdateJobStatus(t, replacement.Endpoint(), queued.ID, storage.JobStatusDone, 10*time.Second)
			}
			if scenario == "selection advances" {
				waitForUpdateJobStatus(t, replacement.Endpoint(), active.ID, storage.JobStatusDone, 10*time.Second)
			}
			if unmanaged {
				return
			}
			output, err = exec.Command("systemctl", "--user", "start", unit).CombinedOutput()
			require.NoError(t, err, "%s", output)
			assert.Equal(t, replacement.PID, serviceMainPIDForTest(t, unit))
		})
	}
}

func assertVanishedSocketPauseRefused(t *testing.T, original *daemon.RuntimeInfo, unit, dataDir string, queued storage.ReviewJob) {
	t.Helper()
	oldFind, oldDiscovery, oldPause := findDaemonService, getAnyRunningDaemonForStart, pendingStartPause
	t.Cleanup(func() {
		findDaemonService, getAnyRunningDaemonForStart, pendingStartPause = oldFind, oldDiscovery, oldPause
	})
	findDaemonService = func(ctx context.Context, pid int) (*daemonService, error) {
		owner, err := lookupDaemonService(ctx, pid)
		if owner != nil {
			owner.executable = "/proc/self/exe"
		}
		return owner, err
	}
	missed := false
	getAnyRunningDaemonForStart = func(ctx context.Context) (*daemon.RuntimeInfo, error) {
		if !missed {
			// The real daemon exits after ownership is captured. Its socket
			// remains active, and its queued review has not been claimed.
			require.NoError(t, daemon.KillDaemon(original))
			missed = true
			return nil, os.ErrNotExist
		}
		return daemon.GetAnyRunningDaemonContext(ctx)
	}
	paused := true
	pendingStartPause = &paused
	err := recoverDaemon(original, nil)
	assert.ErrorContains(t, err, "pause queue before shutdown")
	assert.Zero(t, serviceMainPIDForTest(t, unit), "failed pause must leave the activation socket untouched")
	db, err := storage.OpenReadOnly(filepath.Join(dataDir, "reviews.db"))
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, db.Close()) })
	job, err := db.GetJobByID(queued.ID)
	require.NoError(t, err)
	assert.Equal(t, storage.JobStatusQueued, job.Status, "pause failure must leave the queued review unclaimed")
}

// assertSocketReplacementKept triggers socket activation after the old daemon
// exits and before recovery starts the unit.
func assertSocketReplacementKept(t *testing.T, original *daemon.RuntimeInfo, unit, listen string, queued storage.ReviewJob) {
	t.Helper()
	// The test binary is not the service executable; accept its installation.
	oldFind, oldDrain := findDaemonService, drainManagedDaemon
	t.Cleanup(func() { findDaemonService, drainManagedDaemon = oldFind, oldDrain })
	findDaemonService = func(ctx context.Context, pid int) (*daemonService, error) {
		owner, err := lookupDaemonService(ctx, pid)
		if owner != nil {
			owner.executable = "/proc/self/exe"
		}
		return owner, err
	}
	var activated int
	drainManagedDaemon = func(info *daemon.RuntimeInfo) error {
		if err := daemon.KillDaemon(info); err != nil {
			return err
		}
		conn, err := net.Dial("unix", listen)
		require.NoError(t, err)
		defer conn.Close()
		// systemd starts the replacement in a separate process.
		require.Eventually(t, func() bool {
			activated = serviceMainPIDForTest(t, unit)
			return activated != 0 && activated != original.PID
		}, 15*time.Second, 10*time.Millisecond)
		require.Eventually(t, func() bool {
			current, err := daemon.GetAnyRunningDaemon()
			return err == nil && current.PID == activated
		}, 15*time.Second, 10*time.Millisecond)
		return nil
	}
	require.NoError(t, recoverDaemon(original, nil))
	assert.Equal(t, activated, serviceMainPIDForTest(t, unit), "recovery must keep the activated replacement")
	replacement, err := daemon.GetAnyRunningDaemon()
	require.NoError(t, err)
	assert.Equal(t, activated, replacement.PID)
	waitForUpdateJobStatus(t, replacement.Endpoint(), queued.ID, storage.JobStatusDone, 15*time.Second)
	assert.Equal(t, activated, serviceMainPIDForTest(t, unit))
}
