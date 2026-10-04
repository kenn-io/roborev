package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"time"

	"github.com/BurntSushi/toml"

	"go.kenn.io/roborev/internal/requestsigning"
)

// RemoteClientConfig is only used for explicitly selected HTTPS endpoints.
// It never changes the trust or discovery policy of local daemon connections.
type RemoteClientConfig struct {
	ExternalURL string `toml:"external_url"`
	KeyID       string `toml:"key_id"`
	SecretFile  string `toml:"secret_file"`
	SecretEnv   string `toml:"secret_env"`
	CAFile      string `toml:"ca_file"`
}

// MatchOrigin binds credential loading to an operator-configured HTTPS origin
// and prefix, including when a long-lived client reloads rotated credentials.
func (r RemoteClientConfig) MatchOrigin(raw string) error {
	want, err := requestsigning.ValidateBase(r.ExternalURL)
	if err != nil {
		return errors.New("remote client requires a fixed HTTPS external_url")
	}
	got, err := requestsigning.ValidateBase(raw)
	if err != nil || got.String() != want.String() {
		return errors.New("remote endpoint differs from configured credential origin or prefix")
	}
	return nil
}

// RemoteSigningKey grants history access independently of native bearer auth.
type RemoteSigningKey struct {
	ID         string   `toml:"id"`
	SecretFile string   `toml:"secret_file"`
	SecretEnv  string   `toml:"secret_env"`
	Grants     []string `toml:"grants"`
	RepoIDs    []int64  `toml:"repo_ids"`
	AllRepos   bool     `toml:"all_repos"`
}

// RemoteConfig is opt-in, captures policy at startup, and exposes no mutations.
// Budgets have no speculative defaults: operators select ingress constraints.
type RemoteConfig struct {
	Enabled           bool               `toml:"enabled"`
	Listen            string             `toml:"listen"`
	ExternalURL       string             `toml:"external_url"`
	TrustedProxy      bool               `toml:"trusted_proxy"`
	CertFile          string             `toml:"cert_file"`
	TLSKeyFile        string             `toml:"tls_key_file"`
	ReplayFile        string             `toml:"replay_file"`
	MaxBodyBytes      int64              `toml:"max_body_bytes"`
	MaxHeaderBytes    int                `toml:"max_header_bytes"`
	MaxConcurrent     int                `toml:"max_concurrent"`
	ReplayCapacity    int                `toml:"replay_capacity"`
	ReadHeaderTimeout string             `toml:"read_header_timeout"`
	ReadTimeout       string             `toml:"read_timeout"`
	RequestTimeout    string             `toml:"request_timeout"`
	Keys              []RemoteSigningKey `toml:"keys"`
}

func validateRemoteConfig(cfg *Config) error {
	r := cfg.Remote
	if !r.Enabled {
		return nil
	}
	if runtime.GOOS == "windows" {
		return errors.New("remote listener requires POSIX owner-only signing files")
	}
	if cfg.AuthKey == "" || ValidateAuthKey(cfg.AuthKey) != nil {
		return errors.New("remote listener requires a valid nonempty auth_key")
	}
	if _, err := requestsigning.ValidateBase(r.ExternalURL); err != nil {
		return err
	}
	if r.Listen != "" {
		host, _, err := net.SplitHostPort(r.Listen)
		if err != nil || net.ParseIP(host) == nil {
			return errors.New("remote.listen must contain an explicit IP address and port")
		}
		if r.TrustedProxy && !net.ParseIP(host).IsLoopback() {
			return errors.New("trusted proxy remote listener must bind loopback")
		}
	}
	if r.TrustedProxy {
		if r.CertFile != "" || r.TLSKeyFile != "" {
			return errors.New("trusted proxy mode must not configure native TLS files")
		}
	} else if r.CertFile == "" || r.TLSKeyFile == "" {
		return errors.New("remote listener requires native TLS files or explicit trusted_proxy")
	}
	if r.MaxBodyBytes <= 0 || r.MaxHeaderBytes <= 0 || r.MaxConcurrent <= 0 || r.ReplayCapacity <= 0 || r.ReplayFile == "" {
		return errors.New("remote listener requires explicit positive body, header, concurrency and replay budgets and a replay_file")
	}
	for _, value := range []string{r.ReadHeaderTimeout, r.ReadTimeout, r.RequestTimeout} {
		d, err := time.ParseDuration(value)
		if err != nil || d <= 0 {
			return errors.New("remote listener time budgets must be positive Go durations")
		}
	}
	if len(r.Keys) == 0 {
		return errors.New("remote listener requires signing keys")
	}
	ids := map[string]bool{}
	for _, k := range r.Keys {
		if ids[k.ID] || (requestsigning.Key{ID: k.ID, Secret: make([]byte, 64)}).Validate() != nil {
			return errors.New("remote signing key IDs must be valid and unique")
		}
		ids[k.ID] = true
		if (k.SecretFile == "") == (k.SecretEnv == "") {
			return errors.New("remote signing key requires exactly one secret_file or secret_env")
		}
		if len(k.Grants) == 0 {
			return errors.New("remote signing key requires explicit grants")
		}
		for _, grant := range k.Grants {
			if grant != "history:read" && grant != "history:events" {
				return errors.New("remote signing grants support only history:read and history:events")
			}
		}
		if k.AllRepos == (len(k.RepoIDs) > 0) {
			return errors.New("remote signing key requires repo_ids or explicit all_repos")
		}
		for _, id := range k.RepoIDs {
			if id <= 0 {
				return errors.New("remote signing repository IDs must be positive")
			}
		}
	}
	return nil
}

// LoadRemoteClient reads only the native HTTPS client configuration, without
// requiring unrelated daemon configuration to be semantically valid.
func LoadRemoteClient() (RemoteClientConfig, error) {
	var values struct {
		RemoteClient RemoteClientConfig `toml:"remote_client"`
	}
	path := GlobalConfigPath()
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return values.RemoteClient, nil
	} else if err != nil {
		return values.RemoteClient, err
	}
	if _, err := toml.DecodeFile(path, &values); err != nil {
		return RemoteClientConfig{}, safeGlobalConfigError(path, err)
	}
	return values.RemoteClient, nil
}

// ValidateRemote checks listener configuration before any daemon startup work.
func ValidateRemote(cfg *Config) error { return validateRemoteConfig(cfg) }

func (r RemoteClientConfig) SigningKey() (requestsigning.Key, error) {
	key, err := requestsigning.ReadKey(r.KeyID, r.SecretFile, r.SecretEnv)
	if err != nil {
		return requestsigning.Key{}, fmt.Errorf("remote client signing: %w", err)
	}
	return key, nil
}
