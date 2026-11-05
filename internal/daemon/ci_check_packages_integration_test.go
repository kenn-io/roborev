//go:build integration && !windows

package daemon

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/sandbox"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

// Real installers consume controlled archives, then the real sandbox imports
// them. Lifecycle and interpreter startup sentinels distinguish host execution.
func TestCICheckPackagesSandboxIntegration(t *testing.T) {
	testutil.RequireSandboxIntegration(t)
	for _, name := range []string{"npm", "python3"} {
		_, err := exec.LookPath(name)
		require.NoError(t, err)
	}
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	python, err := resolveCICheckCommand(t.Context(), "/", "python3")
	require.NoError(t, err)
	runtime := t.TempDir()
	copyRuntime := exec.CommandContext(t.Context(), "/bin/cp", "-a", filepath.Dir(filepath.Dir(python))+"/.", runtime)
	output, err := copyRuntime.CombinedOutput()
	require.NoError(t, err, "%s", output)
	python = filepath.Join(runtime, "bin", "python3")
	wheels := t.TempDir()
	for _, pkg := range []struct{ name, version string }{
		{"checker_fixture", "1.0"}, {"driver_fixture", "1.0"}, {"driver_fixture", "2.0"}, {"project_self", "1.0"},
	} {
		name, version := pkg.name, pkg.version
		wheel, err := os.Create(filepath.Join(wheels, name+"-"+version+"-py3-none-any.whl"))
		require.NoError(t, err)
		zw := zip.NewWriter(wheel)
		for name, data := range map[string]string{
			name + ".py":  fmt.Sprintf("value = 'python %s ready'\n", version),
			name + ".pth": "import sys,pathlib; pathlib.Path(sys.prefix, 'pth-ran').write_text('started')\n",
			name + "-" + version + ".dist-info/METADATA": "Metadata-Version: 2.1\nName: " + strings.ReplaceAll(name, "_", "-") + "\nVersion: " + version + "\nProvides-Extra: test\n",
			name + "-" + version + ".dist-info/WHEEL":    "Wheel-Version: 1.0\nGenerator: fixture\nRoot-Is-Purelib: true\nTag: py3-none-any\n",
			name + "-" + version + ".dist-info/RECORD":   "",
		} {
			w, err := zw.Create(name)
			require.NoError(t, err)
			_, err = io.WriteString(w, data)
			require.NoError(t, err)
		}
		require.NoError(t, zw.Close())
		require.NoError(t, wheel.Close())
	}
	tools := t.TempDir()
	// The trusted tool fixture redirects registry downloads to our controlled wheel.
	wrapper := fmt.Sprintf(`#!/bin/sh
case "$*" in
*download*) exec '%s' "$@" --no-index --find-links '%s';;
*) exec '%s' "$@";;
esac
`, python, wheels, python)
	require.NoError(t, os.WriteFile(filepath.Join(tools, "python3"), []byte(wrapper), 0o700))
	// Exercise a trusted Python installation that isn't included in mise's read grants.
	node, err := resolveCICheckCommand(t.Context(), "/", "node")
	require.NoError(t, err)
	node, err = filepath.EvalSymlinks(node)
	require.NoError(t, err)
	mise := fmt.Sprintf("#!/bin/sh\ncase \"$*\" in 'ls --current --installed --json') printf '%%s' '{\"node\":[{\"install_path\":%q}]}';; 'bin-paths') printf '%%s\\n' '%s';; *) exit 1;; esac\n", filepath.Dir(filepath.Dir(node)), filepath.Dir(node))
	require.NoError(t, os.WriteFile(filepath.Join(tools, "mise"), []byte(mise), 0o700))
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
	marker := filepath.Join(t.TempDir(), "lifecycle-ran")
	require.NoError(t, os.WriteFile(filepath.Join(wheels, "missing_source_only-1.0.tar.gz"), ciCheckPackageArchive(t, map[string]string{
		"source/pyproject.toml": "[build-system]\nrequires=[]\nbuild-backend='backend'\nbackend-path=['.']\n",
		"source/backend.py":     fmt.Sprintf("from pathlib import Path\ndef get_requires_for_build_wheel(config_settings=None):\n Path(%q).write_text('source build ran')\n raise RuntimeError('controlled backend')\n", marker),
	}), 0o600))
	manifest := fmt.Sprintf(`{"name":"checker-fixture","version":"1.0.0","main":"index.js","bin":{"fixture-tool":"./index.js"},"scripts":{"install":"node -e \"require('fs').writeFileSync('%s','ran')\""}}`, marker)
	archive := ciCheckPackageArchive(t, map[string]string{"package/package.json": manifest, "package/index.js": "module.exports = 'javascript ready';\n"})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(archive) }))
	t.Cleanup(server.Close)
	repo := testutil.NewTestRepoWithCommit(t)
	repo.CommitFile("pyproject.toml", "[project]\nname = 'Project.Self'\ndependencies = ['checker-fixture [test] >=1; python_version >= \"3\"', 'missing-source-only']\n[project.optional-dependencies]\na = ['driver-fixture==1.0']\nb = ['driver-fixture==2.0']\nall = ['PROJECT_self[dev]']\n", "python dependencies")
	repo.CommitFile("web/package.json", `{"name":"fixture","version":"1.0.0","main":"./index.js","exports":"./index.js","overrides":{"checker-fixture":"$checker-fixture"},"dependencies":{"checker-fixture":"1.0.0"}}`, "javascript dependencies")
	repo.CommitFile("web/package-lock.json", fmt.Sprintf(`{"name":"fixture","version":"1.0.0","lockfileVersion":3,"packages":{"":{"name":"fixture","version":"1.0.0","dependencies":{"checker-fixture":"1.0.0"}},"node_modules/checker-fixture":{"version":"1.0.0","resolved":%q,"hasInstallScript":true,"bin":{"fixture-tool":"./index.js"}}}}`, server.URL+"/package.tgz"), "javascript lock")
	sha := repo.HeadSHA()
	command := filepath.Join(t.TempDir(), "codex")
	require.NoError(t, os.WriteFile(command, []byte("#!/bin/sh\nexit 0\n"), 0o700))
	var log bytes.Buffer
	run, err := (&WorkerPool{}).prepareCICheck(t.Context(), &storage.ReviewJob{RepoPath: repo.Root, GitRef: sha + ".." + sha}, command, nil, &log)
	require.NoError(t, err)
	t.Cleanup(run.cleanup)
	assert.NoFileExists(t, marker, log.String())
	require.FileExists(t, filepath.Join(run.Repo, "web", "node_modules", "checker-fixture", "index.js"), log.String())
	require.FileExists(t, filepath.Join(run.Repo, ".venv", "bin", "python"), log.String())
	published, err := filepath.Glob(filepath.Join(run.Repo, ".venv", "lib", "python*", "site-packages", "project_self.py"))
	require.NoError(t, err)
	assert.Empty(t, published, "self-referencing extras must leave the PR's package uninstalled")
	assert.NoFileExists(t, filepath.Join(run.Repo, ".venv", "pth-ran"), "populated Python must first start inside the sandbox")
	run.Job.Route = sandbox.Route{Name: "fixture", Upstream: server.URL, Path: "/responses", EnvVar: "FIXTURE_TOKEN", Capture: []string{"/bin/echo", "fixture-token"}}
	probe := fmt.Sprintf(`set -e
cd '%s'
python -c 'import checker_fixture,driver_fixture; assert driver_fixture.value == "python 1.0 ready"; print(checker_fixture.value)'
node -e "console.log(require('./web/node_modules/checker-fixture'))"
`, run.Repo)
	argv, env, cleanup, err := run.Job.Command(t.Context(), []string{"bash", "--norc", "--noprofile", "-c", probe})
	require.NoError(t, err)
	t.Cleanup(cleanup)
	cmd := exec.CommandContext(t.Context(), argv[0], argv[1:]...)
	cmd.Env = env
	output, err = cmd.CombinedOutput()
	require.NoError(t, err, "%s\n%s", output, log.String())
	assert.Contains(t, string(output), "python 1.0 ready\n")
	assert.Contains(t, string(output), "javascript ready\n")
	assert.FileExists(t, filepath.Join(run.Repo, ".venv", "pth-ran"))
}

