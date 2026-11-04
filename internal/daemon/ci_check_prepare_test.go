package daemon

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

// Preparation warms modules in a separate checkout.
func TestCICheckPreparation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires shell fixtures and symlinks")
	}
	miseTools, _ := ciCheckMiseFixture(t)
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	repo := testutil.NewTestRepoWithCommit(t)
	repo.CommitFile("go.mod", "module example.com/check\n\ngo 1.25.0\n", "module")
	marker := filepath.Join(t.TempDir(), "hook-ran")
	repo.WriteFile(".githooks/post-checkout", fmt.Sprintf("#!/bin/sh\nprintf 'hook ran' > '%s'\n", marker))
	require.NoError(t, os.Chmod(filepath.Join(repo.Root, ".githooks", "post-checkout"), 0o700))
	repo.RunGit("add", ".githooks/post-checkout")
	repo.RunGit("commit", "-m", "checkout hook fixture")
	globalConfig := filepath.Join(t.TempDir(), "gitconfig")
	require.NoError(t, os.WriteFile(globalConfig, []byte("[core]\n\thooksPath = .githooks\n"), 0o600))
	t.Setenv("GIT_CONFIG_GLOBAL", globalConfig)
	tools := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(tools, "bin"), 0o700))
	moduleCache := t.TempDir()
	script := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = env ]; then printf '%%s\\n' '%s' '%s'; else echo 'require example.com/upgraded v1.0.0' >> go.mod; exit 1; fi\n", tools, moduleCache)
	require.NoError(t, os.WriteFile(filepath.Join(tools, "bin", "go"), []byte(script), 0o700))
	require.NoError(t, os.Symlink(filepath.Join("bin", "go"), filepath.Join(tools, "go")))
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
	sha := repo.HeadSHA()
	job := &storage.ReviewJob{RepoPath: repo.Root, GitRef: sha + ".." + sha}
	wp := &WorkerPool{}
	command := filepath.Join(t.TempDir(), "codex")
	require.NoError(t, os.WriteFile(command, []byte("\x7fELFnative fixture"), 0o700))
	var log bytes.Buffer
	run, err := wp.prepareCICheck(t.Context(), job, command, []string{"CHECKER_GIT_TOKEN=synthetic"}, &log)
	require.NoError(t, err)
	t.Cleanup(run.cleanup)
	assert.NoFileExists(t, marker, "checker preparation must leave repository hooks unexecuted on the host")
	assert.Contains(t, log.String(), "module warming failed: exit status 1")
	assert.Contains(t, run.Job.Env, "GOMODCACHE="+moduleCache, log.String())
	assert.Contains(t, run.Job.ReadPaths, tools)
	assert.Contains(t, run.Job.ReadPaths, moduleCache)
	assert.Contains(t, run.Job.ReadPaths, miseTools)
	assert.Contains(t, run.Job.Env, "PATH="+filepath.Join(tools, "bin")+string(os.PathListSeparator)+filepath.Join(miseTools, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
	module, err := os.ReadFile(filepath.Join(run.Repo, "go.mod"))
	require.NoError(t, err)
	assert.Equal(t, "module example.com/check\n\ngo 1.25.0\n", string(module))
}

// Recovery restores write access inside checker roots without following symlinks.
func TestCICheckRecovery(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires symlinks and Unix directory permissions")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	repo := testutil.NewTestRepoWithCommit(t)
	sha := repo.HeadSHA()
	wp := &WorkerPool{}
	checkout, closeCheckout, err := wp.createExactCheckout(t.Context(), "ci-checker", &storage.ReviewJob{RepoPath: repo.Root, GitRef: sha + ".." + sha})
	require.NoError(t, err)
	t.Cleanup(ciCheckCleanup(checkout, closeCheckout))
	root, err := os.MkdirTemp(checkout, "roborev-ci-check-")
	require.NoError(t, err)
	workDir := filepath.Join(root, "work")
	require.NoError(t, os.MkdirAll(filepath.Join(workDir, "gocache"), 0o700))
	dir := filepath.Join(workDir, "gocache", "readonly")
	require.NoError(t, os.Mkdir(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "file"), []byte("content"), 0o600))
	require.NoError(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	external := t.TempDir()
	require.NoError(t, os.Chmod(external, 0o500))
	t.Cleanup(func() { _ = os.Chmod(external, 0o700) })
	require.NoError(t, os.Symlink(external, filepath.Join(root, "external")))
	require.NoError(t, cleanupStaleCIWorktrees(t.Context()))
	assert.NoDirExists(t, root)
	assert.NoDirExists(t, checkout)
	info, err := os.Stat(external)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o500), info.Mode().Perm())
}

