package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
)

// DefaultKataTeammate names roborev's own writes in Kata when no teammate is
// configured.
const DefaultKataTeammate = "roborev"

// kataTeammatePattern matches Kata v0.19.0's 1-64 character handle rule
// (internal/teammate/teammate.go:10-17).
var kataTeammatePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// IntegrationsConfig groups the optional connections to other services.
type IntegrationsConfig struct {
	Kata KataIntegrationConfig `toml:"kata"`
}

// KataIntegrationConfig is the daemon's optional connection to a Kata server.
// It is global-only: a repository cannot choose where credentials are sent.
// When enabled, every Kata use goes through this connection; when disabled,
// roborev keeps running the kata CLI as before.
type KataIntegrationConfig struct {
	Enabled        bool   `toml:"enabled" comment:"Connect the daemon to Kata's API instead of running the kata CLI. Requires endpoint."`
	Endpoint       string `toml:"endpoint" comment:"Kata API endpoint: https://host, loopback http://localhost:port, or unix:///path/to/socket."`
	APIKey         string `toml:"api_key" json:"-" sensitive:"true" comment:"Bearer credential for Kata. Prefer api_key_file; masked in config output."`
	APIKeyFile     string `toml:"api_key_file" comment:"File holding the Kata credential, read by the daemon. A leading ~/ is the home directory."`
	DefaultProject string `toml:"default_project" comment:"Kata project used when a repository has no .kata.local.toml or .kata.toml binding."`
	Teammate       string `toml:"teammate" comment:"Teammate name Kata records on comments roborev posts (default roborev)."`
}

// KataEndpointKind classifies an accepted Kata endpoint.
type KataEndpointKind string

// Accepted endpoint kinds.
const (
	KataEndpointHTTPS        KataEndpointKind = "https"
	KataEndpointLoopbackHTTP KataEndpointKind = "loopback_http"
	KataEndpointUnix         KataEndpointKind = "unix"
)

// ErrKataEndpoint marks an endpoint that is malformed or not allowed.
var ErrKataEndpoint = errors.New("kata endpoint is not allowed")

// KataEndpoint is a validated endpoint: the base URL to dial (always
// http://unix for a socket), its kind, and the socket path for unix endpoints.
type KataEndpoint struct {
	URL        *url.URL
	Kind       KataEndpointKind
	SocketPath string
}

// ParseKataEndpoint accepts https://host, loopback http://host (localhost or a
// loopback IP), or unix:///absolute/socket/path with no host. Userinfo, query
// strings, and fragments are rejected for every scheme, and remote plaintext
// HTTP is never accepted. It checks the shape only, so configuration loading
// needs neither the filesystem nor a credential.
func ParseKataEndpoint(raw string) (KataEndpoint, error) {
	reject := func(format string, args ...any) (KataEndpoint, error) {
		return KataEndpoint{}, fmt.Errorf("%w: %s", ErrKataEndpoint, fmt.Sprintf(format, args...))
	}
	if raw == "" {
		return reject("endpoint is empty")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" {
		return reject("endpoint must be an absolute URL")
	}
	if u.User != nil {
		return reject("endpoint must not contain userinfo")
	}
	if u.Fragment != "" || strings.Contains(raw, "#") {
		return reject("endpoint must not contain a fragment")
	}
	if u.RawQuery != "" || u.ForceQuery {
		return reject("endpoint must not contain a query string")
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		if u.Hostname() == "" {
			return reject("https endpoint requires a host")
		}
		return KataEndpoint{URL: u, Kind: KataEndpointHTTPS}, nil
	case "http":
		if u.Hostname() == "" || !isLoopbackHost(u.Hostname()) {
			return reject("remote plaintext HTTP is not allowed; use https:// or a loopback host such as http://localhost")
		}
		return KataEndpoint{URL: u, Kind: KataEndpointLoopbackHTTP}, nil
	case "unix":
		if u.Host != "" || !strings.HasPrefix(u.Path, "/") || strings.IndexFunc(u.Path, unicode.IsControl) >= 0 {
			return reject("unix endpoint requires an absolute socket path and no host (unix:///path/to/socket)")
		}
		return KataEndpoint{
			URL:        &url.URL{Scheme: "http", Host: "unix"},
			Kind:       KataEndpointUnix,
			SocketPath: u.Path,
		}, nil
	default:
		return reject("unsupported scheme %q (use https, http, or unix)", u.Scheme)
	}
}

