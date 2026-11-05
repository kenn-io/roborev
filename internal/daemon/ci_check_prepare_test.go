package daemon

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
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
	miseTools, miseShims := ciCheckMiseFixture(t)
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
	require.NoError(t, os.Symlink(filepath.Join(miseShims, "mise"), command))
	var log bytes.Buffer
	run, err := wp.prepareCICheck(t.Context(), job, command, []string{"CHECKER_GIT_TOKEN=synthetic"}, &log)
	require.NoError(t, err)
	defer run.cleanup()
	var tmp string
	for _, entry := range run.Job.Env {
		if value, ok := strings.CutPrefix(entry, "TMPDIR="); ok {
			tmp = value
		}
	}
	require.NotEmpty(t, tmp)
	t.Cleanup(func() { assert.NoDirExists(t, filepath.Dir(tmp)) })
	require.NoError(t, os.WriteFile(filepath.Join(tmp, "temporary"), []byte("scratch"), 0o600))
	contents, err := os.ReadFile(filepath.Join(run.Job.WorkDir, "tmp", "temporary"))
	require.NoError(t, err)
	assert.Equal(t, "scratch", string(contents))
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
	home := filepath.Join(miseTools, "home")
	require.NoError(t, os.Mkdir(home, 0o700))
	t.Setenv("HOME", home)
	_, err = wp.prepareCICheck(t.Context(), job, command, nil, io.Discard)
	require.ErrorContains(t, err, "read grant contains host home")
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
		// Version-manager shims must resolve globally, outside temporary directory ancestry.
		tokenFile := filepath.Join(t.TempDir(), "download-token")
		script := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = env ]; then [ \"$(pwd -P)\" = '/' ] || exit 4; printf '%%s\\n' '%s' '%s'; else [ \"$1\" = mod ] && [ \"$2\" = download ] || exit 3; printf '%%s' \"$CHECKER_GIT_TOKEN\" > '%s'; fi\n", tools, cache, tokenFile)
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
	require.NoError(t, os.WriteFile(filepath.Join(bin, "codex"), []byte("\x7fELFnative fixture"), 0o700))
	selected := filepath.Join(shims, "selected")
	require.NoError(t, os.Symlink(tools, selected))
	inventory := fmt.Sprintf(`{"node":[{"install_path":%q}]}`, selected)
	script := fmt.Sprintf("#!/bin/sh\ncase \"$*\" in\n'which codex') [ \"$(pwd -P)\" = '/' ] || exit 4; printf '%%s\\n' '%s';;\n'ls --current --installed --json') printf '%%s' '%s';;\n'bin-paths') printf '%%s\\n' '%s';;\n*) exit 3;;\nesac\n", filepath.Join(bin, "codex"), inventory, filepath.Join(selected, "bin"))
	require.NoError(t, os.WriteFile(filepath.Join(shims, "mise"), []byte(script), 0o700))
	t.Setenv("PATH", shims+string(os.PathListSeparator)+os.Getenv("PATH"))
	return tools, shims
}

func TestCICheckMiseDiscovery(t *testing.T) {
	tools, shims := ciCheckMiseFixture(t)
	selected := filepath.Join(shims, "selected")
	missing := filepath.Join(tools, "installs", "python", "system")
	require.NoError(t, os.MkdirAll(filepath.Dir(missing), 0o700))
	// An installation can disappear after mise lists its executable directory.
	removedBin := filepath.Join(tools, "removed-bin")
	require.NoError(t, os.Mkdir(removedBin, 0o700))
	inventory := fmt.Sprintf(`{"node":[{"install_path":%q}]}`, selected)
	// mise reports system selections as installed even though this directory doesn't exist.
	mixedInventory := fmt.Sprintf(`{"python":[{"version":"system","install_path":%q,"installed":true,"active":true}],"node":[{"install_path":%q,"installed":true,"active":true}]}`, missing, selected)
	mixedScript := fmt.Sprintf(`#!/bin/sh
[ "$(pwd -P)" = '/' ] || { printf 'mise discovery ran from unexpected directory: %%s\n' "$(pwd -P)" >&2; exit 4; }
case "$*" in
'ls --current --installed --json') printf '%%s' '%s';;
'bin-paths') printf '%%s\n' '%s' '%s'; /bin/rmdir '%s';;
*) exit 3;;
esac
`, mixedInventory, removedBin, filepath.Join(selected, "bin"), removedBin)
	for _, tc := range []struct {
		name, script, message string
		roots, bins           []string
	}{
		{name: "absent"},
		{name: "malformed", script: "#!/bin/sh\nprintf 'invalid json'\n", message: "mise discovery failed"},
		{name: "inventory failure", script: "#!/bin/sh\nexit 3\n", message: "exit status 3"},
		{name: "bin paths failure", script: fmt.Sprintf("#!/bin/sh\n[ \"$1\" = ls ] && { printf '%%s' '%s'; exit 0; }; exit 4\n", inventory), message: "exit status 4"},
		{name: "runs from root and skips unavailable paths", script: mixedScript, message: missing, roots: []string{tools}, bins: []string{filepath.Join(tools, "bin")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			commands := t.TempDir()
			if tc.script != "" {
				require.NoError(t, os.WriteFile(filepath.Join(commands, "mise"), []byte(tc.script), 0o700))
			}
			t.Setenv("PATH", commands)
			var log bytes.Buffer
			roots, bins := ciCheckMisePaths(t.Context(), &log)
			assert.Equal(t, tc.roots, roots, log.String())
			assert.Equal(t, tc.bins, bins, log.String())
			if tc.message == "" {
				assert.Empty(t, log.String())
			} else {
				assert.Contains(t, log.String(), tc.message)
			}
		})
	}
}

