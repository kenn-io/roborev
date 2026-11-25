package agent

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os/exec"
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
	if !agentic && !ompLimitsTools(ctx, a.Command) {
		return "", fmt.Errorf("omp reviews need omp %s or later, which limits a run to read-only tools; upgrade omp", ompToolLimitVersion)
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
	result, err := runStreamingCLI(ctx, streamingCLISpec{
		Name: "omp", Command: a.Command, Args: args,
		Dir: repoPath, Output: output, StreamStderr: true, CaptureStdout: true, DrainStdout: true,
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
		// A cost cap or an abort also ends the run with a non-zero exit, and
		// JSON mode reports why only in the event stream.
		if reason := ompStopReason(result.Stdout); reason != "" {
			return "", fmt.Errorf("omp failed: %w: %s\nstderr: %s", result.WaitErr, reason, result.Stderr)
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

// ompToolLimitVersion is the first omp release whose --no-tools limits a run
// to the tools --tools names. Earlier releases accept --no-tools but still
// add MCP, extension and custom tools, and a user's tools.approval allow rule
// lets them run under --approval-mode always-ask (omp 19.2.0 changelog,
// Breaking Changes).
const ompToolLimitVersion = "19.2.0"

var ompToolLimitSupport capabilityCache

// ompLimitsTools reports whether command is an omp release that honors
// --no-tools. omp prints its version as "omp/19.2.1"; a version it does not
// report counts as too old.
func ompLimitsTools(ctx context.Context, command string) bool {
	cacheKey := capabilityCacheKey(command)
	if cached, ok := ompToolLimitSupport.Load(cacheKey); ok {
		return cached.(bool)
	}
	cmd := exec.CommandContext(ctx, command, "--version")
	configureCapabilityProbe(ctx, cmd)
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	supported := versionAtLeast(strings.ReplaceAll(string(out), "/", " "), ompToolLimitVersion)
	ompToolLimitSupport.Store(cacheKey, supported)
	return supported
}

// ompStopReason returns why omp stopped a run early: the message of the last
// error notice, such as the cost gate's refusal, or of an aborted assistant
// message. It returns "" when the stream names no reason.
func ompStopReason(stdout string) string {
	var reason string
	_ = scanStreamJSONLines(strings.NewReader(stdout), nil, func(line string) error {
		var ev struct {
			Type    string         `json:"type"`
			Level   string         `json:"level"`
			Message jsontext.Value `json:"message"`
		}
		if json.Unmarshal([]byte(line), &ev) != nil {
			return nil
		}
		switch {
		case ev.Type == "notice" && ev.Level == "error":
			var message string
			if json.Unmarshal(ev.Message, &message) == nil && strings.TrimSpace(message) != "" {
				reason = strings.TrimSpace(message)
			}
		case ev.Type == "message_end":
			var message struct {
				Role         string `json:"role"`
				StopReason   string `json:"stopReason"`
				ErrorMessage string `json:"errorMessage"`
			}
			if json.Unmarshal(ev.Message, &message) == nil && message.Role == "assistant" &&
				message.StopReason == "aborted" && strings.TrimSpace(message.ErrorMessage) != "" {
				reason = strings.TrimSpace(message.ErrorMessage)
			}
		}
		return nil
	})
	return reason
}

func init() { Register(NewOmpAgent("")) }
