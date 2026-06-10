package daemon

import (
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/testenv"
)

const (
	defaultTestPort = 7373
	defaultTestAddr = "127.0.0.1:7373"
)

type runtimeData struct {
	PID     int    `json:"pid"`
	Address string `json:"address"`
	Version string `json:"version"`
}

// createRuntimeFile creates a daemon runtime JSON file in dir. If data is
// nil a valid default is generated from pid.
func createRuntimeFile(t *testing.T, dir string, pid int, data *runtimeData) string {
	t.Helper()
	if data == nil {
		data = &runtimeData{
			PID:     pid,
			Address: defaultTestAddr,
			Version: "test",
		}
	}
	bytes, err := json.Marshal(data)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Failed to marshal runtime data: %v", err)
	}
	path := filepath.Join(dir, fmt.Sprintf("daemon.%d.json", pid))
	if err := os.WriteFile(path, bytes, 0o644); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Failed to write runtime file: %v", err)
	}
	return path
}

// startMockDaemon starts an httptest server with an http.ServeMux and returns the
// "host:port" address and the mux. The server is closed automatically when the test ends.
func startMockDaemon(t *testing.T) (string, *http.ServeMux) {
	t.Helper()
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return strings.TrimPrefix(server.URL, "http://"), mux
}

// mockIdentifyProcess replaces the global identifyProcess function with mock
// for the duration of the test. Not safe for use with t.Parallel().
func mockIdentifyProcess(t *testing.T, mock func(int) processIdentity) {
	t.Helper()
	orig := identifyProcess
	identifyProcess = mock
	t.Cleanup(func() { identifyProcess = orig })
}

func TestFindAvailablePort(t *testing.T) {
	// Test finding an available port
	addr, port, err := FindAvailablePort(defaultTestAddr)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "FindAvailablePort failed: %v", err)
	}

	if addr == "" {
		assert.Condition(t, func() bool {
			return false
		}, "Expected non-empty address")
	}
	if port < defaultTestPort {
		assert.Condition(t, func() bool {
			return false
		}, "Expected port >= %d, got %d", defaultTestPort, port)
	}
}

func TestFindAvailablePort_Ephemeral(t *testing.T) {
	// Test finding an available port with ephemeral :0
	addr, port, err := FindAvailablePort("127.0.0.1:0")
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "FindAvailablePort failed for ephemeral port: %v", err)
	}

	if addr == "" {
		assert.Condition(t, func() bool {
			return false
		}, "Expected non-empty address")
	}
	if port == 0 {
		assert.Condition(t, func() bool {
			return false
		}, "Expected non-zero port assigned by OS")
	}
	expectedAddr := fmt.Sprintf("127.0.0.1:%d", port)
	if addr != expectedAddr {
		assert.Condition(t, func() bool {
			return false
		}, "Expected address %q, got %q", expectedAddr, addr)
	}
}

func TestFindAvailablePort_IPv6Loopback(t *testing.T) {
	ln, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback not available: %v", err)
	}
	ln.Close()

	addr, port, err := FindAvailablePort("[::1]:0")
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "FindAvailablePort failed for IPv6 loopback: %v", err)
	}

	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "returned address %q is invalid: %v", addr, err)
	}
	if host != "::1" {
		require.Condition(t, func() bool {
			return false
		}, "expected host ::1, got %q", host)
	}
	if portText == "0" || port == 0 {
		require.Condition(t, func() bool {
			return false
		}, "expected an assigned port, got addr=%q port=%d", addr, port)
	}
}

func TestRuntimeInfoReadWrite(t *testing.T) {
	testenv.SetDataDir(t)

	t.Run("WriteAndRead", func(t *testing.T) {
		// Write runtime info
		err := WriteRuntime(DaemonEndpoint{Network: "tcp", Address: defaultTestAddr}, "test-version")
		if err != nil {
			require.Condition(t, func() bool {
				return false
			}, "WriteRuntime failed: %v", err)
		}

		// Read it back
		info, err := ReadRuntime()
		if err != nil {
			require.Condition(t, func() bool {
				return false
			}, "ReadRuntime failed: %v", err)
		}

		if info.Address != defaultTestAddr {
			assert.Condition(t, func() bool {
				return false
			}, "Expected addr '%s', got '%s'", defaultTestAddr, info.Address)
		}
		if info.PID == 0 {
			assert.Condition(t, func() bool {
				return false
			}, "Expected non-zero PID")
		}
		if info.Version != "test-version" {
			assert.Condition(t, func() bool {
				return false
			}, "Expected version 'test-version', got '%s'", info.Version)
		}
	})

	t.Run("Remove", func(t *testing.T) {
		// Remove it
		RemoveRuntime()

		// Should fail to read now
		_, err := ReadRuntime()
		if err == nil {
			assert.Condition(t, func() bool {
				return false
			}, "Expected error after RemoveRuntime")
		}
	})
}

