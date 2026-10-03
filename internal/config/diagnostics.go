package config

import (
	"os"
	"reflect"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
)

// AgentReference is one agent name set in a config file, with the dotted
// TOML key that sets it (for example "review_agent_fast" or "ci.agents[0]").
type AgentReference struct {
	Key  string `json:"key"`
	Name string `json:"name"`
}

// AgentReferences lists every non-empty agent name a *Config or *RepoConfig
// sets, in deterministic key order.
func AgentReferences(cfg any) []AgentReference {
	var refs []AgentReference
	_ = walkAgentReferences(reflect.ValueOf(cfg), "", func(key, name string) error {
		if name = strings.TrimSpace(name); name != "" {
			refs = append(refs, AgentReference{Key: key, Name: name})
		}
		return nil
	})
	return refs
}

// UnknownGlobalKeys returns keys in the global config file at path that do
// not map to any config setting. roborev ignores such keys, so they are
// usually typos. A missing file has no unknown keys.
func UnknownGlobalKeys(path string) ([]string, error) {
	return unknownKeys(path, DefaultConfig())
}

// UnknownRepoKeys is like UnknownGlobalKeys for the repository's
// .roborev.toml, resolved the same way LoadRepoConfig resolves it.
func UnknownRepoKeys(repoPath string) ([]string, error) {
	return unknownKeys(RepoConfigPath(repoPath), &RepoConfig{})
}

func unknownKeys(path string, into any) ([]string, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	md, err := toml.Decode(string(data), into)
	if err != nil {
		return nil, err
	}
	var keys []string
	for _, key := range md.Undecoded() {
		keys = append(keys, key.String())
	}
	slices.Sort(keys)
	return keys, nil
}
