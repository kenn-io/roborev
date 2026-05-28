package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeTempCommand writes script to a temporary executable file and returns
// its path. The file is cleaned up when the test finishes.
//
// # ETXTBSY probes and Go 1.25
//
// Go 1.25 added a built-in ETXTBSY probe to os/exec: every exec.Cmd.Start()
// forks a child that calls execve(path, ["path", "--help-probe-etxtbsy"]) to
// check whether the file is still being written. If execve succeeds (no
// ETXTBSY), the probe child has *actually started running the command* — it
// is not killed or cleaned up, and the parent waits for it to exit.
//
// For most commands this is harmless because the probe exits quickly. But
// shell scripts that ignore their arguments will execute their full body
// inside the probe child. A script containing "sleep 60" will spawn a probe
// process that sleeps for 60 real seconds, and the Go test binary blocks
// until it finishes (confirmed via strace: the probe child runs
// clock_nanosleep for the full duration, and the test binary's waitid blocks
// on it).
//
// This means every shell script launched via os/exec in Go 1.25 runs twice:
// once as the ETXTBSY probe, once for real. Additionally, writeTempCommand
// itself does a manual probe (the retry loop below), so slow scripts can
// run three times total.
//
// If your test script does anything expensive or long-running, add this
// guard as the first line after the shebang:
//
//	case "$1" in *etxtbsy*) exit 0;; esac
//
// This makes the probe child exit immediately while the real invocation
// (which receives no arguments, or receives the agent's real arguments)
// runs normally. See https://github.com/golang/go/issues/22315 for
// background on Go's ETXTBSY handling.
func writeTempCommand(t *testing.T, script string) string {
	t.Helper()
	skipIfWindows(t)

	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "cmd")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		t.Fatalf("write temp command: %v", err)
	}
	if _, err := f.Write([]byte(script)); err != nil {
		f.Close()
		t.Fatalf("write temp command: %v", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		t.Fatalf("write temp command sync: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("write temp command close: %v", err)
	}
	const maxRetries = 10
	for i := range maxRetries {
		_, err = exec.Command(path, "--help-probe-etxtbsy").CombinedOutput()
		if err == nil || !strings.Contains(err.Error(), "text file busy") {
			break
		}
		if i == maxRetries-1 {
			t.Fatalf("write temp command: ETXTBSY persisted after %d retries", maxRetries)
		}
		time.Sleep(time.Duration(1<<i) * time.Millisecond)
	}
	return path
}

// MockCLIOpts controls the behavior of a mock agent CLI script.
type MockCLIOpts struct {
	HelpOutput   string
	ExitCode     int
	CaptureArgs  bool
	CaptureStdin bool
	CaptureEnv   bool
	StdoutLines  []string
	StderrLines  []string
}

// MockCLIResult holds paths to the mock command and any capture files.
type MockCLIResult struct {
	CmdPath   string
	ArgsFile  string
	StdinFile string
	EnvFile   string
}

// readMockArgs reads the captured arguments from a mock CLI's ArgsFile and splits them into a slice.
func readMockArgs(t *testing.T, path string) []string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read args file %s: %v", path, err)
	}
	return strings.Split(strings.TrimSpace(string(content)), " ")
}

// readMockEnv reads the captured env from a mock CLI's EnvFile.
// Returns each env var as a "KEY=VALUE" string.
func readMockEnv(t *testing.T, path string) []string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read env file %s: %v", path, err)
	}
	trimmed := strings.TrimRight(string(content), "\n")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

// assertContainsArg checks that args contains target, failing with a descriptive message.
func assertContainsArg(t *testing.T, args []string, target string) {
	t.Helper()
	if !containsString(args, target) {
		t.Fatalf("expected %q in args, got %v", target, args)
	}
}

// assertNotContainsArg checks that args does not contain target.
func assertNotContainsArg(t *testing.T, args []string, target string) {
	t.Helper()
	if containsString(args, target) {
		t.Fatalf("unexpected %q in args %v", target, args)
	}
}