func TestKillDaemonSkipsHTTPForNonLoopback(t *testing.T) {
	// Verify that isLoopbackAddr correctly rejects non-loopback addresses,
	// which prevents KillDaemon from making HTTP requests to them.
	if isLoopbackAddr("192.168.1.100:7373") {
		require.Condition(t, func() bool {
			return false
		}, "192.168.1.100:7373 should not be identified as loopback")
	}

	// Mock identifyProcess so we don't have to rely on actual OS PID behavior
	mockIdentifyProcess(t, func(pid int) processIdentity {
		return processNotRoborev
	})

	// KillDaemon with a non-loopback address should skip HTTP and fall
	// through to killProcess (which returns true because of the mock).
	// This must complete promptly without attempting network connections.
	info := &RuntimeInfo{
		PID:     os.Getpid(),          // Existing PID, but mocked as not-roborev
		Address: "192.168.1.100:7373", // Non-loopback address
	}

	result := KillDaemon(info)

	// killProcess confirms the process is not roborev, so KillDaemon returns true
	if !result {
		assert.Condition(t, func() bool {
			return false
		}, "KillDaemon should return true for process confirmed not roborev")
	}
}

func TestListAllRuntimesSkipsUnreadableFiles(t *testing.T) {
	// Skip on Windows where chmod 0000 doesn't block reads
	if runtime.GOOS == "windows" {
		t.Skip("chmod 0000 doesn't block reads on Windows")
	}

	_ = testenv.SetDataDir(t)
	runtimeDir := runtimeStore().Dir
	require.NoError(t, os.MkdirAll(runtimeDir, 0o700))

	// Create a valid runtime file
	createRuntimeFile(t, runtimeDir, math.MaxInt32, nil)

	// Create an unreadable runtime file
	unreadablePath := createRuntimeFile(t, runtimeDir, math.MaxInt32-1, &runtimeData{
		PID:     math.MaxInt32 - 1,
		Address: "127.0.0.1:7374",
	})
	os.Chmod(unreadablePath, 0o000)
	t.Cleanup(func() { os.Chmod(unreadablePath, 0o644) })

	// Probe whether chmod 0000 actually blocks reads on this filesystem
	if f, probeErr := os.Open(unreadablePath); probeErr == nil {
		f.Close()
		t.Skip("filesystem does not enforce chmod 0000 read restrictions")
	}

	// ListAllRuntimes should return the readable entry without error
	runtimes, err := ListAllRuntimes()
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "ListAllRuntimes should not error on unreadable files: %v", err)
	}

	// Should have found the valid runtime
	if len(runtimes) != 1 {
		require.Condition(t, func() bool {
			return false
		}, "Expected exactly 1 runtime, got %d", len(runtimes))
	}
	if runtimes[0].PID != math.MaxInt32 {
		assert.Condition(t, func() bool {
			return false
		}, "Expected PID %d, got %d", math.MaxInt32, runtimes[0].PID)
	}
}

func TestIdentifyProcessTriState(t *testing.T) {
	// Test that identifyProcess returns appropriate tri-state values

	// Non-existent PID should return processUnknown (can't determine)
	// or processNotRoborev if the system can confirm no such process
	result := identifyProcess(math.MaxInt32)
	// Either unknown or not-roborev is acceptable for non-existent PID
	if result == processIsRoborev {
		assert.Condition(t, func() bool {
			return false
		}, "identifyProcess(math.MaxInt32) should not return processIsRoborev for non-existent PID")
	}

	// Current process is a test binary, not roborev daemon
	// Should return processNotRoborev (confirmed not a daemon)
	currentPID := os.Getpid()
	result = identifyProcess(currentPID)
	if result == processIsRoborev {
		assert.Condition(t, func() bool {
			return false
		}, "identifyProcess(%d) should not return processIsRoborev for test process", currentPID)
	}
	// On most systems we should be able to identify our own process
	if result == processUnknown {
		t.Logf("identifyProcess(%d) returned processUnknown (may be expected on some systems)", currentPID)
	}
}

