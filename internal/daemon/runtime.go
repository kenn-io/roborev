package daemon

import (
	"cmp"
	"context"
	"encoding/json/v2"
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

	"github.com/cenkalti/backoff/v7"
	kitdaemon "go.kenn.io/kit/daemon"

	"go.kenn.io/roborev/internal/auth"
	"go.kenn.io/roborev/internal/config"
	roborevclient "go.kenn.io/roborev/pkg/client"
)

const (
	daemonServiceName           = "roborev"
	runtimeAlternateNetworkKey  = "alternate_network"
	runtimeAlternateAddressKey  = "alternate_address"
	runtimeWebAddressKey        = "web_address"
	runtimeWebOriginKey         = "web_origin"
	runtimeWebBasePathKey       = "web_base_path"
	runtimeWebCapabilitiesKey   = "web_capabilities"
	runtimeWebDisabledReasonKey = "web_disabled_reason"
	runtimeTLSCertificateKey    = "tls_certificate"
)

// Reasons the daemon publishes when the browser listener is not running, so
// CLI commands can tell users how to get the web UI back.
const (
	// WebDisabledReasonConfig means [web] enabled = false in the config.
	WebDisabledReasonConfig = "config"
	// WebDisabledReasonMissingAssets means this binary was built without the
	// production web assets and cannot serve the browser application.
	WebDisabledReasonMissingAssets = "missing-web-assets"
)

// ErrDaemonAccessDenied means the daemon rejected authentication, its server
// identity could not be verified, or local permissions prevented every usable
// endpoint from being probed.
var ErrDaemonAccessDenied = auth.ErrUnverifiedServer

// ErrPlaintextAuthTransport means the client refused to probe a TCP endpoint
// because it could not verify the server before sending the shared key.
var ErrPlaintextAuthTransport = auth.ErrPlaintextTransport

var probeRuntimeEndpoint = probeRuntimeRecord

// RuntimeInfo stores daemon runtime state
type RuntimeInfo struct {
	PID               int      `json:"pid"`
	Network           string   `json:"network,omitempty"`
	Address           string   `json:"address"`
	Service           string   `json:"service,omitempty"`
	Version           string   `json:"version,omitempty"`
	SourcePath        string   `json:"-"` // Path to the runtime file (not serialized, set by ListAllRuntimes)
	AlternateNetwork  string   `json:"-"`
	AlternateAddress  string   `json:"-"`
	WebAddress        string   `json:"-"`
	WebOrigin         string   `json:"-"`
	WebBasePath       string   `json:"-"`
	WebCapabilities   []string `json:"-"`
	WebDisabledReason string   `json:"-"`
	TLSCertPEM        string   `json:"-"`

	processIdentity kitdaemon.ProcessIdentity
	legacyRecord    bool
}

// BrowserRuntimeInfo is the non-secret discovery information published for
// the daemon's optional browser listener. When the listener is not running,
// DisabledReason carries the machine-readable cause and all other fields are
// empty.
type BrowserRuntimeInfo struct {
	Address        string
	Origin         string
	WebBasePath    string
	Capabilities   []string
	DisabledReason string
}

// Endpoint returns a DaemonEndpoint for this runtime.
func (r RuntimeInfo) Endpoint() DaemonEndpoint {
	info := daemonEndpointFromKit(kitdaemon.RuntimeRecord{
		PID:     r.PID,
		Network: r.Network,
		Address: r.Address,
		Service: r.Service,
		Version: r.Version,
	}.Endpoint())
	info.TLSCertPEM = r.TLSCertPEM
	return info
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
	if err != nil || alternate.Network == primary.Network && alternate.Address == primary.Address {
		return endpoints
	}
	if alternate.Network == "tcp" {
		alternate.TLSCertPEM = r.TLSCertPEM
	}
	return append(endpoints, alternate)
}

