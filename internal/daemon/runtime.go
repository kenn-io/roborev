package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	kitdaemon "go.kenn.io/kit/daemon"

	"go.kenn.io/roborev/internal/config"
)

const (
	daemonServiceName          = "roborev"
	runtimeAlternateNetworkKey = "alternate_network"
	runtimeAlternateAddressKey = "alternate_address"
	runtimeWebAddressKey       = "web_address"
	runtimeWebOriginKey        = "web_origin"
	runtimeWebCapabilitiesKey  = "web_capabilities"
)

// ErrDaemonAccessDenied means a daemon runtime was found but local permissions
// prevented every usable endpoint from being probed.
var ErrDaemonAccessDenied = errors.New("daemon access denied")

var probeRuntimeEndpoint = probeRuntimeRecord

// RuntimeInfo stores daemon runtime state
type RuntimeInfo struct {
	PID              int      `json:"pid"`
	Network          string   `json:"network,omitempty"`
	Address          string   `json:"address"`
	Service          string   `json:"service,omitempty"`
	Version          string   `json:"version,omitempty"`
	SourcePath       string   `json:"-"` // Path to the runtime file (not serialized, set by ListAllRuntimes)
	AlternateNetwork string   `json:"-"`
	AlternateAddress string   `json:"-"`
	WebAddress       string   `json:"-"`
	WebOrigin        string   `json:"-"`
	WebCapabilities  []string `json:"-"`
}