func TestCICheckPackagesDependencySourcesIntegration(t *testing.T) {
	for _, name := range []string{"npm", "node"} {
		_, err := exec.LookPath(name)
		require.NoError(t, err)
	}
	hostArchive := filepath.Join(t.TempDir(), "host-fixture.tgz")
	require.NoError(t, os.WriteFile(hostArchive, ciCheckPackageArchive(t, map[string]string{
		"package/package.json": `{"name":"host-fixture","version":"1.0.0"}`,
		"package/index.js":     "module.exports = 'host-only fixture';\n",
	}), 0o600))
	hostDirectory := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(hostDirectory, "package.json"), []byte(`{"name":"host-fixture","version":"1.0.0"}`), 0o600))
	repo := testutil.NewTestRepoWithCommit(t)
	gitServer := httptest.NewTLSServer(http.FileServer(http.Dir(repo.Root)))
	t.Cleanup(gitServer.Close)
	marker := filepath.Join(t.TempDir(), "git-payload-ran")
	gitSource := "git+" + gitServer.URL + "/.git"
	repo.CommitFile("package.json", fmt.Sprintf(`{"name":"git-fixture","version":"1.0.0","scripts":{"prepare":"node -e ''"},"dependencies":{"child-fixture":%q}}`, gitSource), "git package")
	repo.CommitFile(".npmrc", "git=./git-payload\n", "package settings")
	repo.CommitFile("git-payload", fmt.Sprintf("#!/bin/sh\nprintf ran > '%s'\nexit 1\n", marker), "controlled git command")
	require.NoError(t, os.Chmod(filepath.Join(repo.Root, "git-payload"), 0o700))
	repo.RunGit("add", "git-payload")
	repo.RunGit("commit", "-m", "executable fixture")
	repo.RunGit("update-server-info")
	certificate := filepath.Join(t.TempDir(), "certificate.pem")
	require.NoError(t, os.WriteFile(certificate, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: gitServer.Certificate().Raw}), 0o600))
	archives := map[string][]byte{
		"/file.tgz": ciCheckPackageArchive(t, map[string]string{
			"package/package.json": fmt.Sprintf(`{"name":"remote-fixture","version":"1.0.0","dependencies":{"host-fixture":%q}}`, "file:"+hostArchive),
		}),
		"/git.tgz": ciCheckPackageArchive(t, map[string]string{
			"package/package.json": fmt.Sprintf(`{"name":"remote-fixture","version":"1.0.0","dependencies":{"git-fixture":%q}}`, gitSource),
		}),
	}
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(archives[r.URL.Path]) }))
	t.Cleanup(remote.Close)
	hostedSource := "github:checker-fixture/package#0123456789abcdef0123456789abcdef01234567"
	hostedArchive := ciCheckPackageArchive(t, map[string]string{
		"package/package.json": `{"name":"git-fixture","version":"1.0.0","scripts":{"prepare":"node -e ''"},"dependencies":{"child-fixture":"git+https://example.invalid/child.git"}}`,
		"package/.npmrc":       "git=./git-payload\n",
		"package/git-payload":  fmt.Sprintf("#!/bin/sh\nprintf ran > '%s'\nexit 1\n", marker),
	})
	hostedParent := ciCheckPackageArchive(t, map[string]string{"package/package.json": fmt.Sprintf(`{"name":"parent-fixture","version":"1.0.0","dependencies":{"git-fixture":%q}}`, hostedSource)})
	hostedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/parent.tgz" {
			_, _ = w.Write(hostedParent)
		} else {
			_, _ = w.Write(hostedArchive)
		}
	}))
	t.Cleanup(hostedServer.Close)
	// Redirect only npm's HTTPS transport; dependency resolution and extraction stay native.
	preload := filepath.Join(t.TempDir(), "transport.cjs")
	require.NoError(t, os.WriteFile(preload, []byte(fmt.Sprintf(`const https = require('https'), http = require('http');
https.request = options => http.request({...options, protocol: 'http:', hostname: '127.0.0.1', port: new URL(%q).port, agent: undefined});
`, hostedServer.URL)), 0o600))

	for _, tc := range []struct {
		name, kind, dependency, source, nodeOptions string
	}{
		{"file manifest", "file", "host-fixture", "file:" + hostArchive, ""},
		{"file directory", "directory", "host-fixture", "file:" + hostDirectory, ""},
		{"file locked link", "link", "host-fixture", "1.0.0", ""},
		{"file transitive", "file", "remote-fixture", remote.URL + "/file.tgz", ""},
		{"git direct", "git", "git-fixture", gitSource, ""},
		{"git transitive", "git", "remote-fixture", remote.URL + "/git.tgz", ""},
		{"git hosted", "git", "git-fixture", hostedSource, "--require=" + preload},
		{"git hosted transitive", "git", "parent-fixture", hostedServer.URL + "/parent.tgz", "--require=" + preload},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, os.RemoveAll(marker))
			project, stage, private := t.TempDir(), t.TempDir(), t.TempDir()
			for _, dir := range []string{"home", "tmp"} {
				require.NoError(t, os.Mkdir(filepath.Join(private, dir), 0o700))
			}
			metadata := map[string][]byte{"package.json": []byte(fmt.Sprintf(`{"name":"fixture","version":"1.0.0","dependencies":{%q:%q}}`, tc.dependency, tc.source))}
			if tc.kind == "link" {
				relative, err := filepath.Rel(stage, hostDirectory)
				require.NoError(t, err)
				metadata["package-lock.json"] = []byte(fmt.Sprintf(`{"lockfileVersion":3,"packages":{"":{"dependencies":{"host-fixture":"1.0.0"}},"node_modules/host-fixture":{"resolved":%q,"link":true},%q:{"name":"host-fixture","version":"1.0.0"}}}`, relative, relative))
			}
			env := append(ciCheckPackageEnv(private, os.Getenv("PATH"), nil), "GIT_SSL_CAINFO="+certificate)
			if tc.nodeOptions != "" {
				env = append(env, "NODE_OPTIONS="+tc.nodeOptions)
			}
			var log bytes.Buffer
			prepareCICheckJS(t.Context(), project, stage, metadata, nil, env, &log)
			assert.NoDirExists(t, filepath.Join(project, "node_modules"), log.String())
			if tc.kind == "git" {
				assert.NoFileExists(t, marker, log.String())
				assert.Contains(t, log.String(), "package preparation failed")
			} else if tc.kind == "link" {
				assert.Contains(t, log.String(), "local dependencies")
			} else {
				assert.Contains(t, log.String(), "package preparation failed")
			}
		})
	}
}