func TestKillProcessConservativeOnUnknown(t *testing.T) {
	// Test that killProcess is conservative when process identity is unknown
	// Using a very high PID that almost certainly doesn't exist
	nonExistentPID := math.MaxInt32

	// killProcess should return true for non-existent PID (process is dead)
	// This is safe because the process doesn't exist at all
	result := killProcess(nonExistentPID)
	if !result {
		assert.Condition(t, func() bool {
			return false
		}, "killProcess should return true for non-existent PID")
	}
}

func TestKillProcessUnknownIdentityIsConservative(t *testing.T) {
	// Mock identifyProcess to always return unknown
	mockIdentifyProcess(t, func(pid int) processIdentity {
		return processUnknown
	})

	// Use current process PID (definitely exists)
	currentPID := os.Getpid()

	// killProcess should return false (conservative - don't clean up)
	// when identity is unknown for a live process
	result := killProcess(currentPID)
	if result {
		assert.Condition(t, func() bool {
			return false
		}, "killProcess should return false (conservative) when identity is unknown for live process")
	}
}

func TestIsLoopbackAddr(t *testing.T) {
	tests := []struct {
		addr string
		want bool
	}{
		// Valid loopback addresses
		{"127.0.0.1:7373", true},
		{"127.0.0.1:80", true},
		{"127.0.1.1:7373", true},
		{"localhost:7373", true},
		{"[::1]:7373", true},

		// Invalid/non-loopback
		{"192.168.1.1:7373", false},
		{"10.0.0.1:7373", false},
		{"8.8.8.8:7373", false},
		{"example.com:7373", false},
		{"", false},

		// Bypass attempts
		{"127.0.0.1.evil.com:80", false},   // Hostname that starts with 127
		{"127.0.0.1@evil.com:80", false},   // Userinfo bypass
		{"localhost.evil.com:7373", false}, // Hostname that starts with localhost
		{"evil.com:7373", false},
	}

	for _, tt := range tests {
		t.Run(tt.addr, func(t *testing.T) {
			got := isLoopbackAddr(tt.addr)
			if got != tt.want {
				assert.Condition(t, func() bool {
					return false
				}, "isLoopbackAddr(%q) = %v, want %v", tt.addr, got, tt.want)
			}
		})
	}
}

func TestProbeDaemonPrefersPing(t *testing.T) {
	addr, mux := startMockDaemon(t)
	mux.HandleFunc("/api/ping", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(PingInfo{
			OK:      true,
			Service: daemonServiceName,
			Version: "v-test",
			PID:     123,
		})
	})

	info, err := ProbeDaemon(DaemonEndpoint{Network: "tcp", Address: addr}, time.Second)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "ProbeDaemon failed: %v", err)
	}
	if info.Service != daemonServiceName {
		require.Condition(t, func() bool {
			return false
		}, "ProbeDaemon service = %q, want %q", info.Service, daemonServiceName)
	}
	if info.Version != "v-test" {
		require.Condition(t, func() bool {
			return false
		}, "ProbeDaemon version = %q, want %q", info.Version, "v-test")
	}
}