// Preparation stages nested committed manifests, excludes repository settings,
// and hands packages and the root interpreter to the sandbox job.
func TestCICheckPackagesPreparation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires shell fixtures")
	}
	_, shims := ciCheckMiseFixture(t)
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	tools := t.TempDir()
	script := `#!/bin/sh
[ -z "$CHECKER_SECRET" ] && [ -z "$NODE_OPTIONS" ] || exit 6
[ ! -e .npmrc ] && [ ! -e setup.py ] && [ ! -e sitecustomize.py ] || exit 7
case "$*" in
*download*source-only*) exit 2;;
*download*) exit 0;;
*venv*)
 for arg do dest="$arg"; done
 /bin/mkdir -p "$dest/bin"
 /bin/cp "$0" "$dest/bin/python"
 ;;
*install*--find-links*)
 /bin/mkdir -p "${0%/bin/python}/lib"
 printf '%s\n' "$@" > "${0%/bin/python}/lib/installed"
 ;;
*)
 [ "$npm_config_ignore_scripts" = true ] || exit 8
 /bin/mkdir -p node_modules/test-package
 printf 'package ready' > node_modules/test-package/index.js
 ;;
esac
`
	for _, tool := range []string{"npm", "bun", "python3"} {
		require.NoError(t, os.WriteFile(filepath.Join(tools, tool), []byte(script), 0o700))
	}
	// Mise still resolves the fixture Codex, while installers use selected tools.
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CHECKER_SECRET", "synthetic")
	t.Setenv("NODE_OPTIONS", "--require=/untrusted/module")
	repo := testutil.NewTestRepoWithCommit(t)
	repo.CommitFile("web/package.json", `{"dependencies":{"test-package":"1"}}`, "javascript dependencies")
	repo.CommitFile("web/bun.lock", `{"packages":{"test-package":["test-package@1", "", {"bin":{"tool":"./bin/tool.mjs"}}]}}`, "bun executable metadata")
	repo.CommitFile("web/.npmrc", "onload-script=./untrusted.js", "repository settings")
	repo.CommitFile("sitecustomize.py", "raise RuntimeError('untrusted')", "repository module")
	repo.CommitFile("pyproject.toml", "[project]\ndependencies = ['test-package>=1', 'source-only', 'unsafe @ https://example.com/source.tar.gz']\n[project.optional-dependencies]\ntest = ['optional-fixture==2']\n[dependency-groups]\ndev = ['group-fixture; python_version >= \"3.10\"']\n", "python dependencies")
	sha := repo.HeadSHA()
	var log bytes.Buffer
	run, err := (&WorkerPool{}).prepareCICheck(t.Context(), &storage.ReviewJob{RepoPath: repo.Root, GitRef: sha + ".." + sha}, filepath.Join(shims, "selected", "bin", "codex"), nil, &log)
	require.NoError(t, err)
	root := filepath.Dir(run.Job.WorkDir)
	defer run.cleanup()
	t.Cleanup(func() { assert.NoDirExists(t, root) })
	installed, err := os.ReadFile(filepath.Join(run.Repo, "web", "node_modules", "test-package", "index.js"))
	require.NoError(t, err, log.String())
	assert.Equal(t, "package ready", string(installed))
	requirements, err := os.ReadFile(filepath.Join(run.Repo, ".venv", "lib", "installed"))
	require.NoError(t, err, log.String())
	assert.Contains(t, string(requirements), "test-package>=1")
	assert.Contains(t, string(requirements), "optional-fixture==2")
	assert.Contains(t, string(requirements), `group-fixture; python_version >= "3.10"`)
	assert.NotContains(t, string(requirements), "unsafe")
	assert.NotContains(t, string(requirements), "source-only")
	assert.Contains(t, log.String(), "package preparation failed", "a source-only requirement must leave the other wheels available")
	var path string
	for _, entry := range run.Job.Env {
		if value, ok := strings.CutPrefix(entry, "PATH="); ok {
			path = value
		}
	}
	assert.Equal(t, filepath.Join(run.Repo, ".venv", "bin"), strings.Split(path, string(os.PathListSeparator))[0])
	assert.NoDirExists(t, filepath.Join(repo.Root, "web", "node_modules"))
	assert.NoDirExists(t, filepath.Join(repo.Root, ".venv"))
}