// A tool venv's site configuration must not let registry metadata run a host build.
func TestCICheckPackagesPythonConfigIntegration(t *testing.T) {
	python, err := resolveCICheckCommand(t.Context(), "/", "python3")
	require.NoError(t, err)
	toolVenv := filepath.Join(t.TempDir(), "tool")
	command := exec.CommandContext(t.Context(), python, "-I", "-m", "venv", toolVenv)
	output, err := command.CombinedOutput()
	require.NoError(t, err, "%s", output)
	marker := filepath.Join(t.TempDir(), "backend-ran")
	source := ciCheckPackageArchive(t, map[string]string{
		"source/pyproject.toml": "[build-system]\nrequires=[]\nbuild-backend='backend'\nbackend-path=['.']\n",
		"source/backend.py":     fmt.Sprintf("from pathlib import Path\ndef get_requires_for_build_wheel(config_settings=None):\n Path(%q).write_text('executed')\n raise RuntimeError('controlled backend')\n", marker),
	})
	var wheel bytes.Buffer
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/simple/config-chain-fixture/":
			_, _ = io.WriteString(w, `<a href="/config_chain_fixture-1.0-py3-none-any.whl">fixture</a>`)
		case "/config_chain_fixture-1.0-py3-none-any.whl":
			_, _ = w.Write(wheel.Bytes())
		case "/source.tar.gz":
			_, _ = w.Write(source)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	zw := zip.NewWriter(&wheel)
	for name, data := range map[string]string{
		"METADATA": "Metadata-Version: 2.1\nName: config-chain-fixture\nVersion: 1.0\nRequires-Dist: source-fixture @ " + server.URL + "/source.tar.gz\n",
		"WHEEL":    "Wheel-Version: 1.0\nRoot-Is-Purelib: true\nTag: py3-none-any\n",
		"RECORD":   "",
	} {
		entry, err := zw.Create("config_chain_fixture-1.0.dist-info/" + name)
		require.NoError(t, err)
		_, err = io.WriteString(entry, data)
		require.NoError(t, err)
	}
	require.NoError(t, zw.Close())
	require.NoError(t, os.WriteFile(filepath.Join(toolVenv, "pip.conf"), []byte("[global]\nindex-url = "+server.URL+"/simple/\n"), 0o600))
	tools := t.TempDir()
	wrapper := fmt.Sprintf("#!/bin/sh\ncase \"$*\" in *download*) exec '%s/bin/python' \"$@\" --retries 0 --timeout 1;; *) exec '%s/bin/python' \"$@\";; esac\n", toolVenv, toolVenv)
	require.NoError(t, os.WriteFile(filepath.Join(tools, "python3"), []byte(wrapper), 0o700))
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
	stage := t.TempDir()
	for _, dir := range []string{"home", "tmp"} {
		require.NoError(t, os.Mkdir(filepath.Join(stage, dir), 0o700))
	}
	probe := exec.CommandContext(t.Context(), filepath.Join(tools, "python3"), "-I", "-m", "pip", "--isolated", "download", "--only-binary=:all:", "--dest", filepath.Join(stage, "probe-wheels"), "config-chain-fixture")
	probe.Env = append(ciCheckPackageEnv(stage, os.Getenv("PATH"), nil), "PIP_CONFIG_FILE="+filepath.Join(toolVenv, "pip.conf"))
	output, err = probe.CombinedOutput()
	require.Error(t, err, "%s", output)
	require.FileExists(t, marker, "the configured fixture must reach its backend: %s", output)
	require.NoError(t, os.Remove(marker))
	var log bytes.Buffer
	prepareCICheckPython(t.Context(), t.TempDir(), stage, []byte("[project]\ndependencies=['config-chain-fixture']\n"), ciCheckPackageEnv(stage, os.Getenv("PATH"), nil), &log)
	assert.NoFileExists(t, marker, log.String())
}