// BrowserRuntimeInfo is the non-secret discovery information published for
// the daemon's optional browser listener.
type BrowserRuntimeInfo struct {
	Address      string
	Origin       string
	Capabilities []string
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

// Endpoints returns the primary endpoint followed by a valid distinct
// alternate endpoint published in runtime metadata.
func (r RuntimeInfo) Endpoints() []DaemonEndpoint {
	primary := r.Endpoint()
	endpoints := []DaemonEndpoint{primary}
	if r.AlternateNetwork == "" || r.AlternateAddress == "" {
		return endpoints
	}

	var raw string
	switch r.AlternateNetwork {
	case "tcp":
		raw = r.AlternateAddress
	case "unix":
		raw = "unix://" + r.AlternateAddress
	default:
		return endpoints
	}
	alternate, err := ParseEndpoint(raw)
	if err != nil || alternate == primary {
		return endpoints
	}
	return append(endpoints, alternate)
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
	info := &RuntimeInfo{
		PID:              rec.PID,
		Network:          ep.Network,
		Address:          ep.Address,
		Service:          rec.Service,
		Version:          rec.Version,
		SourcePath:       rec.SourcePath,
		AlternateNetwork: rec.Metadata[runtimeAlternateNetworkKey],
		AlternateAddress: rec.Metadata[runtimeAlternateAddressKey],
		WebAddress:       rec.Metadata[runtimeWebAddressKey],
		WebOrigin:        rec.Metadata[runtimeWebOriginKey],
	}
	if raw := rec.Metadata[runtimeWebCapabilitiesKey]; raw != "" {
		info.WebCapabilities = strings.Split(raw, ",")
	}
	return info
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
func WriteRuntime(primary DaemonEndpoint, alternate *DaemonEndpoint, version string, browser *BrowserRuntimeInfo) error {
	rec := kitdaemon.NewRuntimeRecord(daemonServiceName, version, primary.kitEndpoint())
	rec.Metadata = make(map[string]string)
	if alternate != nil {
		info := RuntimeInfo{
			Network:          primary.Network,
			Address:          primary.Address,
			AlternateNetwork: alternate.Network,
			AlternateAddress: alternate.Address,
		}
		if len(info.Endpoints()) == 2 {
			rec.Metadata[runtimeAlternateNetworkKey] = alternate.Network
			rec.Metadata[runtimeAlternateAddressKey] = alternate.Address
		}
	}
	if browser != nil {
		if err := validateBrowserRuntime(*browser); err != nil {
			return err
		}
		rec.Metadata[runtimeWebAddressKey] = browser.Address
		rec.Metadata[runtimeWebOriginKey] = browser.Origin
		rec.Metadata[runtimeWebCapabilitiesKey] = strings.Join(browser.Capabilities, ",")
	}
	if len(rec.Metadata) == 0 {
		rec.Metadata = nil
	}
	_, err := runtimeStore().Write(rec)
	return err
}

func validateBrowserRuntime(browser BrowserRuntimeInfo) error {
	if strings.TrimSpace(browser.Address) == "" || strings.TrimSpace(browser.Origin) == "" {
		return fmt.Errorf("browser runtime address and origin are required")
	}
	seen := make(map[string]struct{}, len(browser.Capabilities))
	for _, capability := range browser.Capabilities {
		if capability == "" || capability != strings.TrimSpace(capability) || strings.Contains(capability, ",") {
			return fmt.Errorf("invalid browser capability %q", capability)
		}
		if _, found := seen[capability]; found {
			return fmt.Errorf("duplicate browser capability %q", capability)
		}
		seen[capability] = struct{}{}
	}
	return nil
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

func probeRuntimeRecord(ctx context.Context, ep DaemonEndpoint) (*PingInfo, error) {
	if ep.Address == "" {
		return nil, fmt.Errorf("empty daemon address")
	}
	if !ep.IsUnix() && !isLoopbackAddr(ep.Address) {
		return nil, fmt.Errorf("non-loopback daemon address: %s", ep.Address)
	}
	info, err := kitdaemon.Probe(ctx, ep.kitEndpoint(), kitdaemon.ProbeOptions{
		ExpectedService: daemonServiceName,
		Timeout:         time.Second,
	})
	if err != nil {
		return nil, err
	}
	return pingInfoFromKit(info), nil
}

// IsDaemonAccessDenied reports whether err is roborev's access-denied sentinel
// or an operating-system permission error from a local endpoint probe.
func IsDaemonAccessDenied(err error) bool {
	return errors.Is(err, ErrDaemonAccessDenied) ||
		errors.Is(err, os.ErrPermission) ||
		errors.Is(err, syscall.EACCES) ||
		errors.Is(err, syscall.EPERM)
}

func discoverRuntimeRecords(
	ctx context.Context,
	records []kitdaemon.RuntimeRecord,
	probe func(context.Context, DaemonEndpoint) (*PingInfo, error),
) (*RuntimeInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var deniedErr error
	for _, rec := range records {
		info := runtimeInfoFromRecord(rec)
		primary := info.Endpoint()
		for _, ep := range info.Endpoints() {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			ping, err := probe(ctx, ep)
			if err == nil && ping != nil && ping.PID != 0 && ping.PID == rec.PID {
				info.Network = ep.Network
				info.Address = ep.Address
				if ep != primary {
					info.AlternateNetwork = primary.Network
					info.AlternateAddress = primary.Address
				}
				return info, nil
			}
			if IsDaemonAccessDenied(err) {
				deniedErr = fmt.Errorf("%w at %s: %w", ErrDaemonAccessDenied, ep, err)
			}
		}
	}
	if deniedErr != nil {
		return nil, deniedErr
	}
	return nil, os.ErrNotExist
}

// GetAnyRunningDaemonContext returns info about a responsive daemon.
// Returns os.ErrNotExist if no responsive daemon is found.
func GetAnyRunningDaemonContext(ctx context.Context) (*RuntimeInfo, error) {
	records, err := runtimeStore().List()
	if err != nil {
		return nil, err
	}
	return discoverRuntimeRecords(ctx, records, probeRuntimeEndpoint)
}

// GetAnyRunningDaemon returns info about a responsive daemon.
// Returns os.ErrNotExist if no responsive daemon is found.
func GetAnyRunningDaemon() (*RuntimeInfo, error) {
	return GetAnyRunningDaemonContext(context.Background())
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

// ProbeDaemonAlive checks if a daemon at the given endpoint is actually responding.
// This is more reliable than checking PID and works cross-platform.
// Only allows loopback addresses (for TCP) to prevent SSRF via malicious runtime files.
// Uses retry logic to avoid misclassifying a slow or transiently failing daemon.
func ProbeDaemonAlive(ep DaemonEndpoint) (bool, error) {
	if ep.Address == "" {
		return false, nil
	}

	var lastErr error
	for attempt := range 2 {
		if attempt > 0 {
			time.Sleep(200 * time.Millisecond)
		}
		if _, err := probeRuntimeEndpoint(context.Background(), ep); err == nil {
			return true, nil
		} else if IsDaemonAccessDenied(err) {
			return false, fmt.Errorf("%w at %s: %w", ErrDaemonAccessDenied, ep, err)
		} else {
			lastErr = err
		}
	}
	return false, lastErr
}

// IsDaemonAlive checks if a daemon at the given endpoint is actually responding.
func IsDaemonAlive(ep DaemonEndpoint) bool {
	alive, _ := ProbeDaemonAlive(ep)
	return alive
}

func probeRuntimeAlive(info *RuntimeInfo) (bool, error) {
	var deniedErr error
	for _, ep := range info.Endpoints() {
		alive, err := ProbeDaemonAlive(ep)
		if alive {
			return true, nil
		}
		if IsDaemonAccessDenied(err) {
			deniedErr = err
		}
	}
	return false, deniedErr
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

// KillDaemon requests graceful shutdown and waits until the daemon exits.
// Returns true if the daemon is no longer running.
// Only removes runtime file if the daemon is confirmed dead.
func KillDaemon(info *RuntimeInfo) bool {
	if info == nil {
		return true
	}

	ep := info.Endpoint()

	// Remove only this process's runtime record. The endpoint may already belong
	// to a service-manager replacement.
	removeRuntimeFile := func() {
		if info.SourcePath != "" {
			os.Remove(info.SourcePath)
		} else if info.PID > 0 {
			RemoveRuntimeForPID(info.PID)
		}
	}

	// When a PID is known, confirm that exact process exited. A service manager
	// may start a replacement daemon on the same endpoint immediately.
	confirmedDead := func() bool {
		if info.PID > 0 {
			return !isProcessAlive(info.PID)
		}
		return !IsDaemonAlive(ep)
	}
	if confirmedDead() {
		removeRuntimeFile()
		return true
	}
	if info.PID > 0 {
		switch identifyProcess(info.PID) {
		case processNotRoborev:
			removeRuntimeFile()
			return true
		case processUnknown:
			ping, err := ProbeDaemon(ep, 2*time.Second)
			if err != nil {
				return false
			}
			if ping.PID != info.PID {
				removeRuntimeFile()
				return true
			}
		}
	}

	// Request graceful shutdown within one shared preparation budget. Once the
	// daemon accepts the request, wait without a deadline for the exact process
	// so running reviews still have unlimited time to finish.
	if ep.Address != "" {
		shutdownCleanupCtx, cancelShutdownCleanup := context.WithTimeout(
			context.Background(), shutdownCleanupTimeout,
		)
		defer cancelShutdownCleanup()
		if requestGracefulDaemonShutdown(shutdownCleanupCtx, ep, confirmedDead) {
			waitForGracefulDaemonExit(200*time.Millisecond, confirmedDead)
			removeRuntimeFile()
			return true
		}
	}
	return false
}

func requestGracefulDaemonShutdown(
	ctx context.Context,
	ep DaemonEndpoint,
	confirmedDead func() bool,
) bool {
	client := ep.HTTPClient(0)
	for {
		if confirmedDead() {
			return true
		}
		req, err := http.NewRequestWithContext(
			ctx, http.MethodPost, ep.BaseURL()+"/api/shutdown", nil,
		)
		if err != nil {
			return false
		}
		resp, err := client.Do(req)
		if err == nil {
			accepted := resp.StatusCode >= http.StatusOK &&
				resp.StatusCode < http.StatusMultipleChoices
			retryable := resp.StatusCode >= http.StatusInternalServerError
			resp.Body.Close()
			if accepted {
				return true
			}
			if !retryable {
				return false
			}
		}
		if confirmedDead() {
			return true
		}
		select {
		case <-ctx.Done():
			return confirmedDead()
		case <-time.After(shutdownCleanupRetryInterval):
		}
	}
}

func waitForGracefulDaemonExit(
	pollInterval time.Duration, confirmedDead func() bool,
) {
	for !confirmedDead() {
		time.Sleep(pollInterval)
	}
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

		alive, probeErr := probeRuntimeAlive(info)
		if alive {
			continue
		}
		if IsDaemonAccessDenied(probeErr) {
			continue
		}
		if info.PID > 0 && identifyProcess(info.PID) == processNotRoborev {
			if ep.IsUnix() && ep.Address != target.Address {
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

		// Never stop an unresponsive live process during cleanup: it may be
		// running a review. Records without a PID are safe to remove only when
		// their endpoint is also confirmed dead.
		if info.PID <= 0 && KillDaemon(info) {
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
