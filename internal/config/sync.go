// PostgreSQL sync configuration.

package config

import (
	"os"
	"regexp"
	"strings"
)

// SyncConfig holds configuration for PostgreSQL sync
type SyncConfig struct {
	// Enabled enables sync to PostgreSQL
	Enabled bool `toml:"enabled"`

	// PostgresURL is the connection string for PostgreSQL.
	// Supports environment variable expansion via ${VAR} syntax.
	PostgresURL string `toml:"postgres_url" sensitive:"true"`

	// Interval is how often to sync (e.g., "5m", "1h"). Default: 1h
	Interval string `toml:"interval"`

	// MachineName is a friendly name for this machine (optional)
	MachineName string `toml:"machine_name"`

	// ConnectTimeout is the connection timeout (e.g., "5s"). Default: 5s
	ConnectTimeout string `toml:"connect_timeout"`

	// RepoNames provides custom display names for synced repos by identity.
	// Example: {"git@github.com:org/repo.git": "my-project"}
	RepoNames map[string]string `toml:"repo_names"`
}

// PostgresURLExpanded returns the PostgreSQL URL with environment variables expanded.
// Returns empty string if URL is not set.
func (c *SyncConfig) PostgresURLExpanded() string {
	if c.PostgresURL == "" {
		return ""
	}
	return os.ExpandEnv(c.PostgresURL)
}

// GetRepoDisplayName returns the configured display name for a repo identity,
// or empty string if no override is configured.
func (c *SyncConfig) GetRepoDisplayName(identity string) string {
	if c == nil || c.RepoNames == nil {
		return ""
	}
	return c.RepoNames[identity]
}

// Validate checks the sync configuration for common issues.
// Returns a list of warnings (non-fatal issues).
func (c *SyncConfig) Validate() []string {
	var warnings []string

	if !c.Enabled {
		return warnings
	}

	if c.PostgresURL == "" {
		warnings = append(warnings, "sync.enabled is true but sync.postgres_url is not set")
		return warnings
	}

	// Check for environment variable references where the var is not set
	// os.ExpandEnv replaces ${VAR} with empty string if VAR is not set
	if strings.Contains(c.PostgresURL, "${") {
		re := regexp.MustCompile(`\$\{([^}]+)\}`)
		matches := re.FindAllStringSubmatch(c.PostgresURL, -1)
		for _, match := range matches {
			if len(match) > 1 {
				varName := match[1]
				if os.Getenv(varName) == "" {
					warnings = append(warnings, "sync.postgres_url may contain unexpanded environment variables")
					break // Only one warning needed
				}
			}
		}
	}

	return warnings
}
