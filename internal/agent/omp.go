package agent

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"

	"go.kenn.io/roborev/internal/procutil"
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

// PlanningCommandLine returns omp's representative read-only planning command.
func (a *OmpAgent) PlanningCommandLine() string {
	return a.Command + " " + strings.Join(a.buildArgs(false), " ")
}

func (a *OmpAgent) Review(ctx context.Context, repoPath, commitSHA, prompt string, output io.Writer) (string, error) {
	// omp has no mode that ignores project configuration: it loads the
	// checkout's .env, which can redirect provider requests and the
	// credentials they carry, and reads its .omp/config.yml.
	if untrustedCheckout(ctx) {
		return "", errors.New("omp cannot review an untrusted checkout: it has no mode that ignores project configuration")
	}
	agentic := effectiveAgentic(ctx, a.Agentic)
	if !agentic {
		if err := ompCheckReadOnlyVersion(ctx, a.Command, repoPath); err != nil {
			return "", err
		}
	}
	// A file keeps large prompts off the command line. omp reads a non-TTY
	// stdin as prompt text, so stdin stays empty.
	promptPath, cleanup, err := createPromptFile("omp", prompt)
	if err != nil {
		return "", err
	}
	defer cleanup()
	args := append(a.buildArgs(agentic),
		"@"+promptPath, "Please follow the instructions in the attached file to review the code.")

	var stream piStream
	var stopReason string
	result, err := runStreamingCLI(ctx, streamingCLISpec{
		Name: "omp", Command: a.Command, Args: args,
		Dir: repoPath, Output: output, StreamStderr: true, DrainStdout: true,
		Parse: func(r io.Reader, sw *syncWriter) (string, error) {
			err := scanStreamJSONLines(r, sw, func(line string) error {
				if reason := ompStopReason(line); reason != "" {
					stopReason = reason
				}
				return stream.observe(line)
			})
			if err != nil {
				return stream.Text, fmt.Errorf("read omp stream: %w", err)
			}
			return stream.Text, nil
		},
	})
	if err != nil {
		return "", err
	}
	// omp's cost cap (--max-cost / session.maxCost) ends a run with its own
	// exit status, whatever the stream reported before it. A retry starts a
	// new session that can spend up to the cap again, so the job fails over.
	// Callers that classify by error text alone match "cost cap reached"
	// through the omp rule in defaultLimitRules.
	if exitErr, ok := errors.AsType[*exec.ExitError](result.WaitErr); ok && exitErr.ExitCode() == ompCostCapExit {
		reason := stopReason
		if reason == "" {
			reason = "cost cap reached"
		}
		return "", WithLimitClassification(fmt.Errorf("omp failed: %w: %s\nstderr: %s", result.WaitErr, reason, result.Stderr),
			LimitClassification{Kind: LimitKindPermanent, Agent: "omp", Message: reason})
	}
	// omp exits 1 when the provider ends the run with an error, and its JSON
	// mode reports the provider's message only in the event stream.
	if stream.Error != "" {
		return "", fmt.Errorf("omp provider error: %s", stream.Error)
	}
	if result.WaitErr != nil {
		// An abort also ends the run with a non-zero exit, and JSON mode
		// reports why only in the event stream.
		if stopReason != "" {
			return "", fmt.Errorf("omp failed: %w: %s\nstderr: %s", result.WaitErr, stopReason, result.Stderr)
		}
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

// ompReadOnlyMinVersion is the first omp release whose --no-tools limits a run
// to the tools --tools names. Earlier releases accept --no-tools but still
// add MCP, extension and custom tools, and a user's tools.approval allow rule
// lets them run under --approval-mode always-ask (omp 19.2.0 changelog,
// Breaking Changes).
const ompReadOnlyMinVersion = "19.2.0"

// ompCostCapExit is the exit status of an omp print-mode run that its cost
// cap stopped (omp 19.2.1 modes/print-mode.ts).
const ompCostCapExit = 3

// ompCheckReadOnlyVersion returns an error unless command is an omp release
// that honors --no-tools. omp prints its version as "omp/19.2.1". The probe
// runs from repoPath before every read-only run, not from a cache keyed on
// the command file: a wrapper script or a version manager shim can switch the
// omp behind an unchanged file, or pin a different omp per directory.
func ompCheckReadOnlyVersion(ctx context.Context, command, repoPath string) error {
	// The review run resolves a relative path such as ./bin/omp against its
	// working directory, so the probe must too.
	if !filepath.IsAbs(command) && strings.ContainsAny(command, `/\`) {
		command = filepath.Join(repoPath, command)
	}
	cmd := exec.CommandContext(ctx, command, "--version")
	configureCapabilityProbe(ctx, cmd)
	cmd.Dir = repoPath
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

// ompStopReason returns why omp stopped a run early when line reports it: the
// cost gate's refusal, or the error message of an aborted assistant message.
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

func init() { Register(NewOmpAgent("")) }
