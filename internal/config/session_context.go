package config

import "fmt"

// SessionContextConfig opts local commit reviews into archived intent context.
type SessionContextConfig struct {
	Mode string `toml:"mode" comment:"Agent-session provenance in local commit and range reviews: off (default) or on. Includes storing and syncing writing prompts."`
}

func (c SessionContextConfig) Validate() error {
	switch c.Mode {
	case "", "off", "on":
		return nil
	}
	return fmt.Errorf("session_context.mode must be off or on")
}

// ResolveSessionContextFrom applies the repository's explicit mode over global.
func ResolveSessionContextFrom(repo *RepoConfig, global *Config) SessionContextConfig {
	mode := "off"
	if global != nil && global.SessionContext.Mode != "" {
		mode = global.SessionContext.Mode
	}
	if repo != nil && repo.SessionContext.Mode != "" {
		mode = repo.SessionContext.Mode
	}
	return SessionContextConfig{Mode: mode}
}
