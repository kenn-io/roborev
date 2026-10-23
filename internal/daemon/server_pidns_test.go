//go:build linux

package daemon

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kitdaemon "go.kenn.io/kit/daemon"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/testenv"
	"go.kenn.io/roborev/internal/testutil"
)

func TestServerStopAfterRefusedStartPreservesForeignRuntime(t *testing.T) {
	if os.Getenv("ROBOREV_TEST_PIDNS_REFUSED_START") == "1" {
		t.Setenv("ROBOREV_DATA_DIR", os.Getenv("ROBOREV_TEST_PIDNS_DATA"))
		records, err := runtimeStore().List()
		require.NoError(t, err)
		require.Len(t, records, 1)
		rec := records[0]
		require.Equal(t, os.Getpid(), rec.PID)
		require.Equal(t, kitdaemon.ProcessScopeForeign, kitdaemon.RuntimeProcessScope(rec))
		before, err := os.ReadFile(rec.SourcePath)
		require.NoError(t, err)
		db, _ := testutil.OpenTestDBWithDir(t)
		cfg := config.DefaultConfig()
		cfg.ServerAddr = "127.0.0.1:0"
		server := NewServer(db, cfg, "")
		t.Cleanup(func() { _ = server.Close() })
		require.EqualError(t, server.Start(t.Context()), fmt.Sprintf("daemon already running (pid %d on %s)", rec.PID, rec.Address))
		require.NoError(t, server.Stop())
		after, err := os.ReadFile(rec.SourcePath)
		require.NoError(t, err)
		assert.Equal(t, before, after)
		return
	}

	t.Setenv("HOME", t.TempDir())
	t.Setenv("ROBOREV_TEST_PIDNS_DATA", testenv.SetDataDir(t))
	setShortRuntimeDir(t)
	var rec kitdaemon.RuntimeRecord
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(PingInfo{OK: true, Service: "roborev", PID: rec.PID})
	}))
	t.Cleanup(foreign.Close)
	rec = testutil.UnjudgedRuntimeRecord(t, "tcp", foreign.Listener.Addr().String())
	_, err := runtimeStore().Write(rec)
	require.NoError(t, err)
	t.Setenv("ROBOREV_TEST_RUNTIME_WRITER", "")
	t.Setenv("ROBOREV_TEST_PIDNS_REFUSED_START", "1")
	testutil.RunPIDNamespaceTest(t, nil, "")
}

func TestServerStartWithSandboxSocket(t *testing.T) {
	for _, useTLS := range []bool{false, true} {
		t.Run(fmt.Sprintf("tls=%t", useTLS), func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			testenv.SetDataDir(t)
			address := filepath.Join(setShortRuntimeDir(t), "daemon.sock")
			cfg := config.DefaultConfig()
			cfg.ServerAddr = "unix://" + address
			cfg.AuthKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
			var rec kitdaemon.RuntimeRecord
			var requests, credentials atomic.Int32
			foreign := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				assert.Equal(t, "/api/ping", r.URL.Path)
				if r.Header.Get("Authorization") != "" {
					credentials.Add(1)
				}
				_ = json.NewEncoder(w).Encode(PingInfo{OK: true, Service: "roborev", PID: rec.PID})
			}))
			t.Cleanup(foreign.Close)
			if useTLS {
				cfg.DaemonTLS = newTestPKI(t)
				cfg.ServerAddr = "127.0.0.1:0"
				rec = testutil.UnknownRuntimeRecord("tcp", foreign.Listener.Addr().String())
				rec.Metadata = map[string]string{runtimeAlternateNetworkKey: "unix", runtimeAlternateAddressKey: address}
				cert, err := tls.LoadX509KeyPair(cfg.DaemonTLS.CertFile, cfg.DaemonTLS.KeyFile)
				require.NoError(t, err)
				foreign.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
				foreign.StartTLS()
			} else {
				rec = testutil.UnjudgedRuntimeRecord(t, "unix", address)
				listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: address, Net: "unix"})
				require.NoError(t, err)
				listener.SetUnlinkOnClose(false)
				require.NoError(t, foreign.Listener.Close())
				foreign.Listener = listener
				foreign.Start()
			}
			path, err := runtimeStore().Write(rec)
			require.NoError(t, err)
			before, err := os.ReadFile(path)
			require.NoError(t, err)
			db, _ := testutil.OpenTestDBWithDir(t)
			require.NoError(t, config.SaveGlobal(cfg))
			server := NewServer(db, cfg, "")
			t.Cleanup(func() { _ = server.Close() })
			errCh := make(chan error, 1)
			go func() { errCh <- server.Start(t.Context()) }()
			returned := false
			t.Cleanup(func() {
				if !returned {
					stopTestServer(t, server, errCh)
				}
			})
			// Wall-clock wait: startup probes the foreign listener.
			require.Eventually(t, func() bool {
				select {
				case err = <-errCh:
					returned = true
					return true
				default:
					return false
				}
			}, 5*time.Second, 10*time.Millisecond)
			require.EqualError(t, err, fmt.Sprintf("daemon already running (pid %d on %s)", rec.PID, rec.Address))
			assert.Equal(t, int32(1), requests.Load())
			assert.Zero(t, credentials.Load())
			if !useTLS {
				ping, err := probeRuntimeRecordWithAuth(t.Context(), DaemonEndpoint{Network: "unix", Address: address}, config.ClientAuth{})
				require.NoError(t, err)
				assert.Equal(t, rec.PID, ping.PID)
			}
			after, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, before, after)
		})
	}
}

