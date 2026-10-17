package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// MuseAgent runs Meta's Muse Code CLI in headless mode.
type MuseAgent struct {
	Command   string
	Model     string
	Reasoning ReasoningLevel
	Agentic   bool
}

func NewMuseAgent(command string) *MuseAgent {
	if command == "" {
		command = "muse"
	}
	return &MuseAgent{Command: command, Reasoning: ReasoningStandard}
}

func (a *MuseAgent) clone(opts ...agentCloneOption) *MuseAgent {
	cfg := newAgentCloneConfig(a.Command, a.Model, a.Reasoning, a.Agentic, "", opts...)
	return &MuseAgent{Command: cfg.Command, Model: cfg.Model, Reasoning: cfg.Reasoning, Agentic: cfg.Agentic}
}

func (a *MuseAgent) WithReasoning(level ReasoningLevel) Agent {
	return a.clone(withClonedReasoning(level))
}

func (a *MuseAgent) WithAgentic(agentic bool) Agent {
	return a.clone(withClonedAgentic(agentic))
}

func (a *MuseAgent) WithModel(model string) Agent {
	if model == "" {
		return a
	}
	return a.clone(withClonedModel(model))
}

func (a *MuseAgent) Name() string        { return "muse" }
func (a *MuseAgent) CommandName() string { return a.Command }

func (a *MuseAgent) buildArgs(agentic bool, promptFile string) []string {
	args := []string{"exec", "--no-session-log", "--contributor-mode", "off"}
	if a.Model != "" {
		args = append(args, "--model", a.Model)
	}
	if effort := reasoningEffort(a.Reasoning); effort != "" {
		args = append(args, "--reasoning-effort", effort)
	}
	if agentic {
		args = append(args, "--yolo")
	} else {
		args = append(args, "-c", `run.toolset=["read_file","search"]`, "-c", `hooks={}`, "-c", "managed_hooks_path=null")
	}
	return append(args, "--prompt-file", promptFile)
}

func (a *MuseAgent) CommandLine() string {
	return a.Command + " " + strings.Join(a.buildArgs(a.Agentic || AllowUnsafeAgents(), "<prompt-file>"), " ")
}

func (a *MuseAgent) PlanningCommandLine() string {
	return a.Command + " " + strings.Join(a.buildArgs(false, "<prompt-file>"), " ")
}

func (a *MuseAgent) Review(ctx context.Context, repoPath, commitSHA, prompt string, output io.Writer) (string, error) {
	// Muse requires a real file for prompts that exceed argv limits.
	promptPath, env, cleanup, err := prepareMuseRun(prompt, effectiveAgentic(ctx, a.Agentic))
	if err != nil {
		return "", err
	}
	defer cleanup()
	result, err := runStreamingCLI(ctx, streamingCLISpec{
		Name: "muse", Command: a.Command,
		Args: a.buildArgs(effectiveAgentic(ctx, a.Agentic), promptPath),
		Dir:  repoPath, Env: env, Output: output, StreamStderr: true, DrainStdout: true,
		Parse: parsePlainTextOutput,
	})
	if err != nil {
		return "", err
	}
	if result.WaitErr != nil {
		return "", formatStreamingCLIWaitError("muse", result, result.Stderr)
	}
	if result.ParseErr != nil {
		return result.Result, result.ParseErr
	}
	if result.Result == "" {
		return "No review output generated", nil
	}
	return result.Result, nil
}

// prepareMuseRun writes the prompt file. Non-agentic runs also get a private
// XDG_CONFIG_HOME holding only the Muse login and settings, plus an empty
// XDG_DATA_HOME, to exclude saved workspace trust and installed plugins.
func prepareMuseRun(prompt string, agentic bool) (string, []string, func(), error) {
	promptPath, cleanupPrompt, err := createPromptFile("muse", prompt)
	if err != nil || agentic {
		return promptPath, nil, cleanupPrompt, err
	}
	src := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "muse")
	if os.Getenv("XDG_CONFIG_HOME") == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			cleanupPrompt()
			return "", nil, nil, fmt.Errorf("resolve muse config home: %w", err)
		}
		src = filepath.Join(home, ".config", "muse")
	}
	dir, err := os.MkdirTemp("", "roborev-muse-config-*")
	if err != nil {
		cleanupPrompt()
		return "", nil, nil, fmt.Errorf("create muse config dir: %w", err)
	}
	cleanup := func() { cleanupPrompt(); _ = os.RemoveAll(dir) }
	dataDir := filepath.Join(dir, "data")
	if err := os.Mkdir(dataDir, 0o700); err != nil {
		cleanup()
		return "", nil, nil, fmt.Errorf("create muse data dir: %w", err)
	}
	err = os.Mkdir(filepath.Join(dir, "muse"), 0o700)
	for _, name := range []string{"auth.json", "settings.json"} {
		data, readErr := os.ReadFile(filepath.Join(src, name))
		if err == nil && readErr == nil {
			err = os.WriteFile(filepath.Join(dir, "muse", name), data, 0o600)
		} else if err == nil && !errors.Is(readErr, fs.ErrNotExist) {
			err = readErr
		}
	}
	if err != nil {
		cleanup()
		return "", nil, nil, fmt.Errorf("copy muse config: %w", err)
	}
	return promptPath, append(os.Environ(), "XDG_CONFIG_HOME="+dir, "XDG_DATA_HOME="+dataDir), cleanup, nil
}

func init() { Register(NewMuseAgent("")) }
