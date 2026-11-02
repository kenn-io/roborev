package daemon

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseEndpoint(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		network string
		wantErr string
	}{
		{"empty defaults to TCP", "", "tcp", ""},
		{"bare host:port", "127.0.0.1:7373", "tcp", ""},
		{"ipv6 loopback", "[::1]:7373", "tcp", ""},
		{"localhost", "localhost:7373", "tcp", ""},
		{"http prefix stripped", "http://127.0.0.1:7373", "tcp", ""},
		{"unix auto", "unix://", "unix", ""},                        // skipped on Windows (no os.Getuid)
		{"unix explicit path", "unix:///tmp/test.sock", "unix", ""}, // skipped on Windows (not absolute)
		{"non-loopback rejected", "192.168.1.1:7373", "", "loopback"},
		{"relative unix rejected", "unix://relative.sock", "", "absolute"},
		{"http non-loopback rejected", "http://192.168.1.1:7373", "", "loopback"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if runtime.GOOS == "windows" && strings.HasPrefix(tt.input, "unix://") {
				t.Skip("Unix sockets not supported on Windows")
			}
			ep, err := ParseEndpoint(tt.input)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.network, ep.Network)
			if tt.network == "tcp" {
				assert.NotEmpty(t, ep.Address)
			}
		})
	}
}

func TestParseEndpoint_UnixPathTooLong(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix sockets not supported on Windows")
	}
	long := "unix:///" + strings.Repeat("a", MaxUnixPathLen)
	_, err := ParseEndpoint(long)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeds")
}

func TestParseEndpoint_UnixNullByte(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix sockets not supported on Windows")
	}
	_, err := ParseEndpoint("unix:///tmp/bad\x00.sock")
	require.Error(t, err)
}

func TestDefaultSocketPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix sockets not supported on Windows")
	}

	t.Run("UsesXDGRuntimeDirWhenSet", func(t *testing.T) {
		// Use a short path so the socket path stays under MaxUnixPathLen
		// (104 on macOS). t.TempDir() paths are too long on macOS.
		dir, err := os.MkdirTemp("/tmp", "rr-xdg-*")
		require.NoError(t, err)
		t.Cleanup(func() { os.RemoveAll(dir) })
		t.Setenv("XDG_RUNTIME_DIR", dir)
		path := DefaultSocketPath()
		assert.Less(t, len(path), MaxUnixPathLen,
			"default socket path %q (%d bytes) exceeds limit %d",
			path, len(path), MaxUnixPathLen)
		assert.Equal(t, filepath.Join(dir, "roborev", "daemon.sock"), path)
	})

	t.Run("FallsBackWhenXDGIsRelative", func(t *testing.T) {
		t.Setenv("XDG_RUNTIME_DIR", "relative/path")
		path := DefaultSocketPath()
		assert.NotContains(t, path, "relative/path",
			"should fall back to tempdir for relative XDG_RUNTIME_DIR")
		assert.Contains(t, path, fmt.Sprintf("roborev-%d", os.Getuid()))
	})

	t.Run("FallsBackWhenXDGDoesNotExist", func(t *testing.T) {
		t.Setenv("XDG_RUNTIME_DIR", "/nonexistent-roborev-test-dir")
		path := DefaultSocketPath()
		assert.NotContains(t, path, "nonexistent-roborev-test-dir",
			"should fall back to tempdir for missing XDG_RUNTIME_DIR")
		assert.Contains(t, path, fmt.Sprintf("roborev-%d", os.Getuid()))
	})

	t.Run("FallsBackWhenXDGIsAFile", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "not-a-dir")
		require.NoError(t, os.WriteFile(f, nil, 0o600))
		t.Setenv("XDG_RUNTIME_DIR", f)
		path := DefaultSocketPath()
		assert.NotContains(t, path, "not-a-dir",
			"should fall back to tempdir when XDG_RUNTIME_DIR is a file")
		assert.Contains(t, path, fmt.Sprintf("roborev-%d", os.Getuid()))
	})

	t.Run("FallsBackWhenXDGPathTooLong", func(t *testing.T) {
		// Build a real, deeply nested directory whose joined socket
		// path exceeds MaxUnixPathLen so the length check is the
		// branch that triggers the fallback, not os.Stat.
		base := t.TempDir()
		deep := base
		for len(filepath.Join(deep, "roborev", "daemon.sock")) < MaxUnixPathLen {
			deep = filepath.Join(deep, "d")
		}
		require.NoError(t, os.MkdirAll(deep, 0o700))
		t.Setenv("XDG_RUNTIME_DIR", deep)
		path := DefaultSocketPath()
		assert.Less(t, len(path), MaxUnixPathLen,
			"default socket path %q (%d bytes) exceeds limit %d",
			path, len(path), MaxUnixPathLen)
		assert.NotContains(t, path, deep,
			"should fall back to tempdir, not use overlong XDG path")
		assert.Contains(t, path, fmt.Sprintf("roborev-%d", os.Getuid()))
	})

	t.Run("UsesTempdirOtherwise", func(t *testing.T) {
		t.Setenv("XDG_RUNTIME_DIR", "")
		path := DefaultSocketPath()
		assert.Less(t, len(path), MaxUnixPathLen,
			"default socket path %q (%d bytes) exceeds limit %d",
			path, len(path), MaxUnixPathLen)
		assert.Contains(t, path, fmt.Sprintf("roborev-%d", os.Getuid()))
		assert.True(t, strings.HasSuffix(path, "daemon.sock"))
	})
}

