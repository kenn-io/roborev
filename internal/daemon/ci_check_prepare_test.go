package daemon

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

// Preparation runs without nono or Codex. The sandbox copy must hold the
// committed tree: submodule files present, and go.mod and the go.sum link
// unchanged by warming, which must not write through the link to the host.
func TestCICheckPreparation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires shell fixtures and symlinks")
	}
	proxy := t.TempDir()
	versionDir := filepath.Join(proxy, "example.com", "dependency", "@v")
	require.NoError(t, os.MkdirAll(versionDir, 0o700))
	depMod := "module example.com/dependency\n\ngo 1.25.0\n"
	require.NoError(t, os.WriteFile(filepath.Join(versionDir, "v1.0.0.mod"), []byte(depMod), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(versionDir, "v1.0.0.info"), []byte(`{"Version":"v1.0.0","Time":"2025-01-01T00:00:00Z"}`), 0o600))
	var data bytes.Buffer
	z := zip.NewWriter(&data)
	f, err := z.Create("example.com/dependency@v1.0.0/go.mod")
	require.NoError(t, err)
	_, err = io.WriteString(f, depMod)
	require.NoError(t, err)
	require.NoError(t, z.Close())
	require.NoError(t, os.WriteFile(filepath.Join(versionDir, "v1.0.0.zip"), data.Bytes(), 0o600))
	t.Setenv("GOPROXY", "file://"+proxy)
	t.Setenv("GOSUMDB", "off")
	t.Setenv("GOWORK", "off")
	t.Setenv("GOMODCACHE", t.TempDir())
	t.Setenv("GOFLAGS", "-modcacherw")

	sub := testutil.NewTestRepoWithCommit(t)
	sub.CommitFile("present.txt", "submodule content", "content")
	repo := testutil.NewTestRepoWithCommit(t)
	// The upstream and its sibling exist, while no sibling exists next to the checker copy.
	remoteDir := t.TempDir()
	subPath := filepath.Join(remoteDir, "shared.git")
	require.NoError(t, os.Rename(sub.Root, subPath))
	repo.RunGit("remote", "add", "origin", filepath.Join(remoteDir, "upstream.git"))
	repo.RunGit("-c", "protocol.file.allow=always", "submodule", "add", subPath, "dependency")
	repo.RunGit("config", "-f", ".gitmodules", "submodule.dependency.url", "../shared.git")
	repo.RunGit("commit", "-am", "relative submodule")
	original := "module example.com/check\n\ngo 1.24.0\n\nrequire example.com/dependency v1.0.0\n"
	repo.CommitFile("go.mod", original, "module")
	hostPath := filepath.Join(t.TempDir(), "host-checksums")
	require.NoError(t, os.Symlink(hostPath, filepath.Join(repo.Root, "go.sum")))
	repo.RunGit("add", "go.sum")
	repo.RunGit("commit", "-m", "checksum link")

	tools := t.TempDir()
	nativeDir := filepath.Join(tools, "native")
	require.NoError(t, os.Mkdir(nativeDir, 0o700))
	native, err := os.Create(filepath.Join(nativeDir, "codex"))
	require.NoError(t, err)
	const size = 32 << 20
	require.NoError(t, native.Truncate(size))
	require.NoError(t, native.Chmod(0o700))
	require.NoError(t, native.Close())
	require.NoError(t, os.Symlink(filepath.Join(nativeDir, "codex"), filepath.Join(tools, "codex")))
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))

	sha := testutil.GetHeadSHA(t, repo.Root)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	wp := &WorkerPool{}
	copy, _, spec, cleanup, err := wp.prepareCICheck(t.Context(), &storage.ReviewJob{RepoPath: repo.Root, GitRef: sha + ".." + sha}, nil, io.Discard)
	runtime.ReadMemStats(&after)
	require.NoError(t, err)
	t.Cleanup(cleanup)
	assert.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(size), "launcher probe must read only its header")
	assert.Contains(t, spec.ReadPaths, nativeDir, "native Codex needs its sibling helpers")
	assert.NotContains(t, spec.ReadPaths, tools)
	content, err := os.ReadFile(filepath.Join(copy, "dependency", "present.txt"))
	require.NoError(t, err)
	assert.Equal(t, "submodule content", string(content))
	mod, err := os.ReadFile(filepath.Join(copy, "go.mod"))
	require.NoError(t, err)
	assert.Equal(t, original, string(mod))
	target, err := os.Readlink(filepath.Join(copy, "go.sum"))
	require.NoError(t, err)
	assert.Equal(t, hostPath, target)
	assert.NoFileExists(t, hostPath)
}

func TestCICheckWarming(t *testing.T) {
	checkout := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(checkout, "go.mod"), []byte("module example.com/check\n"), 0o600))
	t.Run("without go", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		var log bytes.Buffer
		paths, err := warmGoModules(t.Context(), checkout, t.TempDir(), nil, &log)
		require.NoError(t, err)
		assert.Empty(t, paths)
		assert.Contains(t, log.String(), "go env failed")
	})
	t.Run("download fails", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("requires shell fixtures")
		}
		tools := t.TempDir()
		require.NoError(t, os.Mkdir(filepath.Join(tools, "bin"), 0o700))
		script := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = env ]; then printf '%%s\\n' '%s' '%s'; else [ \"$CHECKER_GIT_TOKEN\" = synthetic ] || exit 3; echo attempted > warmed; exit 1; fi\n", tools, tools)
		require.NoError(t, os.WriteFile(filepath.Join(tools, "bin", "go"), []byte(script), 0o700))
		require.NoError(t, os.Symlink(filepath.Join("bin", "go"), filepath.Join(tools, "go")))
		t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
		var log bytes.Buffer
		paths, err := warmGoModules(t.Context(), checkout, t.TempDir(), append(os.Environ(), "CHECKER_GIT_TOKEN=synthetic"), &log)
		require.NoError(t, err)
		assert.Equal(t, []string{tools, tools}, paths)
		assert.FileExists(t, filepath.Join(checkout, "warmed"))
		assert.Contains(t, log.String(), "module warming failed")
	})
}
