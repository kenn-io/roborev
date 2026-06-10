package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	kitdaemon "go.kenn.io/kit/daemon"

	"go.kenn.io/roborev/internal/config"
)

const daemonServiceName = "roborev"

// RuntimeInfo stores daemon runtime state
type RuntimeInfo struct {
	PID        int    `json:"pid"`
	Network    string `json:"network,omitempty"`
	Address    string `json:"address"`
	Service    string `json:"service,omitempty"`
	Version    string `json:"version,omitempty"`
	SourcePath string `json:"-"` // Path to the runtime file (not serialized, set by ListAllRuntimes)
}

// Endpoint returns a DaemonEndpoint for this runtime.
func (r RuntimeInfo) Endpoint() DaemonEndpoint {
	return daemonEndpointFromKit(kitdaemon.RuntimeRecord{
		PID:     r.PID,
		Network: r.Network,
		Address: r.Address,
		Service: r.Service,
		Version: r.Version,
	}.Endpoint())
}

// PingInfo is the minimal daemon identity payload used for liveness probes.
type PingInfo struct {
	OK      bool   `json:"ok"`
	Service string `json:"service"`
	Version string `json:"version"`
	PID     int    `json:"pid,omitempty"`
}

func runtimeStore() kitdaemon.RuntimeStore {
	return kitdaemon.RuntimeStore{
		Dir:    filepath.Join(config.DataDir(), "runtime"),
		Prefix: "daemon",
	}
}

// RuntimeStore returns the shared kit runtime store used by roborev daemon
// discovery and startup coordination.
func RuntimeStore() kitdaemon.RuntimeStore {
	return runtimeStore()
}

// DiscoverOptions returns the shared kit discovery options for roborev.
func DiscoverOptions(timeout time.Duration) kitdaemon.DiscoverOptions {
	return kitdaemon.DiscoverOptions{
		Probe: kitdaemon.ProbeOptions{
			ExpectedService: daemonServiceName,
			Timeout:         timeout,
		},
	}
}

func runtimeInfoFromRecord(rec kitdaemon.RuntimeRecord) *RuntimeInfo {
	ep := daemonEndpointFromKit(rec.Endpoint())
	return &RuntimeInfo{
		PID:        rec.PID,
		Network:    ep.Network,
		Address:    ep.Address,
		Service:    rec.Service,
		Version:    rec.Version,
		SourcePath: rec.SourcePath,
	}
}

func pingInfoFromKit(info kitdaemon.PingInfo) *PingInfo {
	return &PingInfo{
		OK:      info.OK,
		Service: info.Service,
		Version: info.Version,
		PID:     info.PID,
	}
}

// RuntimePath returns the path to the runtime info file for the current process
func RuntimePath() string {
	return RuntimePathForPID(os.Getpid())
}

// RuntimePathForPID returns the path to the runtime info file for a specific PID
func RuntimePathForPID(pid int) string {
	path, err := runtimeStore().Path(pid)
	if err != nil {
		return filepath.Join(config.DataDir(), "runtime", fmt.Sprintf("daemon.%d.json", pid))
	}
	return path
}

// WriteRuntime saves the daemon runtime info atomically.
// Uses write-to-temp-then-rename to prevent readers from seeing partial writes.
func WriteRuntime(ep DaemonEndpoint, version string) error {
	rec := kitdaemon.NewRuntimeRecord(daemonServiceName, version, ep.kitEndpoint())
	_, err := runtimeStore().Write(rec)
	return err
}

// ReadRuntime reads the daemon runtime info for the current process
func ReadRuntime() (*RuntimeInfo, error) {
	return ReadRuntimeForPID(os.Getpid())
}

