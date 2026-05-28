package daemon

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/roborev/internal/config"
)

const daemonServiceName = "roborev"

// RuntimeInfo stores daemon runtime state
type RuntimeInfo struct {
	PID        int    `json:"pid"`
	Addr       string `json:"addr"`
	Port       int    `json:"port"`
	Network    string `json:"network"`
	Version    string `json:"version"`
	SourcePath string `json:"-"` // Path to the runtime file (not serialized, set by ListAllRuntimes)
}

// Endpoint returns a DaemonEndpoint for this runtime. An empty Network defaults to "tcp"
// for backwards compatibility with old runtime files that predate the Network field.
func (r RuntimeInfo) Endpoint() DaemonEndpoint {
	network := r.Network
	if network == "" {
		network = "tcp"
	}
	return DaemonEndpoint{Network: network, Address: r.Addr}
}

// PingInfo is the minimal daemon identity payload used for liveness probes.
type PingInfo struct {
	Service string `json:"service"`
	Version string `json:"version"`
	PID     int    `json:"pid,omitempty"`
}

// RuntimePath returns the path to the runtime info file for the current process
func RuntimePath() string {
	return RuntimePathForPID(os.Getpid())
}

// RuntimePathForPID returns the path to the runtime info file for a specific PID
func RuntimePathForPID(pid int) string {
	return filepath.Join(config.DataDir(), fmt.Sprintf("daemon.%d.json", pid))
}

// LegacyRuntimePath returns the old daemon.json path for migration
func LegacyRuntimePath() string {
	return filepath.Join(config.DataDir(), "daemon.json")
}

// WriteRuntime saves the daemon runtime info atomically.
// Uses write-to-temp-then-rename to prevent readers from seeing partial writes.
func WriteRuntime(ep DaemonEndpoint, version string) error {
	info := RuntimeInfo{
		PID:     os.Getpid(),
		Addr:    ep.Address,
		Port:    ep.Port(),
		Network: ep.Network,
		Version: version,
	}

	path := RuntimePath()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}

	// Write to temp file first for atomic creation
	tmpFile, err := os.CreateTemp(dir, "daemon.*.json.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmpFile.Name()

	// Clean up temp file on any error
	success := false
	defer func() {
		if !success {
			os.Remove(tmpPath)
		}
	}()

	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		return err
	}
	if err := tmpFile.Close(); err != nil {
		return err
	}

	// Atomic rename to final path
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}

	// Set permissions to 0644 explicitly. This intentionally ignores umask
	// because the runtime file must be readable by other processes (CLI commands
	// discovering the daemon). The file contains only PID/port/version, not secrets.
	if err := os.Chmod(path, 0o644); err != nil {
		return err
	}

	success = true
	return nil
}

// ReadRuntime reads the daemon runtime info for the current process
func ReadRuntime() (*RuntimeInfo, error) {
	return ReadRuntimeForPID(os.Getpid())
}

// ReadRuntimeForPID reads the daemon runtime info for a specific PID
func ReadRuntimeForPID(pid int) (*RuntimeInfo, error) {
	data, err := os.ReadFile(RuntimePathForPID(pid))
	if err != nil {
		return nil, err
	}

	var info RuntimeInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, err
	}

	return &info, nil
}

// RemoveRuntime removes the runtime info file for the current process
func RemoveRuntime() {
	os.Remove(RuntimePath())
}

// RemoveRuntimeForPID removes the runtime info file for a specific PID
func RemoveRuntimeForPID(pid int) {
	os.Remove(RuntimePathForPID(pid))
}

// ListAllRuntimes returns info for all daemon runtime files found.
// Sets SourcePath on each RuntimeInfo for proper cleanup.
// Continues scanning even if some files are unreadable (e.g., permission errors).
func ListAllRuntimes() ([]*RuntimeInfo, error) {
	dataDir := config.DataDir()

	// Use os.ReadDir instead of filepath.Glob to handle paths with glob metacharacters
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // No data dir yet, no runtimes
		}
		return nil, err
	}

	// Filter for daemon.*.json files
	var matches []string
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, "daemon.") && strings.HasSuffix(name, ".json") {
			matches = append(matches, filepath.Join(dataDir, name))
		}
	}

	// Also check for legacy daemon.json (already covered by the pattern above,
	// but keep explicit check for clarity)
	legacyPath := LegacyRuntimePath()
	if _, err := os.Stat(legacyPath); err == nil {
		// Check if already in matches (daemon.json matches daemon.*.json pattern)
		found := slices.Contains(matches, legacyPath)
		if !found {
			matches = append(matches, legacyPath)
		}
	}

	var runtimes []*RuntimeInfo
	for _, path := range matches {
		data, err := os.ReadFile(path)
		if err != nil {
			// Skip unreadable files (permission errors, file disappeared, etc.)
			// Don't abort the whole scan - there may be other valid daemon files
			continue
		}
		var info RuntimeInfo
		if err := json.Unmarshal(data, &info); err != nil {
			// Corrupted file - remove it
			os.Remove(path)
			continue
		}
		// Validate required fields - remove invalid entries
		if info.PID <= 0 || info.Addr == "" {
			os.Remove(path)
			continue
		}
		// Track source path for proper cleanup
		info.SourcePath = path
		runtimes = append(runtimes, &info)
	}
	return runtimes, nil
}