// PingInfo is the minimal daemon identity payload used for liveness probes.
type PingInfo struct {
	OK      bool   `json:"ok"`
	Service string `json:"service"`
	Version string `json:"version"`
	PID     int    `json:"pid,omitempty"`
	MCPURL  string `json:"mcp_url,omitempty" doc:"Streamable HTTP MCP endpoint when [mcp] is enabled and the daemon listens on TCP"`
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

func runtimeInfoFromRecord(rec kitdaemon.RuntimeRecord) *RuntimeInfo {
	ep := daemonEndpointFromKit(rec.Endpoint())
	info := &RuntimeInfo{
		PID:              rec.PID,
		processIdentity:  cmp.Or(rec.ProcessIdentityV2, rec.ProcessIdentity),
		Network:          ep.Network,
		Address:          ep.Address,
		Service:          rec.Service,
		Version:          rec.Version,
		SourcePath:       rec.SourcePath,
		AlternateNetwork: rec.Metadata[runtimeAlternateNetworkKey],
		AlternateAddress: rec.Metadata[runtimeAlternateAddressKey],
		WebAddress:       rec.Metadata[runtimeWebAddressKey],
		WebOrigin:        rec.Metadata[runtimeWebOriginKey],
		WebBasePath:      rec.Metadata[runtimeWebBasePathKey],
		TLSCertPEM:       rec.Metadata[runtimeTLSCertificateKey],
	}
	info.WebDisabledReason = rec.Metadata[runtimeWebDisabledReasonKey]
	if info.WebOrigin != "" {
		info.WebDisabledReason = ""
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
	return WriteRuntimeWithTLS(primary, alternate, version, browser, "")
}

// WriteRuntimeWithTLS publishes the public certificate used to authenticate
// the daemon's TCP TLS listener. The private key and auth_key are never stored.
func WriteRuntimeWithTLS(
	primary DaemonEndpoint,
	alternate *DaemonEndpoint,
	version string,
	browser *BrowserRuntimeInfo,
	tlsCertPEM string,
) error {
	rec := kitdaemon.NewRuntimeRecord(daemonServiceName, version, primary.kitEndpoint())
	rec.Metadata = make(map[string]string)
	if tlsCertPEM != "" {
		if primary.Network != "tcp" {
			return fmt.Errorf("TLS certificate requires a TCP primary endpoint")
		}
		rec.Metadata[runtimeTLSCertificateKey] = tlsCertPEM
	}
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
	if browser != nil && browser.DisabledReason != "" {
		if browser.Address != "" || browser.Origin != "" ||
			browser.WebBasePath != "" || len(browser.Capabilities) > 0 {
			return fmt.Errorf("browser runtime cannot carry both listener fields and a disabled reason")
		}
		rec.Metadata[runtimeWebDisabledReasonKey] = browser.DisabledReason
	} else if browser != nil {
		if err := validateBrowserRuntime(*browser); err != nil {
			return err
		}
		rec.Metadata[runtimeWebAddressKey] = browser.Address
		rec.Metadata[runtimeWebOriginKey] = browser.Origin
		rec.Metadata[runtimeWebBasePathKey] = browser.WebBasePath
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
	basePath, err := config.NormalizeWebBasePath(browser.WebBasePath)
	if err != nil {
		return fmt.Errorf("browser runtime base path: %w", err)
	}
	if basePath != browser.WebBasePath {
		return fmt.Errorf("browser runtime base path is not canonical")
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
			PID:          legacy.PID,
			Network:      network,
			Address:      legacy.Addr,
			Version:      legacy.Version,
			SourcePath:   path,
			legacyRecord: true,
		})
	}
	return runtimes
}

func probeRuntimeRecord(ctx context.Context, ep DaemonEndpoint) (*PingInfo, error) {
	key, err := loadClientAuthKey()
	if err != nil {
		return nil, err
	}
	return probeRuntimeRecordWithKey(ctx, ep, key)
}

func probeRuntimeRecordWithKey(ctx context.Context, ep DaemonEndpoint, key string) (*PingInfo, error) {
	if ep.Address == "" {
		return nil, fmt.Errorf("empty daemon address")
	}
	if !ep.IsUnix() && !isLoopbackAddr(ep.Address) {
		return nil, fmt.Errorf("non-loopback daemon address: %s", ep.Address)
	}
	return probeDaemonHTTP(ctx, ep, time.Second, ep.HTTPClientWithAuthKey(time.Second, key))
}

// IsDaemonAccessError reports credential, configuration, transport-security,
// and local permission errors that must not trigger recovery of a recorded
// daemon or stale-runtime cleanup.
func IsDaemonAccessError(err error) bool {
	return errors.Is(err, ErrDaemonAccessDenied) || errors.Is(err, ErrPlaintextAuthTransport) ||
		errors.Is(err, ErrClientConfig) ||
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
		if info.HasStaleProcess() {
			continue
		}
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
			if IsDaemonAccessError(err) {
				deniedErr = fmt.Errorf("daemon at %s: %w", ep, err)
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
	if _, err := loadClientAuthKey(); err != nil {
		return nil, err
	}
	return getAnyRunningDaemonContext(ctx, probeRuntimeEndpoint)
}

func getAnyRunningDaemonContext(ctx context.Context, probe func(context.Context, DaemonEndpoint) (*PingInfo, error)) (*RuntimeInfo, error) {
	records, err := runtimeStore().List()
	if err != nil {
		return nil, err
	}
	return discoverRuntimeRecords(ctx, records, probe)
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
	return probeDaemonHTTP(context.Background(), ep, timeout, ep.HTTPClient(timeout))
}

// ProbeDaemonPing validates a daemon endpoint like ProbeDaemon but decodes
// the full roborev ping payload, including fields the shared kit probe does
// not model such as mcp_url.
func ProbeDaemonPing(ep DaemonEndpoint, timeout time.Duration) (*PingInfo, error) {
	if ep.Address == "" {
		return nil, fmt.Errorf("empty daemon address")
	}
	if !ep.IsUnix() && !isLoopbackAddr(ep.Address) {
		return nil, fmt.Errorf("non-loopback daemon address: %s", ep.Address)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	resp, err := ep.APIClient(timeout).PingRaw(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, ErrDaemonAccessDenied
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("daemon ping returned %d", resp.StatusCode)
	}
	var info PingInfo
	if err := json.UnmarshalRead(resp.Body, &info); err != nil {
		return nil, fmt.Errorf("decode daemon ping: %w", err)
	}
	if !info.OK {
		return nil, errors.New("daemon ping returned ok=false")
	}
	if info.Service != daemonServiceName {
		return nil, fmt.Errorf("unexpected daemon service %q", info.Service)
	}
	return &info, nil
}

// ProbeDaemonAlive checks if a daemon at the given endpoint is actually responding.
// This is more reliable than checking PID and works cross-platform.
// Only allows loopback addresses (for TCP) to prevent SSRF via malicious runtime files.
// Uses retry logic to avoid misclassifying a slow or transiently failing daemon.
func ProbeDaemonAlive(ep DaemonEndpoint) (bool, error) {
	if ep.Address == "" {
		return false, nil
	}

	alive, err := backoff.Retry(context.Background(), func() (bool, error) {
		if _, err := probeRuntimeEndpoint(context.Background(), ep); err == nil {
			return true, nil
		} else if IsDaemonAccessError(err) {
			return false, backoff.Permanent(fmt.Errorf("daemon at %s: %w", ep, err))
		} else {
			return false, err
		}
	}, backoff.WithBackOff(backoff.NewConstantBackOff(200*time.Millisecond)),
		backoff.WithMaxTries(2), backoff.WithMaxElapsedTime(0))
	if err != nil {
		return false, backoff.AsRetryError(err).LastErr
	}
	return alive, nil
}

// IsDaemonAlive checks if a daemon at the given endpoint is actually responding.
func IsDaemonAlive(ep DaemonEndpoint) bool {
	alive, _ := ProbeDaemonAlive(ep)
	return alive
}

// HasStaleProcess reports whether the recorded process exited or its PID now
// identifies a different process. An unknown identity is not proof of a
// mismatch; older records may lack it.
func (r *RuntimeInfo) HasStaleProcess() bool {
	return r.PID > 0 && (!kitdaemon.ProcessAlive(r.PID) ||
		kitdaemon.CompareProcessIdentity(r.PID, r.processIdentity) == kitdaemon.ProcessIdentityMismatch)
}

func probeRuntimeAlive(info *RuntimeInfo) (bool, error) {
	if info.HasStaleProcess() {
		return false, nil
	}
	var deniedErr error
	for _, ep := range info.Endpoints() {
		alive, err := ProbeDaemonAlive(ep)
		if alive {
			return true, nil
		}
		if IsDaemonAccessError(err) {
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
// Returns nil if the daemon is no longer running.
// Only removes runtime file if the daemon is confirmed dead.
func KillDaemon(info *RuntimeInfo) error {
	if info == nil {
		return nil
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
		alive, err := ProbeDaemonAlive(ep)
		return !alive && !IsDaemonAccessError(err)
	}
	if info.HasStaleProcess() || confirmedDead() {
		removeRuntimeFile()
		return nil
	}
	if info.PID > 0 {
		switch identifyProcess(info.PID) {
		case processNotRoborev:
			removeRuntimeFile()
			return nil
		case processUnknown:
			ping, err := ProbeDaemon(ep, 2*time.Second)
			if err != nil {
				return err
			}
			if ping.PID != info.PID {
				removeRuntimeFile()
				return nil
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
		shutdownErr := requestGracefulDaemonShutdown(shutdownCleanupCtx, ep, confirmedDead)
		if errors.Is(shutdownErr, auth.ErrPlaintextTransport) {
			if !confirmedDead() {
				if err := requestUnauthenticatedDaemonShutdown(shutdownCleanupCtx, info, ep, confirmedDead); err != nil && !confirmedDead() {
					return err
				}
			}
		} else if shutdownErr != nil {
			return shutdownErr
		}
		if !confirmedDead() {
			waitForGracefulDaemonExit(200*time.Millisecond, confirmedDead)
		}
		removeRuntimeFile()
		return nil
	}
	return fmt.Errorf("daemon shutdown was not accepted")
}

func requestUnauthenticatedDaemonShutdown(
	ctx context.Context,
	info *RuntimeInfo,
	ep DaemonEndpoint,
	confirmedDead func() bool,
) error {
	legacyService := info != nil && info.legacyRecord && info.Service == ""
	if info == nil || info.PID <= 0 || info.Network != "tcp" || !isLoopbackAddr(info.Address) ||
		(info.Service != daemonServiceName && !legacyService) ||
		ep.TLSCertPEM != "" || identifyProcess(info.PID) != processIsRoborev {
		return fmt.Errorf("%w: refusing unauthenticated shutdown for an unverified local daemon", ErrDaemonAccessDenied)
	}
	identityStatus := kitdaemon.CompareProcessIdentity(info.PID, info.processIdentity)
	legacyIdentityUnknown := info.legacyRecord && info.processIdentity == "" && identityStatus == kitdaemon.ProcessIdentityUnknown
	if identityStatus != kitdaemon.ProcessIdentityMatch && !legacyIdentityUnknown {
		return fmt.Errorf("%w: refusing unauthenticated shutdown for an unverified local daemon", ErrDaemonAccessDenied)
	}

	// This path is only for verified local daemons that started before auth_key
	// was enabled. Send no credential. Pre-kit daemons predate /api/ping, so the
	// legacy runtime shape and positive process identity verify those records.
	client := &http.Client{
		Transport: http.DefaultTransport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	if !info.legacyRecord {
		ping, err := probeDaemonHTTP(ctx, ep, 2*time.Second, client)
		if err != nil {
			return err
		}
		if ping.PID != info.PID {
			return fmt.Errorf("%w: daemon endpoint does not match its runtime process", ErrDaemonAccessDenied)
		}
	}
	api, err := roborevclient.NewWithHTTPClient(ep.BaseURL(), client)
	if err != nil {
		return fmt.Errorf("create unauthenticated daemon client: %w", err)
	}
	return requestGracefulDaemonShutdownWithClient(ctx, confirmedDead, api)
}

func requestGracefulDaemonShutdown(
	ctx context.Context,
	ep DaemonEndpoint,
	confirmedDead func() bool,
) error {
	return requestGracefulDaemonShutdownWithClient(ctx, confirmedDead, ep.APIClient(0))
}

func requestGracefulDaemonShutdownWithClient(
	ctx context.Context,
	confirmedDead func() bool,
	client *roborevclient.Client,
) error {
	_, err := backoff.Retry(ctx, func() (struct{}, error) {
		if confirmedDead() {
			return struct{}{}, nil
		}
		resp, err := client.ShutdownRaw(ctx)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
				return struct{}{}, nil
			}
			if resp.StatusCode == http.StatusUnauthorized {
				return struct{}{}, backoff.Permanent(ErrDaemonAccessDenied)
			}
			err = fmt.Errorf("daemon shutdown returned %s", resp.Status)
			if resp.StatusCode < http.StatusInternalServerError {
				return struct{}{}, backoff.Permanent(err)
			}
		}
		if confirmedDead() {
			return struct{}{}, nil
		}
		if IsDaemonAccessError(err) {
			return struct{}{}, backoff.Permanent(err)
		}
		return struct{}{}, err
	}, backoff.WithBackOff(backoff.NewConstantBackOff(shutdownCleanupRetryInterval)),
		backoff.WithMaxTries(0), backoff.WithMaxElapsedTime(0))
	if err != nil && !confirmedDead() {
		return backoff.AsRetryError(err).LastErr
	}
	return nil
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

		// Check the recorded process before any endpoint receives credentials.
		if info.HasStaleProcess() {
			if ep.IsUnix() && ep.Address != target.Address && !kitdaemon.ProcessAlive(info.PID) {
				// Clean up non-matching sockets only when the PID is dead.
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
		if IsDaemonAccessError(probeErr) {
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
		if info.PID <= 0 && KillDaemon(info) == nil {
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
