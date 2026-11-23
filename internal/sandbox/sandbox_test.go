package sandbox

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

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
			if tc.fake {
				name := "nono"
				if runtime.GOOS == "windows" {
					name += ".exe"
				}
				require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nexit 0\n"), 0o700))
			}
			t.Setenv("PATH", dir)
			_, _, _, err := (Job{WorkDir: dir, MemoryMax: 1 << 30}).Command(t.Context(), []string{"true"})
			require.ErrorContains(t, err, tc.want)
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
		write            bool
	}{
		{"file", file, "inside sandbox WorkDir", false},
		{"workdir", work, "inside sandbox WorkDir", false},
		{"symlink", link, "inside sandbox WorkDir", false},
		{"symlink-parent", dirLink + "/../read-only", "inside sandbox WorkDir", false},
		{"relative-symlink-parent", "dir-link/../read-only", "inside sandbox WorkDir", false},
		{"missing", filepath.Join(t.TempDir(), "missing"), "resolve sandbox read path", false},
		{"missing write", filepath.Join(t.TempDir(), "missing"), "resolve sandbox write path", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if runtime.GOOS == "windows" && (tc.name == "symlink-parent" || tc.name == "relative-symlink-parent") {
				t.Skip("Windows resolves parent components before symlinks")
			}
			job := Job{WorkDir: work, MemoryMax: 1 << 30}
			if tc.write {
				job.WritePaths = []string{tc.path}
			} else {
				job.ReadPaths = []string{tc.path}
			}
			_, _, _, err := job.Command(t.Context(), []string{"true"})
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestSandboxSelfCheckRejectsInvalidInput(t *testing.T) {
	hostHome := filepath.Join(t.TempDir(), "codex")
	require.NoError(t, os.Mkdir(hostHome, 0o700))
	runtimeDir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	for _, tc := range []struct {
		name string
		job  Job
		env  []string
		want string
	}{
		{"directory", Job{DenyReadPaths: []string{hostHome}}, nil, "deny-read path must be a file"},
		{"missing", Job{}, nil, "sandbox self-check failed: XDG_RUNTIME_DIR"},
		{"empty-last-value", Job{}, []string{"XDG_RUNTIME_DIR=" + runtimeDir, "XDG_RUNTIME_DIR="}, "sandbox self-check failed: XDG_RUNTIME_DIR"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.job.check(t.Context(), nil, tc.env, nil)
			require.ErrorContains(t, err, tc.want)
		})
	}
}
