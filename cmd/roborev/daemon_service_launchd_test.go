package main

import (
	"encoding/binary"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func procArgsFixture(executable string, args, env []string) []byte {
	data := binary.NativeEndian.AppendUint32(nil, uint32(len(args)))
	data = append(data, executable...)
	data = append(data, 0, 0, 0)
	for _, entry := range append(append([]string(nil), args...), env...) {
		data = append(data, entry...)
		data = append(data, 0)
	}
	return append(data, 0)
}

func TestParseProcArgs2(t *testing.T) {
	invalidArgc := func(argc uint32) []byte {
		data := procArgsFixture("/opt/example tool", []string{"tool"}, []string{"PATH=/bin"})
		binary.NativeEndian.PutUint32(data[:4], argc)
		return data
	}
	for _, tc := range []struct {
		name  string
		data  []byte
		label string
		bad   bool
	}{
		{name: "label", data: procArgsFixture("/opt/example tool", []string{"tool", "", "run"}, []string{"PATH=/bin", "XPC_SERVICE_NAME=com.example.worker"}), label: "com.example.worker"},
		{name: "argv is not environment", data: procArgsFixture("/opt/example tool", []string{"tool", "XPC_SERVICE_NAME=wrong"}, []string{"PATH=/bin"})},
		{name: "zero label", data: procArgsFixture("/opt/example tool", []string{"tool"}, []string{"XPC_SERVICE_NAME=0"})},
		{name: "empty label", data: procArgsFixture("/opt/example tool", []string{"tool"}, []string{"XPC_SERVICE_NAME="})},
		{name: "missing environment", data: procArgsFixture("/opt/example tool", []string{"tool"}, nil), bad: true},
		{name: "duplicate label", data: procArgsFixture("/opt/example tool", []string{"tool"}, []string{"XPC_SERVICE_NAME=a", "XPC_SERVICE_NAME=b"}), bad: true},
		{name: "short header", data: []byte{1, 0}, bad: true},
		{name: "negative argc", data: invalidArgc(^uint32(0)), bad: true},
		{name: "zero argc", data: invalidArgc(0), bad: true},
		{name: "unterminated executable", data: append(binary.NativeEndian.AppendUint32(nil, 1), []byte("/tool")...), bad: true},
		{name: "truncated arguments", data: append(binary.NativeEndian.AppendUint32(nil, 2), []byte("/tool\x00\x00tool\x00")...), bad: true},
		{name: "unterminated environment", data: append(binary.NativeEndian.AppendUint32(nil, 1), []byte("/tool\x00\x00tool\x00PATH=/bin")...), bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			executable, label, err := parseProcArgs2(tc.data)
			if tc.bad {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "/opt/example tool", executable)
			assert.Equal(t, tc.label, label)
		})
	}
}

func TestParseLaunchctlService(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		pid        int
		bad        bool
	}{
		{name: "running", body: "\tstate = running\n\tprogram = /opt/example tool\n\tpid = 42\n", pid: 42},
		{name: "inactive", body: "\tstate = not running\n\tprogram = /opt/example tool\n"},
		{name: "nested ignored", body: "\tstate = waiting\n\tprogram = /opt/example tool\n\tenvironment = {\n\t\tpid = 99\n\t}\n"},
		{name: "missing state", body: "\tpid = 42\n\tprogram = /tool\n", bad: true},
		{name: "missing program", body: "\tstate = running\n\tpid = 42\n", bad: true},
		{name: "duplicate pid", body: "\tstate = running\n\tprogram = /tool\n\tpid = 42\n\tpid = 42\n", bad: true},
		{name: "duplicate program", body: "\tstate = running\n\tprogram = /tool\n\tprogram = /other\n", bad: true},
		{name: "duplicate state", body: "\tstate = running\n\tstate = waiting\n\tprogram = /tool\n", bad: true},
		{name: "negative pid", body: "\tstate = running\n\tprogram = /tool\n\tpid = -42\n", bad: true},
		{name: "pid junk", body: "\tstate = running\n\tprogram = /tool\n\tpid = 42oops\n", bad: true},
		{name: "pid spacing changed", body: "\tstate = running\n\tprogram = /tool\n\tpid=42\n", bad: true},
		{name: "empty output", bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pid, executable, err := parseLaunchctlService([]byte("fixture = {\n" + tc.body + "}\n"))
			if tc.bad {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.pid, pid)
			assert.Equal(t, "/opt/example tool", executable)
		})
	}
}

func FuzzParseProcArgs2(f *testing.F) {
	f.Add(procArgsFixture("/tool", []string{"tool", ""}, []string{"XPC_SERVICE_NAME=com.example.worker"}), "com.example.worker")
	f.Add([]byte{0}, "")
	f.Fuzz(func(t *testing.T, data []byte, label string) {
		_, _, _ = parseProcArgs2(data)
		if strings.ContainsRune(label, 0) || label == "0" {
			return
		}
		_, got, err := parseProcArgs2(procArgsFixture("/tool", []string{"tool", ""}, []string{"XPC_SERVICE_NAME=" + label}))
		require.NoError(t, err)
		assert.Equal(t, label, got)
	})
}

func FuzzParseLaunchctlService(f *testing.F) {
	f.Add([]byte("\tstate = running\n\tprogram = /tool\n\tpid = 42\n"))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _, _ = parseLaunchctlService(data)
		// Nest every arbitrary line: none may supply a top-level PID.
		body := "\tstate = waiting\n\tprogram = /tool\n\t\t" + strings.ReplaceAll(string(data), "\n", "\n\t\t")
		pid, _, err := parseLaunchctlService([]byte(body))
		require.NoError(t, err)
		assert.Zero(t, pid)
	})
}

func TestParseLaunchctlDomain(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		bad        bool
	}{
		{name: "services", body: "\tservices = {\n\t\t42 0 com.example.running\n\t\t0 - com.example.inactive\n\t}\n"},
		{name: "missing table", bad: true},
		{name: "unclosed table", body: "\tservices = {\n", bad: true},
		{name: "malformed row", body: "\tservices = {\n\t\t42 com.example.running\n\t}\n", bad: true},
		{name: "negative PID", body: "\tservices = {\n\t\t-42 0 com.example.running\n\t}\n", bad: true},
		{name: "duplicate label", body: "\tservices = {\n\t\t42 0 com.example.running\n\t\t43 0 com.example.running\n\t}\n", bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			services, err := parseLaunchctlDomain([]byte(tc.body))
			if tc.bad {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, map[string]int{"com.example.running": 42, "com.example.inactive": 0}, services)
		})
	}
}

func FuzzParseLaunchctlDomain(f *testing.F) {
	f.Add([]byte("\tservices = {\n\t\t42 0 com.example.worker\n\t}\n"))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		services, err := parseLaunchctlDomain(data)
		if err == nil {
			for label, pid := range services {
				assert.NotEmpty(t, label)
				assert.GreaterOrEqual(t, pid, 0)
			}
		}
	})
}