func TestCICheckPackagesSkipped(t *testing.T) {
	for _, tc := range []struct{ name, manifest string }{
		{"local", `{"dependencies":{"local":"file:../local"}}`},
		{"relative", `{"devDependencies":{"local":"../local"}}`},
		{"override", `{"overrides":{"pkg":{"nested":"link:../local"}}}`},
		{"resolution", `{"resolutions":{"pkg/nested":"workspace:*"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := testutil.NewTestRepoWithCommit(t)
			repo.CommitFile("package.json", tc.manifest, "dependencies")
			var log bytes.Buffer
			assert.Empty(t, prepareCICheckPackages(t.Context(), repo.Root, t.TempDir(), os.Getenv("PATH"), nil, &log))
			assert.NoDirExists(t, filepath.Join(repo.Root, "node_modules"))
			assert.Contains(t, log.String(), "skips")
		})
	}
	t.Run("existing venv", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("requires shell fixtures")
		}
		repo := testutil.NewTestRepoWithCommit(t)
		repo.CommitFile("pyproject.toml", "[project]\ndependencies = ['fixture']\n", "dependency")
		repo.CommitFile(".venv/marker", "existing", "existing environment")
		tools := t.TempDir()
		marker := filepath.Join(tools, "python-ran")
		require.NoError(t, os.WriteFile(filepath.Join(tools, "python3"), []byte(fmt.Sprintf("#!/bin/sh\nprintf 'ran' > '%s'\n", marker)), 0o700))
		t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
		var log bytes.Buffer
		prepareCICheckPackages(t.Context(), repo.Root, t.TempDir(), os.Getenv("PATH"), nil, &log)
		assert.Contains(t, log.String(), "skips existing .venv")
		assert.NoFileExists(t, marker)
	})
	t.Run("unavailable", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("requires symlinks")
		}
		repo := testutil.NewTestRepoWithCommit(t)
		repo.CommitFile("package.json", `{"dependencies":{"test-package":"1"}}`, "dependencies")
		// Discovery uses the trusted Git command, then tool resolution sees no npm.
		commands := t.TempDir()
		git, err := exec.LookPath("git")
		require.NoError(t, err)
		require.NoError(t, os.Symlink(git, filepath.Join(commands, "git")))
		t.Setenv("PATH", commands)
		var log bytes.Buffer
		prepareCICheckPackages(t.Context(), repo.Root, t.TempDir(), commands, nil, &log)
		assert.Contains(t, log.String(), "package tool unavailable: npm")
	})
}

func TestCICheckPackagesGitAuthentication(t *testing.T) {
	env := ciCheckPackageEnv(t.TempDir(), "/trusted/bin", []string{"CHECKER_SECRET=secret", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.https://github.com/.extraheader", "GIT_CONFIG_VALUE_0=AUTHORIZATION: synthetic"})
	command := exec.CommandContext(t.Context(), "git", "config", "--get", "http.https://github.com/.extraheader")
	command.Env = env
	output, err := command.Output()
	require.NoError(t, err)
	assert.Equal(t, "AUTHORIZATION: synthetic\n", string(output))
	assert.NotContains(t, env, "CHECKER_SECRET=secret")
}
