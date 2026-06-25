package config

import (
	"bytes"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// ConfigOverrideArgs flattens the passthrough Codex config table (the
// `[agent.codex.config]` block) into Codex `-c key=value` override strings,
// one per leaf key, sorted for deterministic output. Nested tables become
// dotted keys (e.g. model_providers.foo.base_url) and values are TOML-encoded
// so Codex parses them as the intended type.
//
// Codex applies `-c` overrides as its highest-precedence config layer,
// independently of --ignore-user-config, so callers can inject a
// model_provider / [model_providers.*] block without loading the user's
// ~/.codex/config.toml.
func (c CodexConfig) ConfigOverrideArgs() []string {
	if len(c.Config) == 0 {
		return nil
	}
	var out []string
	flattenCodexConfigOverrides("", c.Config, &out)
	sort.Strings(out)
	return out
}

func flattenCodexConfigOverrides(prefix string, table map[string]any, out *[]string) {
	for key, val := range table {
		fullKey := key
		if prefix != "" {
			fullKey = prefix + "." + key
		}
		if sub, ok := val.(map[string]any); ok {
			flattenCodexConfigOverrides(fullKey, sub, out)
			continue
		}
		if encoded, ok := encodeTOMLOverrideValue(val); ok {
			*out = append(*out, fullKey+"="+encoded)
		}
	}
}

// encodeTOMLOverrideValue renders a single leaf value as the TOML literal Codex
// expects on the right-hand side of `-c key=value`. It reuses the TOML encoder
// for correct quoting/escaping by encoding a synthetic `v = <value>` line and
// stripping the key. Table-valued or multi-line encodings are rejected, since
// flattenCodexConfigOverrides only passes scalars and flat arrays.
func encodeTOMLOverrideValue(val any) (string, bool) {
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(map[string]any{"v": val}); err != nil {
		return "", false
	}
	encoded := strings.TrimSpace(buf.String())
	idx := strings.IndexByte(encoded, '=')
	if idx < 0 {
		return "", false
	}
	encoded = strings.TrimSpace(encoded[idx+1:])
	if encoded == "" || strings.ContainsAny(encoded, "\r\n") {
		return "", false
	}
	return encoded, true
}
