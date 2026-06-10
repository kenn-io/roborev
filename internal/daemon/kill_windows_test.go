//go:build windows

package daemon

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testRoborevExe = `C:\Program Files\roborev\roborev.exe`

func TestClassifyCommandLine(t *testing.T) {
	tests := []struct {
		group string
		cases []struct {
			name    string
			cmdLine string
			want    processIdentity
		}
	}{
		{
			group: "Standard Matching",
			cases: []struct {
				name    string
				cmdLine string
				want    processIdentity
			}{
				{
					name:    "empty string returns unknown",
					cmdLine: "",
					want:    processUnknown,
				},
				{
					name:    "whitespace only returns unknown",
					cmdLine: "   \t\n  ",
					want:    processUnknown,
				},
				{
					name:    "roborev daemon run returns isRoborev",
					cmdLine: testRoborevExe + ` daemon run`,
					want:    processIsRoborev,
				},
				{
					name:    "roborev daemon run with flags after returns isRoborev",
					cmdLine: testRoborevExe + ` daemon run --port 7373`,
					want:    processIsRoborev,
				},
				{
					name:    "roborev daemon run with flags between returns isRoborev",
					cmdLine: testRoborevExe + ` daemon --verbose run`,
					want:    processIsRoborev,
				},
				{
					name:    "roborev daemon run with multiple flags between returns isRoborev",
					cmdLine: testRoborevExe + ` daemon -v --config C:\roborev.toml run`,
					want:    processIsRoborev,
				},
				{
					name:    "roborev daemon status returns notRoborev",
					cmdLine: testRoborevExe + ` daemon status`,
					want:    processNotRoborev,
				},
				{
					name:    "roborev daemon stop returns notRoborev",
					cmdLine: testRoborevExe + ` daemon stop`,
					want:    processNotRoborev,
				},
				{
					name:    "roborev without daemon returns notRoborev",
					cmdLine: testRoborevExe + ` review`,
					want:    processNotRoborev,
				},
				{
					name:    "unrelated process returns notRoborev",
					cmdLine: `C:\Windows\System32\notepad.exe`,
					want:    processNotRoborev,
				},
				{
					name:    "case insensitive match",
					cmdLine: `C:\ROBOREV\ROBOREV.EXE DAEMON RUN`,
					want:    processIsRoborev,
				},
			},
		},
		{
			group: "False Positive Prevention",
			cases: []struct {
				name    string
				cmdLine string
				want    processIdentity
			}{
				{
					name:    "dry-run flag with daemon status",
					cmdLine: testRoborevExe + ` daemon status --dry-run`,
					want:    processNotRoborev,
				},
				{
					name:    "run-once flag with daemon stop",
					cmdLine: testRoborevExe + ` daemon stop --run-once`,
					want:    processNotRoborev,
				},
				{
					name:    "run as flag value after status",
					cmdLine: testRoborevExe + ` daemon status --output run`,
					want:    processNotRoborev,
				},
				{
					name:    "run as positional arg after status",
					cmdLine: testRoborevExe + ` daemon status run`,
					want:    processNotRoborev,
				},
			},
		},
	}

	for _, g := range tests {
		t.Run(g.group, func(t *testing.T) {
			for _, tt := range g.cases {
				t.Run(tt.name, func(t *testing.T) {
					got := classifyCommandLine(tt.cmdLine)
					assert.Equal(t, tt.want, got,
						"classifyCommandLine(%q) = %v, want %v", tt.cmdLine, got, tt.want)
				})
			}
		})
	}
}

func TestParseWmicOutput(t *testing.T) {
	// Verify that WMIC output containing only the header is treated as empty.
	// This simulates the flow in getCommandLineWmic without spawning wmic.

	tests := []struct {
		name       string
		wmicOutput string
		want       string
	}{
		{
			name:       "header only",
			wmicOutput: "CommandLine\r\n",
			want:       "",
		},
		{
			name:       "header with trailing whitespace",
			wmicOutput: "CommandLine  \r\n  \r\n",
			want:       "",
		},
		{
			name:       "header with UTF-16LE BOM (after NUL removal)",
			wmicOutput: "\xff\xfeCommandLine\r\n",
			want:       "",
		},
		{
			name:       "header with actual data",
			wmicOutput: "CommandLine\r\nroborev.exe daemon run --port 7373\r\n",
			want:       "roborev.exe daemon run --port 7373",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parseWmicOutput(tt.wmicOutput)
			assert.Equal(t, tt.want, result,
				"parseWmicOutput(%q) = %q, want %q", tt.wmicOutput, result, tt.want)
		})
	}
}

func TestNormalizeCommandLine(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "simple string unchanged",
			input: "roborev daemon run",
			want:  "roborev daemon run",
		},
		{
			name:  "strips leading/trailing whitespace",
			input: "  roborev daemon run  \n",
			want:  "roborev daemon run",
		},
		{
			name:  "strips NUL bytes",
			input: "r\x00o\x00b\x00o\x00r\x00e\x00v\x00",
			want:  "roborev",
		},
		{
			name:  "strips UTF-8 BOM",
			input: "\xef\xbb\xbfroborev daemon",
			want:  "roborev daemon",
		},
		{
			name:  "handles UTF-16LE style with NULs",
			input: "r\x00o\x00b\x00o\x00r\x00e\x00v\x00 \x00d\x00a\x00e\x00m\x00o\x00n\x00",
			want:  "roborev daemon",
		},
		{
			name:  "empty string stays empty",
			input: "",
			want:  "",
		},
		{
			name:  "only whitespace becomes empty",
			input: "   \t\n  ",
			want:  "",
		},
		{
			name:  "only NULs becomes empty",
			input: "\x00\x00\x00",
			want:  "",
		},
		{
			name:  "BOM with whitespace becomes empty",
			input: "\xef\xbb\xbf   ",
			want:  "",
		},
		{
			name:  "strips UTF-16LE BOM after NUL removal",
			input: "\xff\xferoborev daemon",
			want:  "roborev daemon",
		},
		{
			name:  "strips UTF-16BE BOM",
			input: "\xfe\xffroborev daemon",
			want:  "roborev daemon",
		},
		{
			name:  "handles real UTF-16LE with BOM",
			input: "\xff\x00\xfe\x00r\x00o\x00b\x00o\x00r\x00e\x00v\x00",
			want:  "roborev", // After NUL removal: \xff\xferoborev, then BOM stripped
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeCommandLine(tt.input)
			assert.Equal(t, tt.want, got,
				"normalizeCommandLine(%q) = %q, want %q", tt.input, got, tt.want)
		})
	}
}

func TestProcessExistsUsesWin32APIWithoutSpawning(t *testing.T) {
	assert := assert.New(t)

	// The current process certainly exists.
	assert.True(processExists(os.Getpid()))
	assert.True(ProcessExists(os.Getpid()))

	// Invalid PIDs are never alive.
	assert.False(processExists(0))
	assert.False(processExists(-1))
	assert.False(ProcessExists(0))
}

func TestProcessExistsDetectsExitedProcess(t *testing.T) {
	// Spawn a short-lived hidden process and wait for it to exit.
	cmd := HiddenCommand("cmd.exe", "/c", "exit 0")
	require.NoError(t, cmd.Start())
	pid := cmd.Process.Pid
	require.NoError(t, cmd.Wait())

	assert.False(t, processExists(pid), "exited process must not be reported alive")
}