// Host-side warming refuses metadata that isn't a regular file, skips hosts without Go, and creates a missing module cache.
func TestCICheckWarming(t *testing.T) {
	metadata := []string{"go.mod", "go.sum", "go.work", "go.work.sum"}
	newCheckout := func(t *testing.T) string {
		checkout := t.TempDir()
		for _, name := range metadata {
			require.NoError(t, os.WriteFile(filepath.Join(checkout, name), []byte("go 1.26.0\n"), 0o600))
		}
		return checkout
	}
	for _, name := range metadata {
		t.Run(name+"/symlink", func(t *testing.T) {
			if runtime.GOOS == "windows" {
				t.Skip("requires symlinks")
			}
			path := filepath.Join(newCheckout(t), name)
			require.NoError(t, os.Remove(path))
			outside := filepath.Join(t.TempDir(), "metadata")
			require.NoError(t, os.WriteFile(outside, nil, 0o600))
			require.NoError(t, os.Symlink(outside, path))
			_, err := warmGoModules(t.Context(), filepath.Dir(path), nil, io.Discard)
			require.ErrorContains(t, err, name+" must be a regular file")
		})
	}
	t.Run("without go", func(t *testing.T) {
		checkout := newCheckout(t)
		t.Setenv("PATH", t.TempDir())
		var log bytes.Buffer
		paths, err := warmGoModules(t.Context(), checkout, nil, &log)
		require.NoError(t, err)
		assert.Empty(t, paths)
		assert.Contains(t, log.String(), "go env failed")
	})
	t.Run("without dependencies", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("requires shell fixtures")
		}
		cache := filepath.Join(t.TempDir(), "missing", "mod")
		tools := t.TempDir()
		require.NoError(t, os.Mkdir(filepath.Join(tools, "bin"), 0o700))
		// Version-manager shims must resolve outside the checkout.
		outside, err := filepath.EvalSymlinks(os.TempDir())
		require.NoError(t, err)
		tokenFile := filepath.Join(t.TempDir(), "download-token")
		script := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = env ]; then [ \"$(pwd -P)\" = '%s' ] || exit 4; printf '%%s\\n' '%s' '%s'; else [ \"$1\" = mod ] && [ \"$2\" = download ] || exit 3; printf '%%s' \"$CHECKER_GIT_TOKEN\" > '%s'; fi\n", outside, tools, cache, tokenFile)
		require.NoError(t, os.WriteFile(filepath.Join(tools, "bin", "go"), []byte(script), 0o700))
		t.Setenv("PATH", filepath.Join(tools, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
		checkout := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(checkout, "go.mod"), []byte("module example.com/check\n\ngo 1.25.0\n"), 0o600))
		var log bytes.Buffer
		paths, err := warmGoModules(t.Context(), checkout, []string{"CHECKER_GIT_TOKEN=synthetic"}, &log)
		require.NoError(t, err)
		require.Len(t, paths, 2, log.String())
		assert.Empty(t, log.String())
		assert.Equal(t, tools, paths[0])
		assert.Equal(t, cache, paths[1])
		assert.DirExists(t, paths[1], "the sandbox must be able to grant the module cache")
		token, err := os.ReadFile(tokenFile)
		require.NoError(t, err)
		assert.Equal(t, "synthetic", string(token))
	})
}

func TestCICheckCredentialOverlap(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("os.UserHomeDir reads USERPROFILE on Windows")
	}
	tools := t.TempDir()
	home := filepath.Join(tools, "home")
	require.NoError(t, os.Mkdir(home, 0o700))
	t.Setenv("HOME", home)
	require.ErrorContains(t, validateCICheckReadPaths([]string{tools}), "read grant contains host")
}

