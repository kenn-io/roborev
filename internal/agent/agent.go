package agent

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// ReasoningLevel controls how much reasoning/thinking an agent uses
type ReasoningLevel string

const (
	// ReasoningMaximum uses the highest available reasoning (e.g., codex xhigh, claude max)
	ReasoningMaximum ReasoningLevel = "maximum"
	// ReasoningThorough uses deep reasoning for thorough analysis (slower)
	ReasoningThorough ReasoningLevel = "thorough"
	// ReasoningMedium uses moderate reasoning (e.g., claude --effort medium)
	ReasoningMedium ReasoningLevel = "medium"
	// ReasoningStandard uses the model's default reasoning (no effort override)
	ReasoningStandard ReasoningLevel = "standard"
	// ReasoningFast uses minimal reasoning for quick responses
	ReasoningFast ReasoningLevel = "fast"
)

// ReasoningLevels returns the canonical reasoning level names.
func ReasoningLevels() []string {
	return []string{string(ReasoningFast), string(ReasoningStandard), string(ReasoningMedium), string(ReasoningThorough), string(ReasoningMaximum)}
}

// ParseReasoningLevel converts a string to ReasoningLevel, defaulting to standard
func ParseReasoningLevel(s string) ReasoningLevel {
	switch s {
	case "maximum", "max", "xhigh":
		return ReasoningMaximum
	case "thorough", "high":
		return ReasoningThorough
	case "medium":
		return ReasoningMedium
	case "fast", "low":
		return ReasoningFast
	case "standard", "":
		return ReasoningStandard
	default:
		return ReasoningStandard
	}
}

// Agent defines the interface for code review agents
type Agent interface {
	// Name returns the agent identifier (e.g., "codex", "claude-code")
	Name() string

	// Review runs a code review and returns the output.
	// If output is non-nil, agent progress is streamed to it in real-time.
	Review(ctx context.Context, repoPath, commitSHA, prompt string, output io.Writer) (result string, err error)

	// WithReasoning returns a copy of the agent configured with the specified reasoning level.
	// Agents that don't support reasoning levels may return themselves unchanged.
	WithReasoning(level ReasoningLevel) Agent

	// WithAgentic returns a copy of the agent configured for agentic mode.
	// In agentic mode, agents can edit files and run commands.
	// If false, agents operate in read-only review mode.
	WithAgentic(agentic bool) Agent

	// WithModel returns a copy of the agent configured to use the specified model.
	// If model is empty, the agent is returned unchanged (preserving any built-in default).
	// Agents that don't support model selection may return themselves unchanged.
	// For opencode, the model format is "provider/model" (e.g., "anthropic/claude-sonnet-4-20250514").
	WithModel(model string) Agent

	// CommandLine returns a representative command line for this agent (binary + flags).
	// Runtime-specific arguments (repo path, output file, prompt) are excluded.
	// Useful for debugging which binary, model, and flags were used.
	CommandLine() string
}

// CommandAgent is an agent that uses an external command
type CommandAgent interface {
	Agent
	// CommandName returns the executable command name
	CommandName() string
}

// CommandCandidatesAgent is implemented by command agents that can run
// through more than one compatible executable, ordered by preference.
type CommandCandidatesAgent interface {
	CommandAgent
	CommandNames() []string
}

// SessionAgent is implemented by agents that can resume an existing session.
type SessionAgent interface {
	WithSessionID(sessionID string) Agent
}

// Registry holds available agents
var (
	registryMu sync.RWMutex
	registry   = make(map[string]Agent)
)

var allowUnsafeAgents atomic.Bool
var codexSandboxDisabled atomic.Bool
var anthropicAPIKey atomic.Value

func AllowUnsafeAgents() bool {
	return allowUnsafeAgents.Load()
}

func SetAllowUnsafeAgents(allow bool) {
	allowUnsafeAgents.Store(allow)
}

// CodexSandboxDisabled returns true when the Codex bwrap sandbox
// should be skipped (e.g. on machines without unprivileged user
// namespace support). When true, Codex runs with --full-auto
// instead of --sandbox read-only.
func CodexSandboxDisabled() bool {
	return codexSandboxDisabled.Load()
}

func SetCodexSandboxDisabled(v bool) {
	codexSandboxDisabled.Store(v)
}

// AnthropicAPIKey returns the configured Anthropic API key, or empty string if not set
func AnthropicAPIKey() string {
	if v := anthropicAPIKey.Load(); v != nil {
		return v.(string)
	}
	return ""
}

// SetAnthropicAPIKey sets the Anthropic API key for Claude Code
func SetAnthropicAPIKey(key string) {
	anthropicAPIKey.Store(key)
}

// UnknownAgentError is returned when a requested agent name is not
// in the registry and is not a recognized alias.
type UnknownAgentError struct {
	Name  string
	Known []string
}

func (e *UnknownAgentError) Error() string {
	return fmt.Sprintf(
		"unknown agent %q (known: %s)",
		e.Name, strings.Join(e.Known, ", "),
	)
}

