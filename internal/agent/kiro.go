package agent

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// maxPromptArgLen is a conservative limit for passing prompts as
// CLI arguments. macOS ARG_MAX is ~1 MB; we leave headroom for
// the command name, flags, and environment.
const maxPromptArgLen = 512 * 1024

// stripKiroOutput removes Kiro's UI chrome (logo, tip box, model line, timing footer)
// and terminal control sequences, returning only the review text.
func stripKiroOutput(raw string) string {
	text, _ := stripKiroReview(raw)
	return text
}

// stripKiroReview strips Kiro chrome and returns the cleaned text
// plus a bool indicating whether a "> " review marker was found.
// When no marker is found the full ANSI-stripped text is returned
// (hasMarker == false), which may be non-review noise.
func stripKiroReview(raw string) (string, bool) {
	s := stripTerminalControls(raw)

	// Kiro prepends a splash screen and tip box before the response.
	// The "> " prompt marker appears near the top; limit the search
	// to avoid mistaking markdown blockquotes for the start marker.
	lines := strings.Split(s, "\n")
	limit := min(30, len(lines))
	start := -1
	for i, line := range lines[:limit] {
		if strings.HasPrefix(line, "> ") || line == ">" {
			start = i
			break
		}
	}
	if start == -1 {
		return strings.TrimSpace(s), false
	}

	// Strip the prompt marker from the first content line.
	// A bare ">" (no trailing content) is skipped entirely.
	if lines[start] == ">" {
		start++
		if start >= len(lines) {
			return "", true
		}
	} else {
		lines[start] = strings.TrimPrefix(lines[start], "> ")
	}

	// Drop the timing footer ("▸ Time: Xs") and anything after it.
	// Trim trailing blank lines first so they don't push the real
	// footer outside the scan window, then scan the last 5 non-blank
	// lines to avoid truncating review content that happens to
	// contain "▸ Time:" in a code snippet.
	end := len(lines)
	for end > start && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	scanFrom := max(start, end-5)
	for i := scanFrom; i < end; i++ {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "▸ Time:") {
			end = i
			break
		}
	}

	return strings.TrimSpace(strings.Join(lines[start:end], "\n")), true
}

// KiroAgent runs code reviews using the Kiro CLI (kiro-cli)
type KiroAgent struct {
	Command   string         // The kiro-cli command to run (default: "kiro-cli")
	Reasoning ReasoningLevel // Reasoning level (stored; kiro-cli has no reasoning flag)
	Agentic   bool           // Whether agentic mode is enabled (uses --trust-all-tools)
}

// NewKiroAgent creates a new Kiro agent with standard reasoning
func NewKiroAgent(command string) *KiroAgent {
	if command == "" {
		command = "kiro-cli"
	}
	return &KiroAgent{Command: command, Reasoning: ReasoningStandard}
}

func (a *KiroAgent) clone(opts ...agentCloneOption) *KiroAgent {
	cfg := newAgentCloneConfig(
		a.Command,
		"",
		a.Reasoning,
		a.Agentic,
		"",
		opts...,
	)
	return &KiroAgent{
		Command:   cfg.Command,
		Reasoning: cfg.Reasoning,
		Agentic:   cfg.Agentic,
	}
}

// WithReasoning returns a copy with the reasoning level stored.
// kiro-cli has no reasoning flag; callers can map reasoning to agent selection instead.
func (a *KiroAgent) WithReasoning(level ReasoningLevel) Agent {
	return a.clone(withClonedReasoning(level))
}

// WithAgentic returns a copy of the agent configured for agentic mode.
// In agentic mode, --trust-all-tools is passed so kiro can use tools without confirmation.
func (a *KiroAgent) WithAgentic(agentic bool) Agent {
	return a.clone(withClonedAgentic(agentic))
}

// WithModel returns the agent unchanged; kiro-cli does not expose a --model CLI flag.
func (a *KiroAgent) WithModel(model string) Agent {
	return a
}

func (a *KiroAgent) Name() string {
	return "kiro"
}

func (a *KiroAgent) CommandName() string {
	return a.Command
}

func (a *KiroAgent) buildArgs(agenticMode bool) []string {
	args := []string{"chat", "--no-interactive"}
	if agenticMode {
		args = append(args, "--trust-all-tools")
	}
	return args
}

func (a *KiroAgent) CommandLine() string {
	agenticMode := a.Agentic || AllowUnsafeAgents()
	args := a.buildArgs(agenticMode)
	return a.Command + " " + strings.Join(args, " ") + " -- <prompt>"
}

func (a *KiroAgent) Review(ctx context.Context, repoPath, commitSHA, prompt string, output io.Writer) (string, error) {
	if len(prompt) > maxPromptArgLen {
		return "", fmt.Errorf(
			"prompt too large for kiro-cli argv (%d bytes, max %d)",
			len(prompt), maxPromptArgLen,
		)
	}

	agenticMode := a.Agentic || AllowUnsafeAgents()

	// kiro-cli chat --no-interactive [--trust-all-tools] <prompt>
	// The prompt is passed as a positional argument
	// (kiro-cli does not support stdin).
	args := a.buildArgs(agenticMode)
	args = append(args, "--", prompt)

	cmd := exec.CommandContext(ctx, a.Command, args...)
	cmd.Dir = repoPath
	cmd.Env = os.Environ()
	// kiro-cli authenticates with a GitHub token, so it is exempt from
	// the GitHub half of the forge credential strip (see forge_env.go).
	tracker := configureSubprocess(cmd, withGitHubCredentials())

	// kiro-cli emits ANSI terminal escape codes that are not
	// suitable for streaming. Capture and return stripped text.
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if ctxErr := contextProcessError(ctx, tracker, err, nil); ctxErr != nil {
			return "", ctxErr
		}
		return "", fmt.Errorf(
			"kiro failed: %w\nstderr: %s",
			err, stderr.String(),
		)
	}

	// Prefer the stream that contains a "> " review marker.
	// - stdout with marker and content → use stdout
	// - stdout empty or marker-only → try stderr
	// - stdout has content but no marker → use stderr only
	//   if stderr has a marker (otherwise keep stdout)
	result, stdoutMarker := stripKiroReview(stdout.String())
	if !stdoutMarker || len(result) == 0 {
		alt, stderrMarker := stripKiroReview(stderr.String())
		if len(alt) > 0 && (len(result) == 0 || stderrMarker) {
			result = alt
		}
	}
	if len(result) == 0 {
		return "No review output generated", nil
	}
	if sw := newSyncWriter(output); sw != nil {
		_, _ = sw.Write([]byte(result + "\n"))
	}
	return result, nil
}

func init() {
	Register(NewKiroAgent(""))
}
