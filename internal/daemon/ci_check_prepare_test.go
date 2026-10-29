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

// Preparation runs without nono or Codex, and its caller releases both trees.
func captureCheckPreparation(t *testing.T, repo *testutil.TestRepo) string {
	t.Helper()
	tools := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(tools, "codex"), []byte("native fixture"), 0o700))
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
	sha := testutil.GetHeadSHA(t, repo.Root)
	wp := &WorkerPool{}
	copy, _, _, cleanup, err := wp.prepareCICheck(t.Context(), &storage.ReviewJob{RepoPath: repo.Root, GitRef: sha + ".." + sha}, nil, io.Discard)
	require.NoError(t, err)
	t.Cleanup(cleanup)
	return copy
}

func TestCICheckCommittedModuleInput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires shell fixtures")
	}
	repo := testutil.NewTestRepoWithCommit(t)
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
	original := "module example.com/check\n\ngo 1.24.0\n\nrequire example.com/dependency v1.0.0\n"
	repo.CommitFile("go.mod", original, "module")
	repo.CommitFile("go.sum", "", "checksums")
	observed := captureCheckPreparation(t, repo)
	mod, err := os.ReadFile(filepath.Join(observed, "go.mod"))
	require.NoError(t, err)
	assert.Equal(t, original, string(mod))
	sum, err := os.ReadFile(filepath.Join(observed, "go.sum"))
	require.NoError(t, err)
	assert.Empty(t, string(sum))
}

func TestCICheckRelativeSubmoduleInput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires shell fixtures")
	}
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
	observed := captureCheckPreparation(t, repo)
	content, err := os.ReadFile(filepath.Join(observed, "dependency", "present.txt"))
	require.NoError(t, err)
	assert.Equal(t, "submodule content", string(content))
}

func TestCICheckNativeLauncherRead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires native fixture")
	}
	repo := testutil.NewTestRepoWithCommit(t)
	tools := t.TempDir()
	nativeDir := filepath.Join(tools, "native")
	require.NoError(t, os.Mkdir(nativeDir, 0o700))
	f, err := os.Create(filepath.Join(nativeDir, "codex"))
	require.NoError(t, err)
	const size = 32 << 20
	require.NoError(t, f.Truncate(size))
	require.NoError(t, f.Chmod(0o700))
	require.NoError(t, f.Close())
	require.NoError(t, os.Symlink(filepath.Join(nativeDir, "codex"), filepath.Join(tools, "codex")))
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
	sha := testutil.GetHeadSHA(t, repo.Root)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	wp := &WorkerPool{}
	_, _, spec, cleanup, err := wp.prepareCICheck(t.Context(), &storage.ReviewJob{RepoPath: repo.Root, GitRef: sha + ".." + sha}, nil, io.Discard)
	runtime.ReadMemStats(&after)
	require.NoError(t, err)
	t.Cleanup(cleanup)
	assert.Contains(t, spec.ReadPaths, nativeDir, "native Codex needs its sibling helpers")
	assert.NotContains(t, spec.ReadPaths, tools)
	assert.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(size), "launcher probe must read only its header")
}

func TestCICheckChecksumSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires symlinks")
	}
	repo := testutil.NewTestRepoWithCommit(t)
	repo.CommitFile("go.mod", "module example.com/check\n\ngo 1.24.0\n", "module")
	hostPath := filepath.Join(t.TempDir(), "host-checksums")
	require.NoError(t, os.Symlink(hostPath, filepath.Join(repo.Root, "go.sum")))
	repo.RunGit("add", "go.sum")
	repo.RunGit("commit", "-m", "checksum link")
	copy := captureCheckPreparation(t, repo)
	target, err := os.Readlink(filepath.Join(copy, "go.sum"))
	require.NoError(t, err)
	assert.Equal(t, hostPath, target)
	assert.NoFileExists(t, hostPath)
}

func TestCICheckWarmingFailureKeepsGo(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires shell fixtures")
	}
	checkout, root, tools := t.TempDir(), t.TempDir(), t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(checkout, "go.mod"), []byte("module example.com/check\n"), 0o600))
	require.NoError(t, os.Mkdir(filepath.Join(tools, "bin"), 0o700))
	script := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = env ]; then printf '%%s\\n' '%s' '%s'; else [ \"$CHECKER_GIT_TOKEN\" = synthetic ] || exit 3; echo attempted > warmed; exit 1; fi\n", tools, tools)
	require.NoError(t, os.WriteFile(filepath.Join(tools, "bin", "go"), []byte(script), 0o700))
	require.NoError(t, os.Symlink(filepath.Join("bin", "go"), filepath.Join(tools, "go")))
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
	var log bytes.Buffer
	paths, err := warmGoModules(t.Context(), checkout, root, append(os.Environ(), "CHECKER_GIT_TOKEN=synthetic"), &log)
	require.NoError(t, err)
	assert.Equal(t, []string{tools, tools}, paths)
	assert.FileExists(t, filepath.Join(checkout, "warmed"))
	assert.Contains(t, log.String(), "module warming failed")
}

func TestCICheckWarmingWithoutGo(t *testing.T) {
	checkout, root := t.TempDir(), t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(checkout, "go.mod"), []byte("module example.com/check\n"), 0o600))
	t.Setenv("PATH", t.TempDir())
	var log bytes.Buffer
	paths, err := warmGoModules(t.Context(), checkout, root, nil, &log)
	require.NoError(t, err)
	assert.Empty(t, paths)
	assert.Contains(t, log.String(), "go env failed")
}
