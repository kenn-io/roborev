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
		fullKey := tomlOverrideKeySegment(key)
		if prefix != "" {
			fullKey = prefix + "." + fullKey
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

// tomlOverrideKeySegment returns key unchanged when it is a valid TOML bare key,
// otherwise as a TOML-quoted key segment. Without this, a provider name with
// dots or other non-bare characters (e.g. model_providers."foo.bar") would be
// joined raw and parsed by Codex as extra nested path segments, targeting the
// wrong key or producing an invalid override.
func tomlOverrideKeySegment(key string) string {
	if isBareTOMLKey(key) {
		return key
	}
	if quoted, ok := encodeTOMLOverrideValue(key); ok {
		return quoted
	}
	return key
}

func isBareTOMLKey(key string) bool {
	if key == "" {
		return false
	}
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
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