// ParsedEndpoint parses the configured endpoint.
func (k KataIntegrationConfig) ParsedEndpoint() (KataEndpoint, error) {
	return ParseKataEndpoint(strings.TrimSpace(k.Endpoint))
}

// ResolvedTeammate returns the configured teammate, or the default.
func (k KataIntegrationConfig) ResolvedTeammate() string {
	if teammate := strings.TrimSpace(k.Teammate); teammate != "" {
		return teammate
	}
	return DefaultKataTeammate
}

// ValidateKataTeammate checks a name against Kata's teammate rule.
func ValidateKataTeammate(name string) error {
	if !kataTeammatePattern.MatchString(name) {
		return fmt.Errorf("teammate %q must match %s", name, kataTeammatePattern)
	}
	return nil
}

// Validate checks an enabled connection. A disabled one is not validated, so a
// stale endpoint does not block unrelated work. The credential file is read
// when the daemon connects, not here.
func (k KataIntegrationConfig) Validate() error {
	if !k.Enabled {
		return nil
	}
	endpoint, err := k.ParsedEndpoint()
	if err != nil {
		return fmt.Errorf("endpoint: %w", err)
	}
	if k.APIKey != "" && k.APIKeyFile != "" {
		return errors.New("api_key and api_key_file are mutually exclusive")
	}
	if endpoint.Kind == KataEndpointLoopbackHTTP && k.APIKey == "" && k.APIKeyFile == "" {
		return errors.New("a loopback HTTP endpoint requires api_key or api_key_file")
	}
	if err := ValidateKataTeammate(k.ResolvedTeammate()); err != nil {
		return err
	}
	if project := k.DefaultProject; project != strings.TrimSpace(project) || strings.IndexFunc(project, unicode.IsControl) >= 0 {
		return errors.New("default_project must not have surrounding whitespace or control characters")
	}
	return nil
}

// ResolveAPIKey returns the credential from api_key or api_key_file, or ""
// when neither is set. A configured source that cannot be read fails; it never
// falls back to the other one. Errors never contain the credential.
func (k KataIntegrationConfig) ResolveAPIKey() (string, error) {
	if k.APIKey != "" && k.APIKeyFile != "" {
		return "", errors.New("api_key and api_key_file are mutually exclusive")
	}
	if k.APIKeyFile == "" {
		key := strings.TrimSpace(k.APIKey)
		if strings.IndexFunc(key, unicode.IsSpace) >= 0 {
			return "", errors.New("api_key must be a single token")
		}
		return key, nil
	}
	path := k.APIKeyFile
	if rest, ok := strings.CutPrefix(path, "~/"); ok {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home for api_key_file: %w", err)
		}
		path = filepath.Join(home, rest)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read api_key_file: %w", err)
	}
	key := strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r")
	switch {
	case key == "":
		return "", errors.New("api_key_file is empty")
	case strings.IndexFunc(key, unicode.IsSpace) >= 0:
		return "", errors.New("api_key_file must contain a single token")
	}
	return key, nil
}

// ValidateKataHooks checks local content policy. Repository hooks cannot enable
// a connection; the daemon also validates them against its effective settings.
func ValidateKataHooks(hooks []HookConfig, enabled bool) error {
	for _, h := range hooks {
		if h.Findings == "" {
			continue
		}
		if h.Type != "kata" {
			return fmt.Errorf("findings requires a Kata hook")
		}
		if h.Findings != "comments" && h.Findings != "summary" {
			return fmt.Errorf("hooks.findings must be comments or summary")
		}
		if !enabled {
			return fmt.Errorf("hooks.findings requires the Kata API connection")
		}
	}
	return nil
}