// CanonicalName resolves an agent alias to its canonical name.
// Returns the name unchanged if it is not an alias.
func CanonicalName(name string) string {
	return resolveAlias(name)
}

// Register adds an agent to the registry
func Register(a Agent) {
	registryMu.Lock()
	registry[a.Name()] = a
	registryMu.Unlock()
}

// Get returns an agent by name (supports aliases like "claude" for "claude-code")
func Get(name string) (Agent, error) {
	name = resolveAlias(name)
	registryMu.RLock()
	a, ok := registry[name]
	registryMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown agent: %s", name)
	}
	return a, nil
}

// Available returns the names of all registered agents
func Available() []string {
	registryMu.RLock()
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	registryMu.RUnlock()
	return names
}

// IsAvailable checks if an agent's command is installed on the system
// Supports aliases like "claude" for "claude-code"
func IsAvailable(name string) bool {
	name = resolveAlias(name)
	registryMu.RLock()
	a, ok := registry[name]
	registryMu.RUnlock()
	if !ok {
		return false
	}

	// Check if agent implements CommandAgent interface
	if ca, ok := a.(CommandAgent); ok {
		return firstAvailableCommand(ca) != ""
	}

	// Non-command agents (like test) are always available
	return true
}

// GetAvailable returns an available agent, trying the requested one first,
// then falling back to alternatives. Returns error only if no agents available.
// Supports aliases like "claude" for "claude-code".
//
// Optional backup agent names are tried after the preferred agent but
// before the hardcoded fallback chain. This lets callers honor
// default_backup_agent config without changing the global chain order.
func GetAvailable(preferred string, backups ...string) (Agent, error) {
	// Resolve alias upfront for consistent comparisons
	preferred = resolveAlias(preferred)

	// Reject unknown agent names (typos, config mistakes).
	// Known-but-unavailable agents still fall back below.
	if preferred != "" {
		registryMu.RLock()
		_, ok := registry[preferred]
		registryMu.RUnlock()
		if !ok {
			known := Available()
			sort.Strings(known)
			return nil, &UnknownAgentError{
				Name:  preferred,
				Known: known,
			}
		}
	}

	// Try preferred agent first
	if preferred != "" && IsAvailable(preferred) {
		a, _ := Get(preferred)
		return applyResolvedCommand(a), nil
	}

	// Try backup agents before the hardcoded fallback chain.
	for _, b := range backups {
		b = resolveAlias(b)
		if b == "" || b == preferred {
			continue
		}
		registryMu.RLock()
		_, ok := registry[b]
		registryMu.RUnlock()
		if ok && IsAvailable(b) {
			a, _ := Get(b)
			return applyResolvedCommand(a), nil
		}
	}

	for _, name := range fallbackAgentOrder {
		if name != preferred && IsAvailable(name) {
			a, _ := Get(name)
			return applyResolvedCommand(a), nil
		}
	}

	// List what's actually available for error message (exclude test agent)
	var available []string
	registryMu.RLock()
	for name := range registry {
		if name != "test" && IsAvailable(name) {
			available = append(available, name)
		}
	}
	registryMu.RUnlock()

	if len(available) == 0 {
		return nil, fmt.Errorf("no agents available (install one of: %s)\nYou may need to run 'roborev daemon restart' from a shell that has access to your agents", strings.Join(installHintAgentNames(), ", "))
	}

	a, _ := Get(available[0])
	return applyResolvedCommand(a), nil
}

func firstAvailableCommand(a CommandAgent) string {
	if candidates, ok := a.(CommandCandidatesAgent); ok {
		for _, command := range candidates.CommandNames() {
			if command == "" {
				continue
			}
			if _, err := exec.LookPath(command); err == nil {
				return command
			}
		}
		return ""
	}

	command := a.CommandName()
	if command == "" {
		return ""
	}
	if _, err := exec.LookPath(command); err == nil {
		return command
	}
	return ""
}

func applyResolvedCommand(a Agent) Agent {
	ca, ok := a.(CommandAgent)
	if !ok {
		return a
	}
	command := firstAvailableCommand(ca)
	if command == "" || command == ca.CommandName() {
		return a
	}
	spec, ok := agentSpecsByName[resolveAlias(a.Name())]
	if !ok || spec.CloneWithCommand == nil {
		return a
	}
	return spec.CloneWithCommand(a, command)
}

// syncWriter wraps an io.Writer with mutex protection for concurrent writes.
// This is needed because io.MultiWriter sends both stdout and stderr to the
// same output concurrently, which could race if the underlying writer isn't
// thread-safe.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

// newSyncWriter creates a thread-safe wrapper around an io.Writer.
// Returns nil if w is nil.
func newSyncWriter(w io.Writer) *syncWriter {
	if w == nil {
		return nil
	}
	return &syncWriter{w: w}
}

func (sw *syncWriter) Write(p []byte) (n int, err error) {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	return sw.w.Write(p)
}

// Unregister removes an agent from the registry (useful for testing)
func Unregister(name string) {
	registryMu.Lock()
	delete(registry, name)
	registryMu.Unlock()
}