// GetAnyRunningDaemon returns info about a responsive daemon.
// Returns os.ErrNotExist if no responsive daemon is found.
func GetAnyRunningDaemon() (*RuntimeInfo, error) {
	runtimes, err := ListAllRuntimes()
	if err != nil {
		return nil, err
	}

	// Only return a daemon that's actually responding
	for _, info := range runtimes {
		if IsDaemonAlive(info.Endpoint()) {
			return info, nil
		}
	}

	return nil, os.ErrNotExist
}

// ProbeDaemon validates that a daemon endpoint is serving the roborev daemon.
// It prefers the lightweight /api/ping endpoint and falls back to /api/status
// for older daemon versions that do not implement /api/ping yet.
func ProbeDaemon(ep DaemonEndpoint, timeout time.Duration) (*PingInfo, error) {
	if ep.Address == "" {
		return nil, fmt.Errorf("empty daemon address")
	}
	if !ep.IsUnix() && !isLoopbackAddr(ep.Address) {
		return nil, fmt.Errorf("non-loopback daemon address: %s", ep.Address)
	}
	client := ep.HTTPClient(timeout)
	baseURL := ep.BaseURL()
	if info, shouldFallback, err := probeDaemonPing(client, baseURL); !shouldFallback {
		return info, err
	}
	return probeLegacyDaemonStatus(client, baseURL)
}

// IsDaemonAlive checks if a daemon at the given endpoint is actually responding.
// This is more reliable than checking PID and works cross-platform.
// Only allows loopback addresses (for TCP) to prevent SSRF via malicious runtime files.
// Uses retry logic to avoid misclassifying a slow or transiently failing daemon.
func IsDaemonAlive(ep DaemonEndpoint) bool {
	if ep.Address == "" {
		return false
	}

	// Try up to 2 times with a short delay between attempts
	for attempt := range 2 {
		if attempt > 0 {
			time.Sleep(200 * time.Millisecond)
		}
		if _, err := ProbeDaemon(ep, 1*time.Second); err == nil {
			return true
		}
	}
	return false
}

func probeDaemonPing(client *http.Client, baseURL string) (*PingInfo, bool, error) {
	resp, err := client.Get(baseURL + "/api/ping")
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		var info PingInfo
		if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
			return nil, false, fmt.Errorf("decode daemon ping: %w", err)
		}
		if info.Service != daemonServiceName {
			return nil, false, fmt.Errorf("unexpected daemon service %q", info.Service)
		}
		return &info, false, nil
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		return nil, true, nil
	default:
		return nil, false, fmt.Errorf("daemon ping returned %d", resp.StatusCode)
	}
}

func probeLegacyDaemonStatus(client *http.Client, baseURL string) (*PingInfo, error) {
	resp, err := client.Get(baseURL + "/api/status")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 500 && resp.StatusCode < 600 {
		return &PingInfo{Service: daemonServiceName}, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("daemon status returned %d", resp.StatusCode)
	}
	if resp.StatusCode == http.StatusNoContent {
		return &PingInfo{Service: daemonServiceName}, nil
	}

	var status struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return nil, fmt.Errorf("decode daemon status: %w", err)
	}
	if status.Version == "" {
		return nil, fmt.Errorf("daemon status missing version")
	}

	return &PingInfo{
		Service: daemonServiceName,
		Version: status.Version,
	}, nil
}

func parseDaemonBindAddr(addr string) (string, int, error) {
	if addr == "" {
		return "127.0.0.1", 7373, nil
	}

	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		return "", 0, fmt.Errorf("invalid daemon server address %q: %w", addr, err)
	}

	port, err := strconv.Atoi(portText)
	if err != nil {
		return "", 0, fmt.Errorf("invalid daemon server port %q: %w", portText, err)
	}

	return host, port, nil
}

// isLoopbackAddr checks if an address is a loopback address.
// Supports IPv4 (127.x.x.x), IPv6 (::1), and localhost.
// Uses strict parsing to prevent bypass via userinfo or hostname tricks.
func isLoopbackAddr(addr string) bool {
	// Use net.SplitHostPort for proper parsing (handles IPv6 brackets)
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// Maybe just a host without port
		host = addr
	}

	// Reject if host contains @ (userinfo bypass attempt)
	if strings.Contains(host, "@") {
		return false
	}

	// Check for localhost (exact match only)
	if host == "localhost" {
		return true
	}

	// Parse as IP and check if loopback
	ip := net.ParseIP(host)
	if ip == nil {
		return false // Not a valid IP and not "localhost"
	}

	return ip.IsLoopback()
}

