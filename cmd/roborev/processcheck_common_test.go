package main

import (
	"encoding/binary"
	"testing"
	"unicode/utf16"

	"github.com/stretchr/testify/require"
)

func TestIsRoborevDaemonCommandForUpdate(t *testing.T) {
	tests := []struct {
		name    string
		cmdLine string
		want    bool
	}{
		{
			name:    "unix daemon run",
			cmdLine: "/usr/local/bin/roborev daemon run",
			want:    true,
		},
		{
			name:    "windows daemon run",
			cmdLine: `C:\Tools\roborev.exe daemon run --port 7373`,
			want:    true,
		},
		{
			name:    "quoted windows args daemon run",
			cmdLine: `"C:\Tools\roborev.exe" "daemon" "run" "--port" "7373"`,
			want:    true,
		},
		{
			name:    "plain flag value before run",
			cmdLine: "roborev daemon --profile prod run",
			want:    true,
		},
		{
			name:    "daemon status command",
			cmdLine: "/usr/local/bin/roborev daemon status",
			want:    false,
		},
		{
			name:    "daemon status with output run",
			cmdLine: "roborev daemon status --output run",
			want:    false,
		},
		{
			name:    "non roborev process",
			cmdLine: "python worker.py",
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isRoborevDaemonCommandForUpdate(tt.cmdLine)
			require.Equal(t, tt.want, got, "isRoborevDaemonCommandForUpdate(%q)", tt.cmdLine)
		})
	}
}

func TestNormalizeCommandLineForUpdate(t *testing.T) {
	raw := "\xff\xferoborev\x00daemon\x00run\r\n"
	got := normalizeCommandLineForUpdate(raw)
	require.Equal(t, "roborev daemon run", got, "normalizeCommandLineForUpdate(%q)", raw)
}

func TestParseWmicOutputForUpdateUTF16LE(t *testing.T) {
	raw := encodeUTF16LE(
		"CommandLine\r\nC:\\Tools\\roborev.exe daemon run --port 7373\r\n", true,
	)
	got := parseWmicOutputForUpdate(raw)
	want := `C:\Tools\roborev.exe daemon run --port 7373`
	require.Equal(t, want, got, "parseWmicOutputForUpdate()")
	require.True(t, isRoborevDaemonCommandForUpdate(got), "expected parsed WMIC output to classify as daemon command")
}

func TestNormalizeCommandLineBytesForUpdateUTF16LENoBOM(t *testing.T) {
	raw := encodeUTF16LE("roborev.exe daemon run --port 7373\r\n", false)
	got := normalizeCommandLineBytesForUpdate(raw)
	want := "roborev.exe daemon run --port 7373"
	require.Equal(t, want, got, "normalizeCommandLineBytesForUpdate()")
}

func encodeUTF16LE(s string, withBOM bool) []byte {
	u16 := utf16.Encode([]rune(s))
	offset := 0
	if withBOM {
		offset = 2
	}
	out := make([]byte, offset+len(u16)*2)
	if withBOM {
		out[0] = 0xff
		out[1] = 0xfe
	}
	for i, r := range u16 {
		binary.LittleEndian.PutUint16(out[offset+i*2:offset+i*2+2], r)
	}
	return out
}