// The inherited shims fail under a disposable HOME; discovery must select the real tools.
func ciCheckMiseFixture(t *testing.T) (string, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("requires shell fixtures")
	}
	tools, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	shims := t.TempDir()
	bin := filepath.Join(tools, "bin")
	require.NoError(t, os.Mkdir(bin, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(tools, "runtime.txt"), []byte("runtime ready\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(shims, "node"), []byte("#!/bin/sh\nexit 126\n"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(bin, "node"), []byte("#!/bin/sh\ncat \"${0%/*}/../runtime.txt\"\n"), 0o700))
	outside, err := filepath.EvalSymlinks(os.TempDir())
	require.NoError(t, err)
	selected := filepath.Join(t.TempDir(), "selected")
	require.NoError(t, os.Symlink(tools, selected))
	inventory := fmt.Sprintf(`{"node":[{"install_path":%q}]}`, selected)
	script := fmt.Sprintf("#!/bin/sh\n[ \"$(pwd -P)\" = '%s' ] || exit 4\ncase \"$*\" in\n'ls --current --installed --json') printf '%%s' '%s';;\n'bin-paths') printf '%%s\\n' '%s';;\n*) exit 3;;\nesac\n", outside, inventory, filepath.Join(selected, "bin"))
	require.NoError(t, os.WriteFile(filepath.Join(shims, "mise"), []byte(script), 0o700))
	t.Setenv("PATH", shims+string(os.PathListSeparator)+os.Getenv("PATH"))
	return tools, shims
}

func prepareCICheckToolchainFixture(t *testing.T) (ciCheckRun, error) {
	t.Helper()
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	repo := testutil.NewTestRepoWithCommit(t)
	sha := repo.HeadSHA()
	command := filepath.Join(t.TempDir(), "codex")
	require.NoError(t, os.WriteFile(command, []byte("#!/bin/sh\nexit 0\n"), 0o700))
	run, err := (&WorkerPool{}).prepareCICheck(t.Context(), &storage.ReviewJob{RepoPath: repo.Root, GitRef: sha + ".." + sha}, command, nil, io.Discard)
	if err == nil {
		t.Cleanup(run.cleanup)
	}
	return run, err
}

func TestCICheckMiseDiscoveryFailure(t *testing.T) {
	for _, tc := range []struct {
		name, script, message string
	}{
		{"absent", "", ""},
		{"malformed", "#!/bin/sh\nprintf 'invalid json'\n", "mise discovery failed"},
		{"inventory failure", "#!/bin/sh\nexit 3\n", "exit status 3"},
		{"bin paths failure", "#!/bin/sh\n[ \"$1\" = ls ] && { printf '{}'; exit 0; }; exit 4\n", "exit status 4"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if runtime.GOOS == "windows" {
				t.Skip("requires shell fixtures")
			}
			tools := t.TempDir()
			if tc.script != "" {
				require.NoError(t, os.WriteFile(filepath.Join(tools, "mise"), []byte(tc.script), 0o700))
			}
			t.Setenv("PATH", tools)
			var log bytes.Buffer
			roots, bins := ciCheckMisePaths(t.Context(), &log)
			assert.Empty(t, roots)
			assert.Empty(t, bins)
			if tc.message == "" {
				assert.Empty(t, log.String())
			} else {
				assert.Contains(t, log.String(), tc.message)
			}
		})
	}
}

func TestCICheckMiseCredentialOverlap(t *testing.T) {
	tools, _ := ciCheckMiseFixture(t)
	home := filepath.Join(tools, "home")
	require.NoError(t, os.Mkdir(home, 0o700))
	t.Setenv("HOME", home)
	_, err := prepareCICheckToolchainFixture(t)
	require.ErrorContains(t, err, "read grant contains host home")
}

func TestCICheckMiseUnavailablePaths(t *testing.T) {
	tools, shims := ciCheckMiseFixture(t)
	missing := filepath.Join(tools, "installs", "python", "system")
	require.NoError(t, os.MkdirAll(filepath.Dir(missing), 0o700))
	// mise reports system selections as installed even though this directory doesn't exist.
	mise := filepath.Join(shims, "mise")
	script, err := os.ReadFile(mise)
	require.NoError(t, err)
	response := strings.Replace(string(script), `{"node":`, fmt.Sprintf(`{"python":[{"version":"system","install_path":%q,"installed":true,"active":true}],"node":`, missing), 1)
	response = strings.Replace(response, "'bin-paths') ", fmt.Sprintf("'bin-paths') printf '%%s\\n' '%s'; ", missing), 1)
	require.NoError(t, os.WriteFile(mise, []byte(response), 0o700))
	var log bytes.Buffer
	roots, bins := ciCheckMisePaths(t.Context(), &log)
	assert.Equal(t, []string{tools}, roots)
	assert.Equal(t, []string{filepath.Join(tools, "bin")}, bins)
	assert.Contains(t, log.String(), missing)
}