func TestCleanupZombieDaemonsPreservesTargetSocket(t *testing.T) {
	// Regression test: when a zombie's socket matches the target
	// (e.g. a systemd-managed socket), cleanup must remove the
	// runtime file but preserve the socket — even when killProcess
	// returns true because the PID was reused by a non-roborev
	// process.
	if runtime.GOOS == "windows" {
		t.Skip("Unix sockets not supported on Windows")
	}

	_ = testenv.SetDataDir(t)
	assert := assert.New(t)

	// Create a real Unix socket as the "target" (stands in for the
	// systemd-managed socket). Use a short path to stay under the
	// Unix socket name length limit on macOS.
	socketDir, err := os.MkdirTemp("/tmp", "rr-test-*")
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(socketDir) })
	socketPath := filepath.Join(socketDir, "d.sock")
	ln, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	defer ln.Close()

	target := DaemonEndpoint{Network: "unix", Address: socketPath}

	// Write a stale runtime file that points at the target socket.
	// Use the current PID so isProcessAlive returns true (the PID
	// exists), but mock identifyProcess to say it's not roborev
	// (simulating PID reuse).
	stalePID := os.Getpid()
	runtimeJSON, err := json.Marshal(map[string]any{
		"pid":     stalePID,
		"address": socketPath,
		"network": "unix",
		"version": "stale",
	})
	require.NoError(t, err)
	runtimeDir := runtimeStore().Dir
	require.NoError(t, os.MkdirAll(runtimeDir, 0o700))
	runtimePath := filepath.Join(
		runtimeDir, fmt.Sprintf("daemon.%d.json", stalePID))
	require.NoError(t, os.WriteFile(runtimePath, runtimeJSON, 0o644))

	mockIdentifyProcess(t, func(pid int) processIdentity {
		return processNotRoborev
	})

	cleaned := CleanupZombieDaemons(target)

	assert.Equal(1, cleaned, "should count stale daemon as cleaned")
	assert.NoFileExists(runtimePath, "runtime file should be removed")
	assert.FileExists(socketPath, "target socket must be preserved")
}

func TestRuntimeInfo_Endpoint(t *testing.T) {
	assert := assert.New(t)

	// TCP with explicit network
	info := RuntimeInfo{PID: 1, Address: "127.0.0.1:7373", Network: "tcp"}
	ep := info.Endpoint()
	assert.Equal("tcp", ep.Network)
	assert.Equal("127.0.0.1:7373", ep.Address)

	// Unix
	info = RuntimeInfo{PID: 1, Address: "/tmp/test.sock", Network: "unix"}
	ep = info.Endpoint()
	assert.Equal("unix", ep.Network)
	assert.Equal("/tmp/test.sock", ep.Address)

	// Empty network follows the kit runtime default.
	info = RuntimeInfo{PID: 1, Address: "127.0.0.1:7373", Network: ""}
	ep = info.Endpoint()
	assert.Equal("tcp", ep.Network)
}

func TestListAllRuntimesWithGlobMetacharacters(t *testing.T) {
	// Create a temp directory with glob metacharacters in the name
	tmpDir := t.TempDir()
	// Create a subdirectory with brackets (glob metacharacter)
	dataDir := filepath.Join(tmpDir, "data[test]")
	if err := os.Mkdir(dataDir, 0o755); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Failed to create test directory: %v", err)
	}

	// Set ROBOREV_DATA_DIR to the directory with metacharacters
	t.Setenv("ROBOREV_DATA_DIR", dataDir)
	runtimeDir := runtimeStore().Dir
	require.NoError(t, os.MkdirAll(runtimeDir, 0o700))

	// Create a valid runtime file
	createRuntimeFile(t, runtimeDir, math.MaxInt32, nil)

	// ListAllRuntimes should work despite glob metacharacters in path
	runtimes, err := ListAllRuntimes()
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "ListAllRuntimes failed with glob metacharacters in path: %v", err)
	}

	if len(runtimes) != 1 {
		require.Condition(t, func() bool {
			return false
		}, "Expected exactly 1 runtime, got %d", len(runtimes))
	}
	if runtimes[0].PID != math.MaxInt32 {
		assert.Condition(t, func() bool {
			return false
		}, "Expected PID %d, got %d", math.MaxInt32, runtimes[0].PID)
	}
}

// writeLegacyRuntimeFile writes a pre-v0.57 runtime file (addr/port layout,
// data dir root) and returns its path.
func writeLegacyRuntimeFile(t *testing.T, dataDir, name string, pid int, addr string) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"pid":     pid,
		"addr":    addr,
		"port":    defaultTestPort,
		"network": "tcp",
		"version": "0.56.0",
	})
	require.NoError(t, err)
	path := filepath.Join(dataDir, name)
	require.NoError(t, os.WriteFile(path, body, 0o644))
	return path
}

