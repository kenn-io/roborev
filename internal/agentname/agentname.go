// Package agentname owns canonical built-in agent names and aliases without
// depending on either config parsing or agent implementations.
package agentname

import "strings"

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
