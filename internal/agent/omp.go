package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
)

// ompReadOnlyTools are the built-in omp tools a non-agentic run may use. omp
// has no ls tool (read lists directories), and its find tool is a semantic
// search that calls a second model, so glob takes its place.
const ompReadOnlyTools = "read,grep,glob"

// OmpAgent runs code reviews using the omp (oh-my-pi) CLI. omp is a fork of
// Pi that writes the same --mode json event stream, so it reuses Pi's parser.
// It is a separate agent because its tools, flags, exit codes and session
// store differ from Pi's.
type OmpAgent struct {
	Command   string         // The omp command to run (default: "omp")
	Model     string         // Model to use (bare name or provider/model)
	Reasoning ReasoningLevel // Reasoning level mapped to --thinking
	Agentic   bool           // Agentic mode
}

// NewOmpAgent creates a new omp agent
func NewOmpAgent(command string) *OmpAgent {
	if command == "" {
		command = "omp"
	}
	return &OmpAgent{Command: command, Reasoning: ReasoningStandard}
}

func (a *OmpAgent) clone(opts ...agentCloneOption) *OmpAgent {
	cfg := newAgentCloneConfig(a.Command, a.Model, a.Reasoning, a.Agentic, "", opts...)
	return &OmpAgent{Command: cfg.Command, Model: cfg.Model, Reasoning: cfg.Reasoning, Agentic: cfg.Agentic}
}

func (a *OmpAgent) Name() string        { return "omp" }
func (a *OmpAgent) CommandName() string { return a.Command }

func (a *OmpAgent) WithReasoning(level ReasoningLevel) Agent {
	return a.clone(withClonedReasoning(level))
}

func (a *OmpAgent) WithAgentic(agentic bool) Agent {
	return a.clone(withClonedAgentic(agentic))
}

func (a *OmpAgent) WithModel(model string) Agent {
	if model == "" {
		return a
	}
	return a.clone(withClonedModel(model))
}

// buildArgs returns the flags for one print-mode run. Non-agentic runs name
// their tools with --tools, and --no-tools keeps MCP, extension, custom and
// memory tools out; omp releases before 19.2.0 accept --no-tools but still
// add those tools. --approval-mode always-ask then refuses any tool that needs
// approval, since print mode has nobody to answer the prompt. Agentic runs
// use omp's default tools and approve every call, whatever the user's saved
// approval mode.
func (a *OmpAgent) buildArgs(agentic bool) []string {
	args := []string{"-p", "--mode", "json"}
	if agentic {
		args = append(args, "--approval-mode", "yolo")
	} else {
		args = append(args, "--tools", ompReadOnlyTools, "--no-tools", "--approval-mode", "always-ask", "--no-lsp")
	}
	if a.Model != "" {
		args = append(args, "--model", a.Model)
	}
	if level := reasoningEffort(a.Reasoning); level != "" {
		args = append(args, "--thinking", level)
	}
	return args
}

func (a *OmpAgent) CommandLine() string {
	return a.Command + " " + strings.Join(a.buildArgs(a.Agentic || AllowUnsafeAgents()), " ")
}

// PlanningCommandLine returns omp's representative read-only planning command.
func (a *OmpAgent) PlanningCommandLine() string {
	return a.Command + " " + strings.Join(a.buildArgs(false), " ")
}

func (a *OmpAgent) Review(ctx context.Context, repoPath, commitSHA, prompt string, output io.Writer) (string, error) {
	// omp has no mode that ignores project configuration: tool modules under
	// .omp/tools load even with every --no-* flag, and Bun loads the
	// checkout's .env, which can redirect provider requests.
	if untrustedCheckout(ctx) {
		return "", errors.New("omp cannot review an untrusted checkout: it has no mode that ignores project configuration")
	}
	// A file keeps large prompts off the command line. omp reads a non-TTY
	// stdin as prompt text, so stdin stays empty.
	promptPath, cleanup, err := createPromptFile("omp", prompt)
	if err != nil {
		return "", err
	}
	defer cleanup()
	args := append(a.buildArgs(effectiveAgentic(ctx, a.Agentic)),
		"@"+promptPath, "Please follow the instructions in the attached file to review the code.")

	var stream piStream
	result, err := runStreamingCLI(ctx, streamingCLISpec{
		Name: "omp", Command: a.Command, Args: args,
		Dir: repoPath, Output: output, StreamStderr: true, DrainStdout: true,
		Parse: func(r io.Reader, sw *syncWriter) (string, error) {
			var parseErr error
			stream, parseErr = parsePiJSON(r, sw)
			return stream.Text, parseErr
		},
	})
	if err != nil {
		return "", err
	}
	// omp exits 1 when the provider ends the run with an error, and its JSON
	// mode reports the provider's message only in the event stream.
	if stream.Error != "" {
		return "", fmt.Errorf("omp provider error: %s", stream.Error)
	}
	if result.WaitErr != nil {
		return "", formatStreamingCLIWaitError("omp", result, result.Stderr)
	}
	if result.ParseErr != nil {
		return result.Result, result.ParseErr
	}
	if result.Result == "" {
		return "No review output generated", nil
	}
	return result.Result, nil
}

func init() { Register(NewOmpAgent("")) }