func TestServerStartAfterSandboxSocketStopsResponding(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	testenv.SetDataDir(t)
	address := filepath.Join(setShortRuntimeDir(t), "daemon.sock")
	var rec kitdaemon.RuntimeRecord
	// End the sandbox before trying to start its replacement.
	t.Run("sandbox", func(t *testing.T) {
		rec = testutil.UnjudgedRuntimeRecord(t, "unix", address)
	})
	if rec.PID == 0 {
		t.Skip("requires PID namespaces")
	}
	_, err := runtimeStore().Write(rec)
	require.NoError(t, err)
	db, _ := testutil.OpenTestDBWithDir(t)
	cfg := config.DefaultConfig()
	cfg.ServerAddr = "unix://" + address
	cfg.AuthKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	require.NoError(t, config.SaveGlobal(cfg))
	server := NewServer(db, cfg, "")
	t.Cleanup(func() { _ = server.Close() })
	errCh, info := startServerAndWaitForRuntime(t, server)
	assert.Equal(t, address, info.Address)
	stopTestServer(t, server, errCh)
}

func TestServerStartForeignProbeUsesSelectedConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	testenv.SetDataDir(t)
	setShortRuntimeDir(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	address := listener.Addr().String()
	require.NoError(t, listener.Close())
	rec := testutil.UnjudgedRuntimeRecord(t, "tcp", address)
	_, err = runtimeStore().Write(rec)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(config.GlobalConfigPath(), []byte("auth_key = 'invalid'\n"), 0o600))
	cfg := config.DefaultConfig()
	cfg.ServerAddr = "127.0.0.1:0"
	customPath := filepath.Join(t.TempDir(), "custom.toml")
	require.NoError(t, config.SaveGlobalTo(customPath, cfg))
	cfg, err = config.LoadGlobalFrom(customPath)
	require.NoError(t, err)
	db, _ := testutil.OpenTestDBWithDir(t)
	server := NewServer(db, cfg, "")
	t.Cleanup(func() { _ = server.Close() })
	errCh, info := startServerAndWaitForRuntime(t, server)
	assert.Equal(t, os.Getpid(), info.PID)
	stopTestServer(t, server, errCh)
}

func TestKillDaemonKeepsUnjudgedRuntimeWithoutShutdown(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	testenv.SetDataDir(t)
	rec := testutil.UnknownRuntimeRecord("tcp", "")
	var shutdownRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/ping":
			_, _ = fmt.Fprintf(w, `{"ok":true,"service":"roborev","pid":%d}`, rec.PID)
		case "/api/shutdown":
			shutdownRequests.Add(1)
			w.WriteHeader(http.StatusBadRequest)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	rec.Address = server.Listener.Addr().String()
	path, err := runtimeStore().Write(rec)
	require.NoError(t, err)
	rec.SourcePath = path
	info := runtimeInfoFromRecord(rec)
	before, err := os.ReadFile(info.SourcePath)
	require.NoError(t, err)

	err = KillDaemon(info)
	assert := assert.New(t)
	require.EqualError(t, err, fmt.Sprintf("cannot judge daemon PID %d from this PID namespace; stop it from where it runs", info.PID))
	assert.Equal(int32(0), shutdownRequests.Load())
	after, err := os.ReadFile(info.SourcePath)
	require.NoError(t, err)
	assert.Equal(before, after)
}

