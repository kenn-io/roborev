package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSandboxCommandRejectsUnavailablePolicy(t *testing.T) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("nono pin requires linux/amd64")
	}
	for _, tc := range []struct {
		name   string
		memory int64
		fake   bool
		want   string
	}{
		{"missing", 1 << 30, false, "find sandbox nono"},
		{"checksum", 1 << 30, true, "checksum mismatch"},
		{"memory", 0, false, "MemoryMax must be positive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			marker := filepath.Join(dir, "executed")
			if tc.fake {
				require.NoError(t, os.WriteFile(filepath.Join(dir, "nono"), []byte("#!/bin/sh\n: >'"+marker+"'\n"), 0o700))
			}
			t.Setenv("PATH", dir)
			_, _, _, err := (Job{WorkDir: dir, MemoryMax: tc.memory}).Command(context.Background(), []string{"true"})
			require.ErrorContains(t, err, tc.want)
			if tc.fake {
				assert.NoFileExists(t, marker)
			}
		})
	}
}

func TestSandboxCommandRejectsReadPaths(t *testing.T) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("nono pin requires linux/amd64")
	}
	work := t.TempDir()
	file := filepath.Join(work, "read-only")
	require.NoError(t, os.WriteFile(file, []byte("fixture"), 0o600))
	link := filepath.Join(t.TempDir(), "read-link")
	require.NoError(t, os.Symlink(file, link))
	for _, tc := range []struct {
		name, path, want string
	}{
		{"file", file, "inside sandbox WorkDir"},
		{"workdir", work, "inside sandbox WorkDir"},
		{"symlink", link, "inside sandbox WorkDir"},
		{"missing", filepath.Join(t.TempDir(), "missing"), "resolve sandbox read path"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, err := (Job{WorkDir: work, ReadPaths: []string{tc.path}, MemoryMax: 1 << 30}).Command(t.Context(), []string{"true"})
			require.ErrorContains(t, err, tc.want)
		})
	}
}
