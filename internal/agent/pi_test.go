package agent

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/roborev/internal/config"
)

func TestParsePiJSON(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"session","id":"pi-session-123"}`,
		`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"Hello"},{"type":"text","text":"World"}]}}`,
	}, "\n") + "\n"

	result, err := parsePiJSON(strings.NewReader(input))
	require.NoError(t, err)
	assert.Equal(t, "Hello\nWorld", result)
}

func TestParsePiJSONLargeMessage(t *testing.T) {
	bigText := strings.Repeat("x", 128*1024)
	input := `{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"` + bigText + `"}]}}` + "\n"

	result, err := parsePiJSON(strings.NewReader(input))
	require.NoError(t, err)
	assert.Equal(t, bigText, result)
}

func TestResolvePiSessionPath(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", dataDir)

	sessionID := "46109439-3160-40f0-81e7-7dfa4f3647b3"
	sessionPath := filepath.Join(dataDir, "sessions", "--repo--", "2026-03-08T18-44-39-718Z_"+sessionID+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(sessionPath), 0o755))
	require.NoError(t, os.WriteFile(sessionPath, []byte("{}\n"), 0o644))
	assert.Equal(t, sessionPath, resolvePiSessionPath(sessionID))
}

func TestPiReviewSessionFlag(t *testing.T) {
	skipIfWindows(t)

	dataDir := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", dataDir)

	sessionID := "46109439-3160-40f0-81e7-7dfa4f3647b3"
	sessionPath := filepath.Join(dataDir, "sessions", "--repo--", "2026-03-08T18-44-39-718Z_"+sessionID+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(sessionPath), 0o755))
	require.NoError(t, os.WriteFile(sessionPath, []byte("{}\n"), 0o644))

	mock := mockAgentCLI(t, MockCLIOpts{
		CaptureArgs: true,
		StdoutLines: []string{
			`{"type":"session","id":"` + sessionID + `"}`,
			`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"ok"}]}}`,
		},
	})

	a := NewPiAgent(mock.CmdPath).WithSessionID(sessionID).(*PiAgent)
	result, err := a.Review(context.Background(), t.TempDir(), "HEAD", "prompt", &bytes.Buffer{})
	require.NoError(t, err)
	assert.Equal(t, "ok", result)

	args := readMockArgs(t, mock.ArgsFile)
	assertContainsArg(t, args, "--session")
	assertContainsArg(t, args, sessionPath)
	assertContainsArg(t, args, "--mode")
	assertContainsArg(t, args, "json")
}

func TestPiCommandLineOmitsResolvedSessionPath(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", dataDir)

	sessionID := "46109439-3160-40f0-81e7-7dfa4f3647b3"
	sessionPath := filepath.Join(dataDir, "sessions", "--repo--", "2026-03-08T18-44-39-718Z_"+sessionID+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(sessionPath), 0o755))
	require.NoError(t, os.WriteFile(sessionPath, []byte("{}\n"), 0o644))

	a := NewPiAgent("pi").WithSessionID(sessionID).(*PiAgent)
	cmdLine := a.CommandLine()

	assert.NotContains(t, cmdLine, "--session")
	assert.NotContains(t, cmdLine, sessionPath)
	assert.Contains(t, cmdLine, "-p --mode json")
}

func TestPiClassifyWithSchemaUsesLockedDownSchemaOutput(t *testing.T) {
	skipIfWindows(t)

	tmpDir := t.TempDir()
	argsFile := filepath.Join(tmpDir, "args.txt")
	promptFile := filepath.Join(tmpDir, "prompt.txt")
	script := fmt.Sprintf(`#!/bin/sh
case "$1" in *etxtbsy*) exit 0;; esac
args_file=%q
prompt_file=%q
: > "$args_file"
json_output=""
prev=""
for arg in "$@"; do
  printf '%%s\n' "$arg" >> "$args_file"
  case "$arg" in
    @*) cat "${arg#@}" > "$prompt_file" ;;
  esac
  if [ "$prev" = "--json-output" ]; then
    json_output="$arg"
  fi
  prev="$arg"
done
if [ -z "$json_output" ]; then
  echo "missing --json-output" >&2
  exit 2
fi
echo "pi progress"
printf '%%s\n' '{"design_review":false,"reason":"pi schema"}' > "$json_output"
`, argsFile, promptFile)

	cli := writeTempCommand(t, script)
	agent := NewPiAgent(cli)
	agent.Provider = "anthropic"
	agent.Model = "sonnet"
	agent.Reasoning = ReasoningFast

	schema := jsonRaw(`{"type":"object","additionalProperties":false,"required":["design_review","reason"],"properties":{"design_review":{"type":"boolean"},"reason":{"type":"string"}}}`)
	var output bytes.Buffer
	result, err := agent.ClassifyWithSchema(context.Background(), t.TempDir(), "HEAD", "classify me", schema, &output)
	require.NoError(t, err)
	assert.JSONEq(t, `{"design_review":false,"reason":"pi schema"}`, string(result))
	assert.Contains(t, output.String(), "pi progress")

	args := readLineFile(t, argsFile)
	for _, want := range []string{
		"--no-session",
		"--no-extensions",
		"--no-builtin-tools",
		"--no-skills",
		"--no-prompt-templates",
		"--no-themes",
		"--no-context-files",
		"--extension",
		config.DefaultPiJSONSchemaExtension,
		"--json-fallback",
		"none",
		"-p",
		"--provider",
		"anthropic",
		"--model",
		"sonnet",
		"--thinking",
		"low",
	} {
		assert.Contains(t, args, want)
	}
	assert.Equal(t, string(schema), argAfter(args, "--json-schema"))
	assert.Contains(t, argAfter(args, "--json-output"), "result.json")
	assert.Equal(t, "classify me", strings.TrimSpace(readTextFile(t, promptFile)))
}

func TestPiClassifyWithSchemaRejectsInvalidJSONOutput(t *testing.T) {
	skipIfWindows(t)

	script := `#!/bin/sh
case "$1" in *etxtbsy*) exit 0;; esac
json_output=""
prev=""
for arg in "$@"; do
  if [ "$prev" = "--json-output" ]; then
    json_output="$arg"
  fi
  prev="$arg"
done
printf '%s\n' 'not json' > "$json_output"
`

	_, err := NewPiAgent(writeTempCommand(t, script)).ClassifyWithSchema(
		context.Background(),
		t.TempDir(),
		"HEAD",
		"classify me",
		jsonRaw(`{"type":"object"}`),
		nil,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not valid JSON")
}

func TestPiClassifyWithSchemaReportsCommandFailure(t *testing.T) {
	skipIfWindows(t)

	script := `#!/bin/sh
case "$1" in *etxtbsy*) exit 0;; esac
echo "boom" >&2
exit 12
`

	_, err := NewPiAgent(writeTempCommand(t, script)).ClassifyWithSchema(
		context.Background(),
		t.TempDir(),
		"HEAD",
		"classify me",
		jsonRaw(`{"type":"object"}`),
		nil,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pi classifier failed")
	assert.Contains(t, err.Error(), "boom")
}

func jsonRaw(s string) []byte {
	return []byte(s)
}

func readLineFile(t *testing.T, path string) []string {
	t.Helper()
	content := readTextFile(t, path)
	trimmed := strings.TrimRight(content, "\n")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

func readTextFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(content)
}

func argAfter(args []string, flag string) string {
	for i, arg := range args {
		if arg == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}