func TestCICheckPackagesPythonRequirementFilterIntegration(t *testing.T) {
	stage := t.TempDir()
	for _, dir := range []string{"home", "tmp"} {
		require.NoError(t, os.Mkdir(filepath.Join(stage, dir), 0o700))
	}
	var log bytes.Buffer
	prepared := prepareCICheckPython(t.Context(), t.TempDir(), stage, []byte(`[project]
dependencies = ['colorama; python_version < "0"', 'numpy=>1', 'fixture @ https://user:secret@example.com/wheel.whl trailing', 'fixture @ https://user:secret@example.invalid/wheel.whl']
`), ciCheckPackageEnv(stage, os.Getenv("PATH"), nil), &log)
	assert.True(t, prepared, "valid entries must remain available: %s", log.String())
	assert.Contains(t, log.String(), "invalid requirement in pyproject.toml dependency declaration 2")
	assert.Contains(t, log.String(), "invalid requirement in pyproject.toml dependency declaration 3")
	assert.NotContains(t, log.String(), "secret")
	assert.NotContains(t, log.String(), "https://")
	assert.NotContains(t, log.String(), "package preparation failed")
}

func ciCheckPackageArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	for name, data := range files {
		mode := int64(0o600)
		if strings.HasSuffix(name, "/git-payload") {
			mode = 0o700
		}
		require.NoError(t, tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(data))}))
		_, err := io.WriteString(tw, data)
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	return archive.Bytes()
}