func TestDiscoverySkipsLiveSandboxDaemon(t *testing.T) {
	if os.Getenv("ROBOREV_TEST_PIDNS_SERVE") == "1" {
		t.Setenv("ROBOREV_DATA_DIR", os.Getenv("ROBOREV_TEST_PIDNS_DATA"))
		require.Equal(t, 2, os.Getpid())
		db, _ := testutil.OpenTestDBWithDir(t)
		cfg := config.DefaultConfig()
		cfg.ServerAddr = "127.0.0.1:0"
		server := NewServer(db, cfg, "")
		t.Cleanup(func() { _ = server.Close() })
		errCh, _ := startServerAndWaitForRuntime(t, server)
		require.NoError(t, os.WriteFile(filepath.Join(config.DataDir(), "ready"), nil, 0o600))
		_, err := io.Copy(io.Discard, os.Stdin)
		require.NoError(t, err)
		stopTestServer(t, server, errCh)
		return
	}

	t.Setenv("HOME", t.TempDir())
	dataDir := testenv.SetDataDir(t)
	t.Setenv("ROBOREV_TEST_PIDNS_DATA", dataDir)
	t.Setenv("ROBOREV_TEST_PIDNS_SERVE", "1")
	setShortRuntimeDir(t)
	testutil.RunPIDNamespaceTest(t, nil, filepath.Join(config.DataDir(), "ready"))

	records, err := runtimeStore().List()
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Equal(t, kitdaemon.ProcessScopeForeign, kitdaemon.RuntimeProcessScope(records[0]))
	path := records[0].SourcePath
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	info, err := GetAnyRunningDaemon()
	require.ErrorIs(t, err, os.ErrNotExist)
	assert.Nil(t, info)
	runtimes, err := ListAllRuntimes()
	require.NoError(t, err)
	require.Len(t, runtimes, 1)
	assert.True(t, runtimes[0].Unjudged())
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, before, after)
}

func TestServerStartAfterShellLaunchedSandbox(t *testing.T) {
	if phase := os.Getenv("ROBOREV_TEST_PIDNS_PHASE"); phase != "" {
		t.Setenv("ROBOREV_DATA_DIR", os.Getenv("ROBOREV_TEST_PIDNS_DATA"))
		setShortRuntimeDir(t)
		require.Equal(t, 2, os.Getppid(), "shell holds the old daemon's PID")
		require.Equal(t, 3, os.Getpid())
		records, err := runtimeStore().List()
		require.NoError(t, err)
		require.Len(t, records, 1)
		require.Equal(t, 2, records[0].PID)
		require.Equal(t, kitdaemon.ProcessScopeForeign, kitdaemon.RuntimeProcessScope(records[0]))

		db, _ := testutil.OpenTestDBWithDir(t)
		cfg := config.DefaultConfig()
		cfg.ServerAddr = "127.0.0.1:0"
		server := NewServer(db, cfg, "")
		t.Cleanup(func() { _ = server.Close() })
		errCh, info := startServerAndWaitForRuntime(t, server)
		assert.Equal(t, 3, info.PID)
		assert.FileExists(t, filepath.Join(runtimeStore().Dir, "daemon.2.json"))
		stopTestServer(t, server, errCh)
		return
	}

	t.Setenv("HOME", t.TempDir())
	t.Setenv("ROBOREV_TEST_PIDNS_DATA", testenv.SetDataDir(t))
	setShortRuntimeDir(t)
	// Keep the first namespace alive so its inode cannot be reused for start.
	rec := testutil.UnjudgedRuntimeRecord(t, "tcp", "127.0.0.1:1")
	_, err := runtimeStore().Write(rec)
	require.NoError(t, err)
	t.Setenv("ROBOREV_TEST_RUNTIME_WRITER", "")
	t.Setenv("ROBOREV_TEST_PIDNS_PHASE", "start")
	// The trailing command keeps the shell at PID 2 while the daemon runs at PID 3.
	testutil.RunPIDNamespaceTest(t, []string{"sh", "-c", `"$@"; result=$?; exit "$result"`, "sh"}, "")
}
