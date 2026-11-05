package main

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseLaunchctlService(t *testing.T) {
	raw, err := os.ReadFile("testdata/launchctl-print-wrapper.txt")
	require.NoError(t, err)
	service, jobType, err := parseLaunchctlService(raw)
	require.NoError(t, err)
	check := assert.New(t)
	check.Equal("LaunchAgent", jobType)
	check.Equal("/bin/sh", service.executable)
	check.Equal(16118, service.mainPID)
	check.Equal("simple", service.serviceType)
	for _, tc := range []struct {
		name, output string
		wantPID      int
		wantErr      bool
	}{
		{name: "missing pid", output: "\ttype = LaunchAgent\n\tprogram = /example/roborev\n"},
		{name: "nested values", output: "\ttype = LaunchAgent\n\tpid = 42\n\tenvironment = {\n\t\ttype = Application\n\t\tpid = 99\n\t}\n", wantPID: 42},
		{name: "missing type", output: "\tprogram = /example/roborev\n\tpid = 42\n", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service, _, err := parseLaunchctlService([]byte(tc.output))
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantPID, service.mainPID)
		})
	}
}

func TestParseLaunchctlDomain(t *testing.T) {
	for _, tc := range []struct {
		name, output string
		want         map[int]string
		wantErr      bool
	}{
		{name: "running jobs", output: "domain = {\n\tservices = {\n\t\t42 0 com.example.daemon\n\t\t43 -15 application.example.daemon\n\t}\n}\n", want: map[int]string{42: "com.example.daemon", 43: "application.example.daemon"}},
		{name: "empty", output: "\tservices = {\n\t}\n", want: map[int]string{}},
		{name: "inactive", output: "\tservices = {\n\t\t0 0 com.example.daemon\n\t}\n", want: map[int]string{}},
		{name: "malformed row", output: "\tservices = {\n\t\tinvalid row\n\t}\n", wantErr: true},
		{name: "extra column", output: "\tservices = {\n\t\t42 0 com.example.daemon extra\n\t}\n", wantErr: true},
		{name: "invalid pid", output: "\tservices = {\n\t\tinvalid 0 com.example.daemon\n\t}\n", wantErr: true},
		{name: "negative pid", output: "\tservices = {\n\t\t-42 0 com.example.daemon\n\t}\n", wantErr: true},
		{name: "signed pid", output: "\tservices = {\n\t\t+42 0 com.example.daemon\n\t}\n", wantErr: true},
		{name: "missing", output: "domain = {}", wantErr: true},
		{name: "unclosed", output: "\tservices = {\n\t\t42 0 example\n", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			jobs, err := parseLaunchctlDomain([]byte(tc.output))
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, jobs)
		})
	}
}