func TestDaemonEndpoint_BaseURL(t *testing.T) {
	assert := assert.New(t)

	tcp := DaemonEndpoint{Network: "tcp", Address: "127.0.0.1:7373"}
	assert.Equal("http://127.0.0.1:7373", tcp.BaseURL())

	unix := DaemonEndpoint{Network: "unix", Address: "/tmp/test.sock"}
	assert.Equal("http://localhost", unix.BaseURL())
}

func TestDaemonEndpoint_String(t *testing.T) {
	assert := assert.New(t)

	tcp := DaemonEndpoint{Network: "tcp", Address: "127.0.0.1:7373"}
	assert.Equal("tcp:127.0.0.1:7373", tcp.String())

	unix := DaemonEndpoint{Network: "unix", Address: "/tmp/test.sock"}
	assert.Equal("unix:/tmp/test.sock", unix.String())
}

func TestDaemonEndpoint_ConfigAddrRoundTrips(t *testing.T) {
	assert := assert.New(t)

	tcp := DaemonEndpoint{Network: "tcp", Address: "127.0.0.1:7373"}
	parsed, err := ParseEndpoint(tcp.ConfigAddr())
	require.NoError(t, err)
	assert.Equal(tcp, parsed)

	if runtime.GOOS != "windows" {
		unix := DaemonEndpoint{Network: "unix", Address: "/tmp/test.sock"}
		parsed, err = ParseEndpoint(unix.ConfigAddr())
		require.NoError(t, err)
		assert.Equal(unix, parsed)
	}
}

func TestDaemonEndpoint_Listener_TCP(t *testing.T) {
	ep := DaemonEndpoint{Network: "tcp", Address: "127.0.0.1:0"}
	ln, err := ep.Listener()
	require.NoError(t, err)
	defer ln.Close()
	assert.Contains(t, ln.Addr().String(), "127.0.0.1:")
}

func TestDaemonEndpoint_Listener_Unix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix sockets not supported on Windows")
	}
	sockPath := filepath.Join("/tmp", fmt.Sprintf("roborev-test-%d.sock", os.Getpid()))
	t.Cleanup(func() { os.Remove(sockPath) })

	ep := DaemonEndpoint{Network: "unix", Address: sockPath}
	ln, err := ep.Listener()
	require.NoError(t, err)
	defer ln.Close()
	assert.Equal(t, sockPath, ln.Addr().String())
}

func TestDaemonEndpoint_HTTPClient_UnixRoundTrip(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix sockets not supported on Windows")
	}
	sockPath := filepath.Join("/tmp", fmt.Sprintf("roborev-test-%d.sock", os.Getpid()))
	t.Cleanup(func() { os.Remove(sockPath) })

	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", sockPath)
	require.NoError(t, err)
	defer ln.Close()

	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})}
	go srv.Serve(ln)
	defer srv.Close()

	ep := DaemonEndpoint{Network: "unix", Address: sockPath}
	client := ep.HTTPClient(2 * time.Second)
	getRequest, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ep.BaseURL()+"/test", nil)
	require.NoError(t, err)
	resp, err := client.Do(getRequest)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestDaemonEndpoint_HTTPClient_UnixIgnoresProxy(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix sockets not supported on Windows")
	}
	sockPath := filepath.Join("/tmp", fmt.Sprintf("roborev-test-%d.sock", os.Getpid()))
	t.Cleanup(func() { os.Remove(sockPath) })

	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", sockPath)
	require.NoError(t, err)
	defer ln.Close()

	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})}
	go srv.Serve(ln)
	defer srv.Close()

	// Set proxy env vars that would break the request if honored
	t.Setenv("HTTP_PROXY", "http://nonexistent-proxy.invalid:9999")
	t.Setenv("HTTPS_PROXY", "http://nonexistent-proxy.invalid:9999")

	ep := DaemonEndpoint{Network: "unix", Address: sockPath}
	client := ep.HTTPClient(2 * time.Second)
	getRequest, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ep.BaseURL()+"/test", nil)
	require.NoError(t, err)
	resp, err := client.Do(getRequest)
	require.NoError(t, err, "Unix socket client should bypass HTTP_PROXY")
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}
