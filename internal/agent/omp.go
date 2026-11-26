package agent

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"go.kenn.io/roborev/internal/procutil"
)

// ompReadOnlyTools are the built-in omp tools a non-agentic run may use. omp
// has no ls tool (read lists directories), and its find tool is a semantic
// search that calls a second model, so glob takes its place.
const ompReadOnlyTools = "read,grep,glob"

// ompReadOnlyMinVersion is the first omp release whose --no-tools limits a run
// to the tools --tools names. Earlier releases accept --no-tools but still
// add MCP, extension and custom tools, and a user's tools.approval allow rule
// lets them run under --approval-mode always-ask (omp 19.2.0 changelog,
// Breaking Changes).
const ompReadOnlyMinVersion = "19.2.0"

// ompCostCapExit is the exit status of an omp print-mode run that its cost
// cap stopped (omp 19.2.1 modes/print-mode.ts).
const ompCostCapExit = 3

// ompCostCapStopped starts the error of a run that omp's cost cap stopped;
// defaultLimitRules matches it.
const ompCostCapStopped = "omp stopped at its cost cap"

// OmpAgent runs code reviews using the omp (oh-my-pi) CLI. omp is a fork of
// Pi that writes the same --mode json event stream. It is a separate agent
// because its tools, flags, exit codes and session store differ from Pi's.
type OmpAgent struct {
	Command   string
	Model     string // bare name (fuzzy match) or provider/model (exact)
	Reasoning ReasoningLevel
	Agentic   bool
}

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
// memory tools out and stops extensions and LSP from loading.
// --approval-mode always-ask also refuses any tool call that needs approval,
// such as a read of an ssh:// path, since print mode has nobody to answer the
// prompt. Agentic runs use omp's default tools in yolo mode, whatever approval
// mode the user's settings choose; per-tool deny rules and restricted folders
// in those settings still apply.
func (a *OmpAgent) buildArgs(agentic bool) []string {
	args := []string{"-p", "--mode", "json"}
	if agentic {
		args = append(args, "--approval-mode", "yolo")
	} else {
		args = append(args, "--tools", ompReadOnlyTools, "--no-tools", "--approval-mode", "always-ask")
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

func (a *OmpAgent) PlanningCommandLine() string {
	return a.Command + " " + strings.Join(a.buildArgs(false), " ")
}

func (a *OmpAgent) Review(ctx context.Context, repoPath, commitSHA, prompt string, output io.Writer) (string, error) {
	// omp has no mode that ignores project configuration: it reads the
	// checkout's SYSTEM.md, which replaces its system prompt, and merges its
	// .omp/config.yml.
	if untrustedCheckout(ctx) {
		return "", errors.New("omp cannot review an untrusted checkout: it has no mode that ignores project configuration")
	}
	// omp loads the .env in the directory it starts in (omp 19.2.1
	// packages/utils/src/env.ts), before --cwd moves it into the repository.
	// That file can set provider base URLs and keys, so omp starts in an
	// empty directory.
	launchDir, err := os.MkdirTemp("", "roborev-omp-")
	if err != nil {
		return "", fmt.Errorf("create omp launch directory: %w", err)
	}
	defer os.RemoveAll(launchDir)
	agentic := effectiveAgentic(ctx, a.Agentic)
	if !agentic {
		if err := ompCheckReadOnlyVersion(ctx, a.Command, launchDir); err != nil {
			return "", err
		}
	}
	// omp reads a non-TTY stdin as prompt text, so the prompt goes in a file
	// and stdin stays empty.
	promptPath, cleanup, err := createPromptFile("omp", prompt)
	if err != nil {
		return "", err
	}
	defer cleanup()
	// --cwd is resolved against the launch directory, so it gets an absolute
	// path.
	workDir, err := filepath.Abs(repoPath)
	if err != nil {
		return "", fmt.Errorf("resolve omp working directory: %w", err)
	}
	args := append(a.buildArgs(agentic), "--cwd", workDir,
		"@"+promptPath, "Please follow the instructions in the attached file to review the code.")

	var stream ompStream
	result, err := runStreamingCLI(ctx, streamingCLISpec{
		Name: "omp", Command: a.Command, Args: args,
		Dir: launchDir, Output: output, StreamStderr: true, DrainStdout: true,
		Parse: func(r io.Reader, sw *syncWriter) (string, error) {
			if err := scanStreamJSONLines(r, sw, stream.observe); err != nil {
				return stream.Text, fmt.Errorf("read omp stream: %w", err)
			}
			return stream.Text, nil
		},
	})
	if err != nil {
		return "", err
	}
	if err := ompRunError(result, stream); err != nil {
		return "", err
	}
	if result.ParseErr != nil {
		return result.Result, result.ParseErr
	}
	if result.Result == "" {
		return "No review output generated", nil
	}
	return result.Result, nil
}

// ompRunError reports why an omp run failed. JSON mode writes the provider's
// message and the reason for a stop only to the event stream.
func ompRunError(result streamingCLIResult, stream ompStream) error {
	var reason string
	if stream.StopReason != "" {
		reason = ": " + stream.StopReason
	}
	exitErr, exited := errors.AsType[*exec.ExitError](result.WaitErr)
	switch {
	case exited && exitErr.ExitCode() == ompCostCapExit:
		// The cost cap's exit status outranks a provider error earlier in the
		// stream. A retry starts a new session that can spend up to the cap
		// again, so the error must classify as permanent.
		return fmt.Errorf("%s (%w)%s\nstderr: %s", ompCostCapStopped, result.WaitErr, reason, result.Stderr)
	case stream.Error != "":
		return fmt.Errorf("omp provider error: %s", stream.Error)
	case result.WaitErr == nil:
		return nil
	case reason != "":
		return fmt.Errorf("omp failed: %w%s\nstderr: %s", result.WaitErr, reason, result.Stderr)
	default:
		return formatStreamingCLIWaitError("omp", result, result.Stderr)
	}
}

// ompStream is what an omp --mode json event stream reports: Pi's events, and
// why omp stopped a run early.
type ompStream struct {
	piStream
	StopReason string
}

func (s *ompStream) observe(line string) error {
	if reason := ompStopReason(line); reason != "" {
		s.StopReason = reason
	}
	return s.piStream.observe(line)
}

// ompStopReason returns the cost gate's refusal or an aborted assistant
// message's error when line reports one.
func ompStopReason(line string) string {
	if !strings.Contains(line, `"cost-gate"`) && !strings.Contains(line, `"aborted"`) {
		return ""
	}
	var ev struct {
		Type    string         `json:"type"`
		Level   string         `json:"level"`
		Source  string         `json:"source"`
		Message jsontext.Value `json:"message"`
	}
	if json.Unmarshal([]byte(line), &ev) != nil {
		return ""
	}
	switch ev.Type {
	case "notice":
		var message string
		if ev.Level == "error" && ev.Source == "cost-gate" && json.Unmarshal(ev.Message, &message) == nil {
			return strings.TrimSpace(message)
		}
	case "message_end":
		var message struct {
			Role         string `json:"role"`
			StopReason   string `json:"stopReason"`
			ErrorMessage string `json:"errorMessage"`
		}
		if json.Unmarshal(ev.Message, &message) == nil && message.Role == "assistant" && message.StopReason == "aborted" {
			return strings.TrimSpace(message.ErrorMessage)
		}
	}
	return ""
}

// ompCheckReadOnlyVersion returns an error unless command is an omp release
// that honors --no-tools. omp prints its version as "omp/19.2.1". The probe
// runs before every read-only run, not from a cache keyed on the command
// file: a wrapper script or a version manager shim can switch the omp behind
// an unchanged file, or pick a version by directory. It starts in launchDir
// with the run's environment, so it resolves the same omp as the run.
func ompCheckReadOnlyVersion(ctx context.Context, command, launchDir string) error {
	cmd := exec.CommandContext(ctx, command, "--version")
	cmd.Dir = launchDir
	configureSubprocess(ctx, cmd)
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("check omp version: %w", procutil.WithStderr(err))
	}
	version := strings.TrimSpace(string(out))
	if !versionAtLeast(strings.ReplaceAll(version, "/", " "), ompReadOnlyMinVersion) {
		return fmt.Errorf("omp reviews need omp %s or later, which limits a run to read-only tools, but %q reports %q; upgrade omp", ompReadOnlyMinVersion, command, version)
	}
	return nil
}

func init() { Register(NewOmpAgent("")) }