// ReadRuntimeForPID reads the daemon runtime info for a specific PID
func ReadRuntimeForPID(pid int) (*RuntimeInfo, error) {
	rec, err := runtimeStore().Read(RuntimePathForPID(pid))
	if err != nil {
		return nil, err
	}
	return runtimeInfoFromRecord(rec), nil
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
	records, err := runtimeStore().List()
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	runtimes := make([]*RuntimeInfo, 0, len(records))
	seen := make(map[int]struct{}, len(records))
	for _, rec := range records {
		runtimes = append(runtimes, runtimeInfoFromRecord(rec))
		seen[rec.PID] = struct{}{}
	}
	for _, info := range listLegacyRuntimes() {
		if _, ok := seen[info.PID]; ok {
			continue
		}
		seen[info.PID] = struct{}{}
		runtimes = append(runtimes, info)
	}
	return runtimes, nil
}

// legacyRuntimeInfo is the on-disk shape written by roborev v0.56 and earlier,
// which stored daemon.<pid>.json and daemon.json in the data dir root.
type legacyRuntimeInfo struct {
	PID     int    `json:"pid"`
	Addr    string `json:"addr"`
	Network string `json:"network"`
	Version string `json:"version"`
}

// listLegacyRuntimes returns runtime records written by roborev v0.56 and
// earlier. Those daemons predate /api/ping, so kit discovery can never see
// them; they are surfaced here so stop, update, and zombie cleanup can
// terminate them by PID after an upgrade. Malformed files are skipped.
func listLegacyRuntimes() []*RuntimeInfo {
	dir := config.DataDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var runtimes []*RuntimeInfo
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if name != "daemon.json" &&
			(!strings.HasPrefix(name, "daemon.") || !strings.HasSuffix(name, ".json")) {
			continue
		}
		path := filepath.Join(dir, name)
		body, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var legacy legacyRuntimeInfo
		if err := json.Unmarshal(body, &legacy); err != nil || legacy.PID <= 0 || legacy.Addr == "" {
			continue
		}
		network := legacy.Network
		if network == "" {
			network = "tcp"
		}
		runtimes = append(runtimes, &RuntimeInfo{
			PID:        legacy.PID,
			Network:    network,
			Address:    legacy.Addr,
			Version:    legacy.Version,
			SourcePath: path,
		})
	}
	return runtimes
}

// GetAnyRunningDaemon returns info about a responsive daemon.
// Returns os.ErrNotExist if no responsive daemon is found.
func GetAnyRunningDaemon() (*RuntimeInfo, error) {
	rec, _, ok, err := kitdaemon.Discover(context.Background(), runtimeStore(), kitdaemon.DiscoverOptions{
		Probe: kitdaemon.ProbeOptions{
			ExpectedService: daemonServiceName,
			Timeout:         time.Second,
		},
	})
	if err != nil {
		return nil, err
	}
	if ok {
		return runtimeInfoFromRecord(rec), nil
	}

	return nil, os.ErrNotExist
}

// ProbeDaemon validates that a daemon endpoint is serving the roborev daemon.
func ProbeDaemon(ep DaemonEndpoint, timeout time.Duration) (*PingInfo, error) {
	if ep.Address == "" {
		return nil, fmt.Errorf("empty daemon address")
	}
	if !ep.IsUnix() && !isLoopbackAddr(ep.Address) {
		return nil, fmt.Errorf("non-loopback daemon address: %s", ep.Address)
	}
	info, err := kitdaemon.Probe(context.Background(), ep.kitEndpoint(), kitdaemon.ProbeOptions{
		ExpectedService: daemonServiceName,
		Timeout:         timeout,
	})
	if err != nil {
		return nil, err
	}
	return pingInfoFromKit(info), nil
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

	// Confirmed dead means no ping response AND, when a PID is known, the
	// process is gone. Legacy (pre-v0.57) daemons never answer /api/ping, so
	// the HTTP check alone would declare them dead while they still run.
	confirmedDead := func() bool {
		if info.PID > 0 && isProcessAlive(info.PID) {
			return false
		}
		return !IsDaemonAlive(ep)
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
				if confirmedDead() {
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
