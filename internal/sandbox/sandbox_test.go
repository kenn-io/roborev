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
	for _, tc := range []struct {
		name string
		fake bool
		want string
	}{
		{"missing", false, "find sandbox nono"},
		{"checksum", true, "checksum mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			marker := filepath.Join(dir, "executed")
			if tc.fake {
				name := "nono"
				if runtime.GOOS == "windows" {
					name += ".exe"
				}
				require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n: >'"+marker+"'\n"), 0o700))
			}
			t.Setenv("PATH", dir)
			_, _, _, err := (Job{WorkDir: dir, MemoryMax: 1 << 30}).Command(context.Background(), []string{"true"})
			require.ErrorContains(t, err, tc.want)
			if tc.fake {
				assert.NoFileExists(t, marker)
			}
		})
	}
}

func TestSandboxCommandRejectsReadPaths(t *testing.T) {
	work := t.TempDir()
	file := filepath.Join(work, "read-only")
	require.NoError(t, os.WriteFile(file, []byte("fixture"), 0o600))
	link := filepath.Join(t.TempDir(), "read-link")
	require.NoError(t, os.Symlink(file, link))
	child := filepath.Join(work, "child")
	require.NoError(t, os.Mkdir(child, 0o700))
	dirLink := filepath.Join(filepath.Dir(link), "dir-link")
	require.NoError(t, os.Symlink(child, dirLink))
	t.Chdir(filepath.Dir(link))
	for _, tc := range []struct {
		name, path, want string
	}{
		{"file", file, "inside sandbox WorkDir"},
		{"workdir", work, "inside sandbox WorkDir"},
		{"symlink", link, "inside sandbox WorkDir"},
		{"symlink-parent", dirLink + "/../read-only", "inside sandbox WorkDir"},
		{"relative-symlink-parent", "dir-link/../read-only", "inside sandbox WorkDir"},
		{"missing", filepath.Join(t.TempDir(), "missing"), "resolve sandbox read path"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, err := (Job{WorkDir: work, ReadPaths: []string{tc.path}, MemoryMax: 1 << 30}).Command(t.Context(), []string{"true"})
			require.ErrorContains(t, err, tc.want)
		})
	}
}