func TestListAllRuntimesIncludesLegacyRecords(t *testing.T) {
	assert := assert.New(t)
	dataDir := testenv.SetDataDir(t)

	// New-style record in runtime/ plus legacy daemon.<pid>.json and
	// daemon.json in the data dir root.
	runtimeDir := runtimeStore().Dir
	require.NoError(t, os.MkdirAll(runtimeDir, 0o700))
	createRuntimeFile(t, runtimeDir, math.MaxInt32, nil)
	legacyPath := writeLegacyRuntimeFile(t, dataDir,
		fmt.Sprintf("daemon.%d.json", math.MaxInt32-1), math.MaxInt32-1, "127.0.0.1:7374")
	writeLegacyRuntimeFile(t, dataDir, "daemon.json", math.MaxInt32-2, "127.0.0.1:7375")

	runtimes, err := ListAllRuntimes()
	require.NoError(t, err)
	require.Len(t, runtimes, 3)

	byPID := make(map[int]*RuntimeInfo, len(runtimes))
	for _, info := range runtimes {
		byPID[info.PID] = info
	}
	require.Contains(t, byPID, math.MaxInt32-1)
	legacy := byPID[math.MaxInt32-1]
	assert.Equal("127.0.0.1:7374", legacy.Address)
	assert.Equal("tcp", legacy.Network)
	assert.Equal("0.56.0", legacy.Version)
	assert.Equal(legacyPath, legacy.SourcePath)
	assert.Equal("127.0.0.1:7374", legacy.Endpoint().Address)
	require.Contains(t, byPID, math.MaxInt32-2)
	assert.Equal("127.0.0.1:7375", byPID[math.MaxInt32-2].Address)
}

func TestListAllRuntimesDeduplicatesLegacyByPID(t *testing.T) {
	dataDir := testenv.SetDataDir(t)

	runtimeDir := runtimeStore().Dir
	require.NoError(t, os.MkdirAll(runtimeDir, 0o700))
	createRuntimeFile(t, runtimeDir, math.MaxInt32, nil)
	// Legacy record for the same PID must not produce a duplicate entry.
	writeLegacyRuntimeFile(t, dataDir,
		fmt.Sprintf("daemon.%d.json", math.MaxInt32), math.MaxInt32, "127.0.0.1:7374")

	runtimes, err := ListAllRuntimes()
	require.NoError(t, err)
	require.Len(t, runtimes, 1)
	// The new-style record wins.
	assert.Equal(t, defaultTestAddr, runtimes[0].Address)
}

func TestListLegacyRuntimesSkipsMalformedFiles(t *testing.T) {
	assert := assert.New(t)
	dataDir := testenv.SetDataDir(t)

	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "daemon.json"), []byte("not json"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "daemon.0.json"), []byte(`{"pid":0,"addr":"x"}`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "daemon.5.json"), []byte(`{"pid":5}`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "config.toml"), []byte(""), 0o644))

	assert.Empty(listLegacyRuntimes())

	runtimes, err := ListAllRuntimes()
	require.NoError(t, err)
	assert.Empty(runtimes)
}

func TestKillDaemonStopsLegacyDaemonGracefully(t *testing.T) {
	assert := assert.New(t)
	dataDir := testenv.SetDataDir(t)

	// A legacy (pre-v0.57) daemon answers /api/shutdown but has no /api/ping.
	addr, mux := startMockDaemon(t)
	shutdownCalled := false
	mux.HandleFunc("/api/shutdown", func(w http.ResponseWriter, r *http.Request) {
		shutdownCalled = true
		w.WriteHeader(http.StatusOK)
	})

	// Use a PID that is certainly dead so the post-shutdown liveness check
	// confirms process exit.
	legacyPath := writeLegacyRuntimeFile(t, dataDir,
		fmt.Sprintf("daemon.%d.json", math.MaxInt32), math.MaxInt32, addr)
	runtimes, err := ListAllRuntimes()
	require.NoError(t, err)
	require.Len(t, runtimes, 1)

	assert.True(KillDaemon(runtimes[0]))
	assert.True(shutdownCalled, "graceful shutdown endpoint must be tried first")
	assert.NoFileExists(legacyPath, "legacy runtime file must be cleaned up")
}