// mockAgentCLI creates a temporary shell script that simulates an agent CLI.
// It handles --help output, argument/stdin capture, and exit codes.
func mockAgentCLI(t *testing.T, opts MockCLIOpts) *MockCLIResult {
	t.Helper()
	skipIfWindows(t)

	tmpDir := t.TempDir()
	result := &MockCLIResult{}

	var script strings.Builder
	script.WriteString("#!/bin/sh\n")

	if opts.HelpOutput != "" {
		helpFile := filepath.Join(tmpDir, "help_output.txt")
		if err := os.WriteFile(helpFile, []byte(opts.HelpOutput), 0o644); err != nil {
			t.Fatalf("write help output file: %v", err)
		}
		script.WriteString(fmt.Sprintf(`case "$*" in *--help*) cat %q; exit 0;; esac`, helpFile) + "\n")
	}

	if opts.CaptureArgs {
		result.ArgsFile = filepath.Join(tmpDir, "args.txt")
		fmt.Fprintf(&script, "echo \"$@\" > %q\n", result.ArgsFile)
	}

	if opts.CaptureStdin {
		result.StdinFile = filepath.Join(tmpDir, "stdin.txt")
		fmt.Fprintf(&script, "cat > %q\n", result.StdinFile)
	}

	if opts.CaptureEnv {
		result.EnvFile = filepath.Join(tmpDir, "env.txt")
		fmt.Fprintf(&script, "env > %q\n", result.EnvFile)
	}

	if len(opts.StdoutLines) > 0 {
		stdoutFile := filepath.Join(tmpDir, "stdout.txt")
		content := strings.Join(opts.StdoutLines, "\n")
		if !strings.HasSuffix(content, "\n") {
			content += "\n"
		}
		if err := os.WriteFile(stdoutFile, []byte(content), 0o644); err != nil {
			t.Fatalf("write stdout file: %v", err)
		}
		script.WriteString(fmt.Sprintf(`cat %q`, stdoutFile) + "\n")
	}

	if len(opts.StderrLines) > 0 {
		stderrFile := filepath.Join(tmpDir, "stderr.txt")
		content := strings.Join(opts.StderrLines, "\n")
		if !strings.HasSuffix(content, "\n") {
			content += "\n"
		}
		if err := os.WriteFile(stderrFile, []byte(content), 0o644); err != nil {
			t.Fatalf("write stderr file: %v", err)
		}
		script.WriteString(fmt.Sprintf(`cat %q >&2`, stderrFile) + "\n")
	}

	script.WriteString("exit " + strings.TrimSpace(fmt.Sprintf("%d", opts.ExitCode)) + "\n")

	result.CmdPath = writeTempCommand(t, script.String())
	return result
}

// makeToolCallJSON returns a JSON string representing a tool call with the
// given name and arguments. It is useful for building test inputs that
// contain tool-call lines without hardcoding brittle JSON strings.
func makeToolCallJSON(name string, args map[string]any) string {
	m := map[string]any{
		"name":      name,
		"arguments": args,
	}
	b, err := json.Marshal(m)
	if err != nil {
		panic(fmt.Sprintf("makeToolCallJSON: %v", err))
	}
	return string(b)
}

// ScriptBuilder helps construct shell scripts for mocking CLI output in tests.
type ScriptBuilder struct {
	lines []string
}

// NewScriptBuilder creates a new ScriptBuilder with the shebang line.
func NewScriptBuilder() *ScriptBuilder {
	return &ScriptBuilder{lines: []string{"#!/bin/sh"}}
}

// AddOutput adds a line that echoes the given string to stdout.
func (b *ScriptBuilder) AddOutput(s string) *ScriptBuilder {
	b.lines = append(b.lines, fmt.Sprintf("echo %q", s))
	return b
}

// AddToolCall adds a line that prints a tool-call JSON object to stdout.
func (b *ScriptBuilder) AddToolCall(name string, args map[string]any) *ScriptBuilder {
	j := makeToolCallJSON(name, args)
	escaped := strings.ReplaceAll(j, "'", "'\\''")
	b.lines = append(b.lines, fmt.Sprintf("printf '%%s\\n' '%s'", escaped))
	return b
}

// AddRaw adds a raw shell line to the script.
func (b *ScriptBuilder) AddRaw(line string) *ScriptBuilder {
	b.lines = append(b.lines, line)
	return b
}

// Build returns the complete shell script as a string.
func (b *ScriptBuilder) Build() string {
	return strings.Join(b.lines, "\n") + "\n"
}
