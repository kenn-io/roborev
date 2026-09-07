package config

import (
	"net/url"
	"strings"

	"go.kenn.io/roborev/internal/git"
)

// ProjectConfig supplies machine-local defaults shared by checkouts of a remote.
type ProjectConfig struct {
	ReviewModel         string `toml:"review_model"`
	DisplayName         string `toml:"display_name"`
	OverridePanelModels bool   `toml:"override_panel_models"`
	SynthesisModel      string `toml:"synthesis_model"`
}

// PanelModelOverride returns the explicit project policy for primary panel
// members, regardless of their review type or individual model setting.
func (c *Config) PanelModelOverride() string {
	if c == nil || !c.project.OverridePanelModels {
		return ""
	}
	return strings.TrimSpace(c.project.ReviewModel)
}

// ForRepo returns global configuration scoped to the repository's remote.
// It does not read tracked configuration or mutate the shared global config.
// Callers resolving from already-loaded configs should scope their global
// config here before passing it to the FromConfig helpers.
func (c *Config) ForRepo(repoPath string) *Config {
	if c == nil || len(c.Projects) == 0 || repoPath == "" {
		return c
	}
	scoped := *c
	scoped.project = c.Projects[projectRemoteIdentity(git.GetRemoteURL(repoPath, ""))]
	return &scoped
}

// projectRemoteIdentity treats SSH and HTTPS transports as the same project.
// Keep the repository path case-sensitive; only DNS hostnames ignore case.
func projectRemoteIdentity(remote string) string {
	remote = strings.TrimSpace(remote)
	var host, repo string
	if strings.Contains(remote, "://") {
		u, err := url.Parse(remote)
		if err != nil || u.Host == "" {
			return ""
		}
		host, repo = u.Host, u.Path
	} else {
		// SCP-style remotes: [user@]host:owner/repository.git.
		if _, after, ok := strings.Cut(remote, "@"); ok {
			remote = after
		}
		var ok bool
		host, repo, ok = strings.Cut(remote, ":")
		if !ok || strings.ContainsAny(host, `/\`) {
			return ""
		}
	}
	repo = strings.TrimSuffix(strings.Trim(repo, "/"), ".git")
	if host == "" || repo == "" {
		return ""
	}
	return strings.ToLower(host) + "/" + repo
}
