package testenv

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func appendToFile(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	require.NoError(t, err, "failed to open file")
	defer f.Close()
	_, err = fmt.Fprintln(f, content)
	require.NoError(t, err, "failed to write to file")
}

func TestProdLogBarrierViolations(t *testing.T) {
	tests := []struct {
		name               string
		setup              func(t *testing.T, dir string)
		mutate             func(t *testing.T, dir string)
		wantViolations     []string
		wantContains       []string
		wantViolationCount int
	}{
		{
			name: "detects activity test event pollution",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				logPath := filepath.Join(dir, "activity.log")
				err := os.WriteFile(logPath, []byte(
					`{"event":"job.completed","component":"worker"}`+"\n",
				), 0o644)
				require.NoError(t, err, "failed to write initial file")
			},
			mutate: func(t *testing.T, dir string) {
				t.Helper()
				appendToFile(t, filepath.Join(dir, "activity.log"),
					`{"event":"test","component":"test","message":"msg"}`)
			},
			wantViolations: []string{
				`test pollution in prod activity.log: event:"test" entry`,
			},
		},
		{
			name: "detects dev daemon start",
			mutate: func(t *testing.T, dir string) {
				t.Helper()
				appendToFile(t, filepath.Join(dir, "activity.log"),
					`{"event":"daemon.started","details":{"version":"dev","pid":"999"}}`)
			},
			wantViolations: []string{
				`test pollution in prod activity.log: daemon.started with version:"dev"`,
			},
		},
		{
			name: "detects daemon runtime file creation",
			mutate: func(t *testing.T, dir string) {
				t.Helper()
				runtimePath := filepath.Join(dir,
					fmt.Sprintf("daemon.%d.json", os.Getpid()))
				err := os.WriteFile(runtimePath, []byte("{}"), 0o644)
				require.NoError(t, err, "failed to write runtime file")
			},
			wantViolations: []string{
				fmt.Sprintf("test wrote daemon.%d.json to prod data dir", os.Getpid()),
			},
		},
		{
			name: "detects errors log pollution",
			mutate: func(t *testing.T, dir string) {
				t.Helper()
				appendToFile(t, filepath.Join(dir, "errors.log"),
					`{"event":"test","component":"test","message":"err"}`)
			},
			wantViolations: []string{
				`test pollution in prod errors.log: event:"test" entry`,
			},
		},
		{
			name: "ignores pre-existing runtime file",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				runtimePath := filepath.Join(dir,
					fmt.Sprintf("daemon.%d.json", os.Getpid()))
				err := os.WriteFile(runtimePath, []byte("{}"), 0o644)
				require.NoError(t, err, "failed to write initial runtime file")
			},
		},
		{
			name: "detects pre-existing runtime mutation",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				runtimePath := filepath.Join(dir,
					fmt.Sprintf("daemon.%d.json", os.Getpid()))
				err := os.WriteFile(runtimePath, []byte("{}"), 0o644)
				require.NoError(t, err, "failed to write initial runtime file")
			},
			mutate: func(t *testing.T, dir string) {
				t.Helper()
				runtimePath := filepath.Join(dir,
					fmt.Sprintf("daemon.%d.json", os.Getpid()))
				err := os.WriteFile(runtimePath, []byte(`{"pid":999}`), 0o644)
				require.NoError(t, err, "failed to modify runtime file")
			},
			wantViolationCount: 1,
			wantContains: []string{
				"test modified daemon.",
				"size 2→11",
				"mtime ",
			},
		},
		{
			name: "passes when clean",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				logPath := filepath.Join(dir, "activity.log")
				err := os.WriteFile(logPath, []byte(
					`{"event":"job.completed","component":"worker"}`+"\n",
				), 0o644)
				require.NoError(t, err, "failed to write initial file")
			},
			mutate: func(t *testing.T, dir string) {
				t.Helper()
				appendToFile(t, filepath.Join(dir, "activity.log"),
					`{"event":"job.completed","component":"worker","details":{"job_id":"6638"}}`)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if tt.setup != nil {
				tt.setup(t, dir)
			}

			barrier := NewProdLogBarrier(dir)

			if tt.mutate != nil {
				tt.mutate(t, dir)
			}

			violations := barrier.Violations()

			if tt.wantViolations != nil {
				assert.Equal(t, tt.wantViolations, violations)
			}
			if tt.wantViolationCount > 0 {
				require.Len(t, violations, tt.wantViolationCount)
			}
			for _, want := range tt.wantContains {
				require.NotEmpty(t, violations, "expected at least one violation")
				assert.Contains(t, violations[0], want)
			}
			if tt.wantViolations == nil && tt.wantViolationCount == 0 {
				assert.Empty(t, violations)
			}
		})
	}
}

func TestProdLogBarrierCheckFormatsViolations(t *testing.T) {
	dir := t.TempDir()
	appendToFile(t, filepath.Join(dir, "activity.log"),
		`{"event":"test","component":"test","message":"msg"}`)

	barrier := NewProdLogBarrier(dir)
	appendToFile(t, filepath.Join(dir, "errors.log"),
		`{"event":"test","component":"test","message":"err"}`)

	violations := barrier.Violations()
	require.Equal(t, []string{
		`test pollution in prod errors.log: event:"test" entry`,
	}, violations)

	assert.Equal(t,
		"PROD LOG BARRIER FAILED:\n  "+strings.Join(violations, "\n  "),
		barrier.Check(),
	)
}