// KillDaemon attempts to gracefully shut down a daemon, then force kill if needed.
// Returns true if the daemon was killed or is no longer running.
// Only removes runtime file if the daemon is confirmed dead.
func KillDaemon(info *RuntimeInfo) bool {
	if info == nil {
		return true
	}

	ep := info.Endpoint()

	// Helper to remove the runtime file using SourcePath if available, otherwise by PID.
	// Also cleans up Unix domain sockets.
	removeRuntimeFile := func() {
		if ep.IsUnix() {
			os.Remove(ep.Address)
		}
		if info.SourcePath != "" {
			os.Remove(info.SourcePath)
		} else if info.PID > 0 {
			RemoveRuntimeForPID(info.PID)
		}
	}

	// First try graceful HTTP shutdown
	if ep.Address != "" {
		client := ep.HTTPClient(2 * time.Second)
		resp, err := client.Post(ep.BaseURL()+"/api/shutdown", "application/json", nil)
		if err == nil {
			resp.Body.Close()
			// Wait for graceful shutdown
			for range 10 {
				time.Sleep(200 * time.Millisecond)
				if !IsDaemonAlive(ep) {
					removeRuntimeFile()
					return true
				}
			}
		}
	}

	// HTTP shutdown failed or timed out, try OS-level kill
	// Only do this if we have a valid PID
	if info.PID > 0 {
		if killProcess(info.PID) {
			removeRuntimeFile()
			return true
		}
		// Kill failed - don't remove runtime file, daemon may still be running
		return false
	}

	// No valid PID, just check if it's still alive
	if ep.Address != "" && !IsDaemonAlive(ep) {
		removeRuntimeFile()
		return true
	}

	return false
}

// CleanupZombieDaemons finds and kills all unresponsive daemons.
// Returns the number of zombies cleaned up.
func CleanupZombieDaemons(target DaemonEndpoint) int {
	runtimes, err := ListAllRuntimes()
	if err != nil {
		return 0
	}

	cleaned := 0
	for _, info := range runtimes {
		ep := info.Endpoint()

		// For Unix sockets, check PID liveness first to avoid slow HTTP probes
		// against sockets whose owner process is already dead.
		if ep.IsUnix() && info.PID > 0 && !isProcessAlive(info.PID) {
			if ep.Address != target.Address {
				// Clean up non-matching sockets.
				os.Remove(ep.Address)
			}
			if info.SourcePath != "" {
				os.Remove(info.SourcePath)
			} else {
				RemoveRuntimeForPID(info.PID)
			}
			cleaned++
			continue
		}

		// Skip responsive daemons
		if IsDaemonAlive(ep) {
			continue
		}

		// Unresponsive — try to kill it. When the zombie's
		// socket matches the target (e.g. a systemd-managed
		// socket we're about to serve on), kill the process
		// and clean up the runtime file but preserve the socket.
		if ep.IsUnix() && ep.Address == target.Address {
			if info.PID > 0 && !killProcess(info.PID) {
				// Could not confirm kill; leave runtime
				// metadata so the next attempt can retry.
				continue
			}
			if info.SourcePath != "" {
				os.Remove(info.SourcePath)
			} else if info.PID > 0 {
				RemoveRuntimeForPID(info.PID)
			}
			cleaned++
		} else if KillDaemon(info) {
			cleaned++
		}
	}

	// Clean up legacy daemon.json - it may contain stale info
	// that ListAllRuntimes picked up
	legacyPath := LegacyRuntimePath()
	if _, err := os.Stat(legacyPath); err == nil {
		// Read it to check if it's for a dead daemon
		if data, err := os.ReadFile(legacyPath); err == nil {
			var info RuntimeInfo
			if json.Unmarshal(data, &info) == nil {
				if !IsDaemonAlive(info.Endpoint()) {
					// Legacy file points to dead daemon, remove it
					os.Remove(legacyPath)
				}
			} else {
				// Corrupted, remove it
				os.Remove(legacyPath)
			}
		}
	}

	return cleaned
}

// FindAvailablePort finds an available port starting from the configured port.
// After zombie cleanup, this should usually succeed on the first try.
// Falls back to searching if the port is still in use (e.g., by another service).
func FindAvailablePort(startAddr string) (string, int, error) {
	host, port, err := parseDaemonBindAddr(startAddr)
	if err != nil {
		return "", 0, err
	}

	// Try ports starting from the configured one
	for i := range 100 {
		addr := net.JoinHostPort(host, strconv.Itoa(port+i))
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			actualPort := ln.Addr().(*net.TCPAddr).Port
			ln.Close()
			return net.JoinHostPort(host, strconv.Itoa(actualPort)), actualPort, nil
		}
	}

	return "", 0, fmt.Errorf("no available port found starting from %d", port)
}