func TestCICheckPackagesSourceClassificationIntegration(t *testing.T) {
	for _, name := range []string{"npm", "node"} {
		_, err := exec.LookPath(name)
		require.NoError(t, err)
	}
	t.Run("separate global prefix", func(t *testing.T) {
		stage := t.TempDir()
		env := append(ciCheckPackageEnv(stage, os.Getenv("PATH"), nil), "npm_config_prefix="+t.TempDir())
		local, err := ciCheckLocalJSSources(t.Context(), stage, []string{"file:../local"}, []string{"file", "directory"}, nil, env)
		require.NoError(t, err)
		assert.True(t, local)
	})

	t.Run("workspace:*", func(t *testing.T) {
		stage := t.TempDir()
		local, err := ciCheckLocalJSSources(t.Context(), stage, []string{"workspace:*"}, []string{"file", "directory"}, nil, ciCheckPackageEnv(stage, os.Getenv("PATH"), nil))
		require.NoError(t, err)
		assert.True(t, local)
	})
}

func TestCICheckPackagesBunLockIntegration(t *testing.T) {
	if _, err := exec.LookPath("bun"); err != nil {
		t.Skip("requires installed Bun")
	}
	archive := ciCheckPackageArchive(t, map[string]string{
		"package/package.json": `{"name":"checker-fixture","version":"1.0.0","bin":{"fixture-tool":"./index.js"}}`,
		"package/index.js":     "module.exports = 'javascript ready';\n",
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(archive) }))
	t.Cleanup(server.Close)
	bun, err := resolveCICheckCommand(t.Context(), "/", "bun")
	require.NoError(t, err)
	archivePath := filepath.Join(t.TempDir(), "package.tgz")
	require.NoError(t, os.WriteFile(archivePath, archive, 0o600))
	stage := t.TempDir()
	localManifest := fmt.Sprintf(`{"name":"fixture","version":"1.0.0","dependencies":{"checker-fixture":%q}}`, server.URL+"/package.tgz")
	require.NoError(t, os.WriteFile(filepath.Join(stage, "package.json"), []byte(localManifest), 0o600))
	generate := exec.CommandContext(t.Context(), bun, "install", "--ignore-scripts", "--lockfile-only", "--no-env-file")
	generate.Dir = stage
	for _, dir := range []string{"home", "tmp"} {
		require.NoError(t, os.Mkdir(filepath.Join(stage, dir), 0o700))
	}
	generate.Env = ciCheckPackageEnv(stage, os.Getenv("PATH"), nil)
	output, err := generate.CombinedOutput()
	require.NoError(t, err, "%s", output)
	lock, err := os.ReadFile(filepath.Join(stage, "bun.lock"))
	require.NoError(t, err)
	registryLock := string(lock)
	require.Contains(t, registryLock, "checker-fixture@"+server.URL+"/package.tgz")
	for _, tc := range []struct{ name, source string }{
		{"escaped", `\u002f` + archivePath[1:]},
		{"registry", server.URL + "/package.tgz"},
		{"Bun GitHub source", "github:checker-fixture/package#0123456789abcdef0123456789abcdef01234567"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			localRepo := testutil.NewTestRepoWithCommit(t)
			manifest := localManifest
			sourceLock := strings.ReplaceAll(registryLock, "checker-fixture@"+server.URL+"/package.tgz", "checker-fixture@"+tc.source)
			if strings.HasPrefix(tc.source, "github:") {
				manifest = strings.ReplaceAll(manifest, server.URL+"/package.tgz", tc.source)
				sourceLock = strings.ReplaceAll(sourceLock, server.URL+"/package.tgz", tc.source)
				// Keep native parsing; isolate the preparation decision from public Git fetching.
				tools := t.TempDir()
				wrapper := fmt.Sprintf("#!/bin/sh\ncase \"$*\" in *--eval*) exec '%s' \"$@\";; *) /bin/mkdir -p node_modules/checker-fixture; printf ready > node_modules/checker-fixture/index.js;; esac\n", bun)
				require.NoError(t, os.WriteFile(filepath.Join(tools, "bun"), []byte(wrapper), 0o700))
				t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
			}
			localRepo.CommitFile("package.json", manifest, "dependency declaration")
			localRepo.CommitFile("bun.lock", sourceLock, "locked archive")
			localRepo.CommitFile("npm-shrinkwrap.json", `{"packages":{"node_modules/obsolete":{"resolved":"file:/obsolete.tgz"}}}`, "obsolete npm lock")
			localRepo.CommitFile("package-lock.json", `{"packages":{"node_modules/obsolete":{"resolved":"file:/obsolete.tgz"}}}`, "obsolete npm lock")
			var preparationLog bytes.Buffer
			prepareCICheckPackages(t.Context(), localRepo.Root, t.TempDir(), os.Getenv("PATH"), nil, &preparationLog)
			if !strings.HasPrefix(tc.source, `\u002f`) {
				assert.FileExists(t, filepath.Join(localRepo.Root, "node_modules", "checker-fixture", "index.js"), preparationLog.String())
			} else {
				assert.NoDirExists(t, filepath.Join(localRepo.Root, "node_modules"), preparationLog.String())
				assert.Contains(t, preparationLog.String(), "skips JavaScript local dependencies")
			}
		})
	}
}

