// Package agentname owns canonical built-in agent names and aliases without
// depending on either config parsing or agent implementations.
package agentname

import (
	"fmt"
	"strings"
)

const ACPPrefix = "acp."

var canonicalByName = map[string]string{
	"codex":       "codex",
	"claude-code": "claude-code",
	"claude":      "claude-code",
	"gemini":      "gemini",
	"copilot":     "copilot",
	"opencode":    "opencode",
	"cursor":      "cursor",
	"agent":       "cursor",
	"kiro":        "kiro",
	"kilo":        "kilo",
	"droid":       "droid",
	"pi":          "pi",
	"grok":        "grok",
	"grok-build":  "grok",
	"acp":         "acp",
	"test":        "test",
}

// Canonical resolves a built-in alias and otherwise returns the trimmed name.
func Canonical(name string) string {
	name = strings.TrimSpace(name)
	if canonical, ok := canonicalByName[name]; ok {
		return canonical
	}
	return name
}

// BuiltIn reports whether name is a built-in agent or alias and returns its
// canonical name.
func BuiltIn(name string) (string, bool) {
	canonical, ok := canonicalByName[strings.TrimSpace(name)]
	return canonical, ok
}

// ACPConfigName returns the [acp.<name>] table key represented by a canonical
// named ACP agent identity. Bare names are not agent identities.
func ACPConfigName(name string) (string, bool) {
	name = strings.TrimSpace(name)
	configName, ok := strings.CutPrefix(name, ACPPrefix)
	if !ok || configName == "" || strings.Contains(configName, ".") {
		return "", false
	}
	return configName, true
}

// NamedACP returns the canonical agent identity for an [acp.<name>] table key.
func NamedACP(configName string) string {
	return ACPPrefix + strings.TrimSpace(configName)
}

// ValidateReference validates an agent-valued configuration field. Custom ACP
// entries must always be referenced by their canonical acp.<name> identity.
func ValidateReference(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	if _, builtIn := BuiltIn(name); builtIn {
		return nil
	}
	if _, canonicalACP := ACPConfigName(name); canonicalACP {
		return nil
	}
	if strings.HasPrefix(name, ACPPrefix) {
		return fmt.Errorf("invalid ACP agent identity %q; expected acp.<name>", name)
	}
	return fmt.Errorf(
		"unknown agent %q; named ACP agents must use %q",
		name, NamedACP(name),
	)
}
