package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// AgentsViewConfig selects a local or remote AgentsView usage server.
// It is global-only because repositories must not choose where tokens are sent.
type AgentsViewConfig struct {
	URL       string `toml:"url" comment:"AgentsView HTTP(S) base URL, optionally including a base path."`
	TokenFile string `toml:"token_file" sensitive:"true" comment:"Bearer token file; takes precedence over AGENTSVIEW_SERVER_TOKEN. Supports ~/ paths."`
}

func (c AgentsViewConfig) Validate(cost CostConfig) error {
	if c.URL != "" {
		u, err := url.Parse(c.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.ForceQuery || u.RawQuery != "" || strings.Contains(c.URL, "#") {
			return fmt.Errorf("agentsview.url must be an absolute HTTP(S) URL with a host and no credentials, query, or fragment")
		}
		if cost.Endpoint != "" {
			return fmt.Errorf("agentsview.url and cost.endpoint are mutually exclusive")
		}
	}
	if c.TokenFile != "" {
		_, err := c.ResolveToken()
		return err
	}
	return nil
}

// ResolveToken reads a configured file before consulting the server token env var.
// Resolved secrets are never stored in Config or included in file errors.
func (c AgentsViewConfig) ResolveToken() (string, error) {
	if c.TokenFile == "" {
		return os.Getenv("AGENTSVIEW_SERVER_TOKEN"), nil
	}
	path := c.TokenFile
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("agentsview.token_file: cannot resolve home directory")
		}
		path = filepath.Join(home, path[2:])
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("agentsview.token_file: cannot read token file")
	}
	return strings.TrimSpace(string(data)), nil
}