func TestCICheckPackagesActiveShrinkwrapIntegration(t *testing.T) {
	archive := ciCheckPackageArchive(t, map[string]string{
		"package/package.json": `{"name":"checker-fixture","version":"1.0.0"}`,
		"package/index.js":     "module.exports = 'ready';\n",
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(archive) }))
	t.Cleanup(server.Close)
	repo := testutil.NewTestRepoWithCommit(t)
	repo.CommitFile("package.json", `{"name":"fixture","version":"1.0.0","dependencies":{"checker-fixture":"1.0.0"}}`, "dependency")
	repo.CommitFile("npm-shrinkwrap.json", fmt.Sprintf(`{"name":"fixture","version":"1.0.0","lockfileVersion":3,"packages":{"":{"name":"fixture","version":"1.0.0","dependencies":{"checker-fixture":"1.0.0"}},"node_modules/checker-fixture":{"version":"1.0.0","resolved":%q},"node_modules/removed-fixture":{"version":"1.0.0","resolved":"file:/removed-fixture.tgz"}}}`, server.URL+"/package.tgz"), "active lock")
	repo.CommitFile("package-lock.json", `{"packages":{"node_modules/obsolete":{"resolved":"file:/obsolete.tgz"}}}`, "obsolete lock")
	var log bytes.Buffer
	prepareCICheckPackages(t.Context(), repo.Root, t.TempDir(), os.Getenv("PATH"), nil, &log)
	assert.FileExists(t, filepath.Join(repo.Root, "node_modules", "checker-fixture", "index.js"), log.String())
	assert.NoDirExists(t, filepath.Join(repo.Root, "node_modules", "removed-fixture"))
}

func TestCICheckPackagesWorkspaceIntegration(t *testing.T) {
	archive := ciCheckPackageArchive(t, map[string]string{
		"package/package.json": `{"name":"checker-fixture","version":"1.0.0","bin":{"fixture-tool":"./index.js"}}`,
		"package/index.js":     "module.exports = 'javascript ready';\n",
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(archive) }))
	t.Cleanup(server.Close)
	workspaceRepo := testutil.NewTestRepoWithCommit(t)
	workspaceRepo.CommitFile("package.json", `{"workspaces":["packages/*"]}`, "workspace declarations")
	workspaceRepo.CommitFile("bun.lock", `{}`, "workspace lock")
	manifest := fmt.Sprintf(`{"name":"fixture","dependencies":{"checker-fixture":%q}}`, server.URL+"/package.tgz")
	workspaceRepo.CommitFile("packages/ui/package.json", manifest, "workspace member")
	workspaceRepo.CommitFile("web/package.json", manifest, "independent project")
	var preparationLog bytes.Buffer
	prepareCICheckPackages(t.Context(), workspaceRepo.Root, t.TempDir(), os.Getenv("PATH"), nil, &preparationLog)
	assert.NoDirExists(t, filepath.Join(workspaceRepo.Root, "packages", "ui", "node_modules"), preparationLog.String())
	assert.FileExists(t, filepath.Join(workspaceRepo.Root, "web", "node_modules", "checker-fixture", "index.js"), preparationLog.String())
}
