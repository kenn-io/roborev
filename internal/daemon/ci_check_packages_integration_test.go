//go:build integration && !windows

package daemon

import (
	"archive/zip"
	"bytes"
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

// The real sandbox imports prepared packages and starts their .pth files there.
func TestCICheckPackagesSandboxIntegration(t *testing.T) {
	testutil.RequireSandboxIntegration(t)
	requireCICheckNpm(t, true)
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("requires installed Python")
	}
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	python, err := resolveCICheckCommand(t.Context(), "/", "python3")
	require.NoError(t, err)
	wheels := t.TempDir()
	ciCheckPackageWheel(t, wheels, "checker_fixture", "1.0", "")
	tools := t.TempDir()
	// The trusted tool fixture redirects registry downloads to our controlled wheel.
	wrapper := fmt.Sprintf(`#!/bin/sh
case "$*" in
*download*) exec '%s' "$@" --no-index --find-links '%s';;
*) exec '%s' "$@";;
esac
`, python, wheels, python)
	require.NoError(t, os.WriteFile(filepath.Join(tools, "python3"), []byte(wrapper), 0o700))
	node, err := resolveCICheckCommand(t.Context(), "/", "node")
	require.NoError(t, err)
	node, err = filepath.EvalSymlinks(node)
	require.NoError(t, err)
	mise := fmt.Sprintf("#!/bin/sh\ncase \"$*\" in 'ls --current --installed --json') printf '%%s' '{\"node\":[{\"install_path\":%q}],\"python\":[{\"install_path\":%q}]}';; 'bin-paths') printf '%%s\\n' '%s' '%s';; *) exit 1;; esac\n", filepath.Dir(filepath.Dir(node)), filepath.Dir(filepath.Dir(python)), tools, filepath.Dir(node))
	require.NoError(t, os.WriteFile(filepath.Join(tools, "mise"), []byte(mise), 0o700))
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
	manifest := `{"name":"checker-fixture","version":"1.0.0","main":"index.js","bin":{"fixture-tool":"./index.js"}}`
	archive := ciCheckPackageArchive(t, map[string]string{"package/package.json": manifest, "package/index.js": "module.exports = 'javascript ready';\n"})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(archive) }))
	t.Cleanup(server.Close)
	repo := testutil.NewTestRepoWithCommit(t)
	repo.CommitFile("pyproject.toml", "[project]\ndependencies = ['checker-fixture']\n", "python dependencies")
	repo.CommitFile("web/package.json", `{"name":"fixture","version":"1.0.0","main":"./index.js","exports":"./index.js","overrides":{"checker-fixture":"$checker-fixture"},"dependencies":{"checker-fixture":"1.0.0"}}`, "javascript dependencies")
	repo.CommitFile("web/package-lock.json", fmt.Sprintf(`{"name":"fixture","version":"1.0.0","lockfileVersion":3,"packages":{"":{"name":"fixture","version":"1.0.0","dependencies":{"checker-fixture":"1.0.0"}},"node_modules/checker-fixture":{"version":"1.0.0","resolved":%q,"hasInstallScript":true,"bin":{"fixture-tool":"./index.js"}}}}`, server.URL+"/package.tgz"), "javascript lock")
	var log bytes.Buffer
	sha := repo.HeadSHA()
	command := filepath.Join(t.TempDir(), "codex")
	require.NoError(t, os.WriteFile(command, []byte("#!/bin/sh\nexit 0\n"), 0o700))
	run, err := (&WorkerPool{}).prepareCICheck(t.Context(), &storage.ReviewJob{RepoPath: repo.Root, GitRef: sha + ".." + sha}, command, nil, &log)
	require.NoError(t, err)
	t.Cleanup(run.cleanup)
	assert.NoFileExists(t, filepath.Join(run.Repo, ".venv", "pth-ran"), "populated Python must first start inside the sandbox")
	run.Job.Route = sandbox.Route{Name: "fixture", Upstream: server.URL, Path: "/responses", EnvVar: "FIXTURE_TOKEN", Capture: []string{"/bin/echo", "fixture-token"}}
	probe := fmt.Sprintf(`set -e
cd '%s'
python -c 'import checker_fixture; print(checker_fixture.value)'
node -e "console.log(require('./web/node_modules/checker-fixture'))"
`, run.Repo)
	argv, env, cleanup, err := run.Job.Command(t.Context(), []string{"bash", "--norc", "--noprofile", "-c", probe})
	require.NoError(t, err)
	t.Cleanup(cleanup)
	cmd := exec.CommandContext(t.Context(), argv[0], argv[1:]...)
	cmd.Env = env
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s\n%s", output, log.String())
	assert.Contains(t, string(output), "python 1.0 ready\n")
	assert.Contains(t, string(output), "javascript ready\n")
	assert.FileExists(t, filepath.Join(run.Repo, ".venv", "pth-ran"))
}

func TestCICheckPackagesDependencySourcesIntegration(t *testing.T) {
	requireCICheckNpm(t, true)
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
	gitSource := "git+" + gitServer.URL + "/.git"
	repo.CommitFile("package.json", fmt.Sprintf(`{"name":"git-fixture","version":"1.0.0","scripts":{"prepare":"node -e ''"},"dependencies":{"child-fixture":%q}}`, gitSource), "git package")
	repo.RunGit("update-server-info")
	certificate := filepath.Join(t.TempDir(), "certificate.pem")
	require.NoError(t, os.WriteFile(certificate, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: gitServer.Certificate().Raw}), 0o600))

	for _, tc := range []struct {
		name, kind, dependency, source string
	}{
		{"file manifest", "file", "host-fixture", "file:" + hostArchive},
		{"file directory", "directory", "host-fixture", "file:" + hostDirectory},
		{"file locked link", "link", "host-fixture", "1.0.0"},
		{"file locked string link", "link string", "host-fixture", "1.0.0"},
		{"file locked number link", "link number", "host-fixture", "1.0.0"},
		{"file v1 package lock absolute", "link v1 absolute", "host-fixture", "1.0.0"},
		{"git direct", "git", "git-fixture", gitSource},
	} {
		t.Run(tc.name, func(t *testing.T) {
			project, stage, private := t.TempDir(), t.TempDir(), t.TempDir()
			for _, dir := range []string{"home", "tmp"} {
				require.NoError(t, os.Mkdir(filepath.Join(private, dir), 0o700))
			}
			metadata := map[string][]byte{"package.json": []byte(fmt.Sprintf(`{"name":"fixture","version":"1.0.0","dependencies":{%q:%q}}`, tc.dependency, tc.source))}
			if strings.HasPrefix(tc.kind, "link") {
				relative, err := filepath.Rel(stage, hostDirectory)
				require.NoError(t, err)
				link := map[string]string{"link": "true", "link string": `"true"`, "link number": "1"}[tc.kind]
				metadata["package-lock.json"] = []byte(fmt.Sprintf(`{"lockfileVersion":3,"packages":{"":{"dependencies":{"host-fixture":"1.0.0"}},"node_modules/host-fixture":{"resolved":%q,"link":%s},%q:{"name":"host-fixture","version":"1.0.0"}}}`, relative, link, relative))
				if strings.HasPrefix(tc.kind, "link v1") {
					source := "file:" + hostDirectory
					metadata["package-lock.json"] = []byte(fmt.Sprintf(`{"name":"fixture","version":"1.0.0","lockfileVersion":1,"dependencies":{"host-fixture":{"version":%q}}}`, source))
				}
			}
			env := append(ciCheckPackageEnv(private, os.Getenv("PATH"), nil), "GIT_SSL_CAINFO="+certificate)
			var log bytes.Buffer
			prepareCICheckJS(t.Context(), project, stage, metadata, nil, env, &log)
			assert.NoDirExists(t, filepath.Join(project, "node_modules"), log.String())
			if strings.HasPrefix(tc.kind, "link") {
				assert.Contains(t, log.String(), "local dependencies")
			} else {
				assert.Contains(t, log.String(), "package preparation failed")
			}
		})
	}
}

// A tool venv's site configuration must not let registry metadata run a host build.
func TestCICheckPackagesPythonConfigIntegration(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("requires installed Python")
	}
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
	var wheel []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/simple/config-chain-fixture/":
			_, _ = io.WriteString(w, `<a href="/config_chain_fixture-1.0-py3-none-any.whl">fixture</a>`)
		case "/config_chain_fixture-1.0-py3-none-any.whl":
			_, _ = w.Write(wheel)
		case "/source.tar.gz":
			_, _ = w.Write(source)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	wheels := t.TempDir()
	ciCheckPackageWheel(t, wheels, "config_chain_fixture", "1.0", "Requires-Dist: source-fixture @ "+server.URL+"/source.tar.gz\n")
	wheel, err = os.ReadFile(filepath.Join(wheels, "config_chain_fixture-1.0-py3-none-any.whl"))
	require.NoError(t, err)
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
	prepared := prepareCICheckPython(t.Context(), t.TempDir(), stage, []byte("[project]\ndependencies=['config-chain-fixture']\n"), ciCheckPackageEnv(stage, os.Getenv("PATH"), nil), &log)
	assert.False(t, prepared, log.String())
	assert.NoFileExists(t, marker, log.String())
}

func TestCICheckPackagesPythonRequirementFilterIntegration(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("requires installed Python")
	}
	python, err := resolveCICheckCommand(t.Context(), "/", "python3")
	require.NoError(t, err)
	wheels := t.TempDir()
	for _, pkg := range []struct{ name, version, requires string }{
		{"checker_fixture", "1.0", ""}, {"driver_fixture", "1.0", ""}, {"driver_fixture", "2.0", ""}, {"project_self", "1.0", ""}, {"transitive_fixture", "1.0", "Requires-Dist: PROJECT.self==1.0\n"},
	} {
		ciCheckPackageWheel(t, wheels, pkg.name, pkg.version, pkg.requires)
	}
	buildMarker := filepath.Join(t.TempDir(), "source-build-ran")
	require.NoError(t, os.WriteFile(filepath.Join(wheels, "missing_source_only-1.0.tar.gz"), ciCheckPackageArchive(t, map[string]string{
		"source/pyproject.toml": "[build-system]\nrequires=[]\nbuild-backend='backend'\nbackend-path=['.']\n",
		"source/backend.py":     fmt.Sprintf("from pathlib import Path\ndef get_requires_for_build_wheel(config_settings=None):\n Path(%q).write_text('source build ran')\n raise RuntimeError('controlled backend')\n", buildMarker),
	}), 0o600))
	tools := t.TempDir()
	wrapper := fmt.Sprintf(`#!/bin/sh
case "$*" in
*download*) exec '%s' "$@" --no-index --find-links '%s';;
*) exec '%s' "$@";;
esac
`, python, wheels, python)
	require.NoError(t, os.WriteFile(filepath.Join(tools, "python3"), []byte(wrapper), 0o700))
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
	stage := t.TempDir()
	for _, dir := range []string{"home", "tmp"} {
		require.NoError(t, os.Mkdir(filepath.Join(stage, dir), 0o700))
	}
	var log bytes.Buffer
	project := t.TempDir()
	prepared := prepareCICheckPython(t.Context(), project, stage, []byte(`[project]
name = "Project.Self"
dependencies = ['colorama; python_version < "0"', 'numpy=>1', 'fixture @ https://user:secret@example.com/wheel.whl trailing', 'fixture @ https://user:secret@example.invalid/wheel.whl', 'checker-fixture [test] >=1; python_version >= "3"', 'missing-source-only']
[project.optional-dependencies]
a = ['driver-fixture==1.0']
b = ['driver-fixture==2.0']
all = ['PROJECT_self[dev]']
c = ['transitive-fixture']
`), ciCheckPackageEnv(stage, os.Getenv("PATH"), nil), &log)
	assert.True(t, prepared, "valid entries must remain available: %s", log.String())
	assert.Contains(t, log.String(), "invalid requirement in pyproject.toml dependency declaration 2")
	assert.Contains(t, log.String(), "invalid requirement in pyproject.toml dependency declaration 3")
	assert.NotContains(t, log.String(), "secret")
	assert.NotContains(t, log.String(), "https://")
	assert.NoFileExists(t, buildMarker, log.String())
	sites, err := filepath.Glob(filepath.Join(project, ".venv", "lib", "python*", "site-packages"))
	require.NoError(t, err)
	require.Len(t, sites, 1)
	assert.FileExists(t, filepath.Join(sites[0], "checker_fixture.py"))
	assert.FileExists(t, filepath.Join(sites[0], "driver_fixture-1.0.dist-info", "METADATA"))
	assert.NoDirExists(t, filepath.Join(sites[0], "driver_fixture-2.0.dist-info"))
	assert.NoFileExists(t, filepath.Join(sites[0], "project_self.py"), "direct and transitive references must leave the project's own package uninstalled")
	assert.NoFileExists(t, filepath.Join(sites[0], "transitive_fixture.py"))
}

func requireCICheckNpm(t *testing.T, policies bool) {
	t.Helper()
	for _, name := range []string{"npm", "node"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("requires installed %s", name)
		}
	}
	if policies {
		command := exec.CommandContext(t.Context(), "npm", "config", "get", "allow-file", "allow-directory", "allow-git")
		command.Dir = t.TempDir()
		output, err := command.CombinedOutput()
		if err != nil {
			t.Skipf("requires npm source policies: %s", output)
		}
		for _, name := range []string{"allow-file", "allow-directory", "allow-git"} {
			if !strings.Contains(string(output), name+"=all") {
				t.Skipf("requires npm source policy %s", name)
			}
		}
	}
}

func ciCheckPackageWheel(t *testing.T, dir, name, version, requires string) {
	t.Helper()
	wheel, err := os.Create(filepath.Join(dir, name+"-"+version+"-py3-none-any.whl"))
	require.NoError(t, err)
	zw := zip.NewWriter(wheel)
	metadata := "Metadata-Version: 2.1\nName: " + strings.ReplaceAll(name, "_", "-") + "\nVersion: " + version + "\nProvides-Extra: test\n" + requires
	for name, data := range map[string]string{
		name + ".py":  fmt.Sprintf("value = 'python %s ready'\n", version),
		name + ".pth": "import sys,pathlib; pathlib.Path(sys.prefix, 'pth-ran').write_text('started')\n",
		name + "-" + version + ".dist-info/METADATA": metadata,
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

func ciCheckPackageArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	entries := make([]testutil.ArchiveEntry, 0, len(files))
	for name, data := range files {
		entries = append(entries, testutil.ArchiveEntry{Name: name, Content: data, Mode: 0o600})
	}
	return testutil.CreateTarGzip(t, entries)
}

func TestCICheckPackagesSourceClassificationIntegration(t *testing.T) {
	requireCICheckNpm(t, false)
	t.Run("separate global prefix", func(t *testing.T) {
		stage := t.TempDir()
		env := append(ciCheckPackageEnv(stage, os.Getenv("PATH"), nil), "npm_config_prefix="+t.TempDir())
		local, err := ciCheckLocalJSSources(t.Context(), stage, []string{"file:../local"}, []string{"file", "directory"}, nil, nil, env)
		require.NoError(t, err)
		assert.True(t, local)
	})

	t.Run("workspace:*", func(t *testing.T) {
		stage := t.TempDir()
		local, err := ciCheckLocalJSSources(t.Context(), stage, []string{"workspace:*"}, []string{"file", "directory"}, nil, nil, ciCheckPackageEnv(stage, os.Getenv("PATH"), nil))
		require.NoError(t, err)
		assert.True(t, local)
	})
}

func TestCICheckPackagesBunLockIntegration(t *testing.T) {
	requireCICheckNpm(t, false)
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
			localRepo.CommitFile("npm-shrinkwrap.json", `{"packages":{"node_modules/obsolete":{"link":true}}}`, "obsolete npm lock")
			localRepo.CommitFile("package-lock.json", `{"packages":{"node_modules/obsolete":{"link":true}}}`, "obsolete npm lock")
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

func TestCICheckPackagesNpmLayoutsIntegration(t *testing.T) {
	requireCICheckNpm(t, true)
	lifecycleMarker := filepath.Join(t.TempDir(), "lifecycle-ran")
	archive := ciCheckPackageArchive(t, map[string]string{
		"package/package.json": fmt.Sprintf(`{"name":"checker-fixture","version":"1.0.0","bin":{"fixture-tool":"./index.js"},"scripts":{"install":"node -e \"require('fs').writeFileSync('%s','ran')\""}}`, lifecycleMarker),
		"package/index.js":     "module.exports = 'ready';\n",
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(archive) }))
	t.Cleanup(server.Close)
	manifest := `{"name":"fixture","version":"1.0.0","dependencies":{"checker-fixture":"1.0.0"}}`
	remoteManifest := fmt.Sprintf(`{"name":"fixture","dependencies":{"checker-fixture":%q}}`, server.URL+"/package.tgz")
	for _, tc := range []struct {
		name, module, missing string
		files                 map[string]string
	}{
		{"legacy ignores from", "", "", map[string]string{
			"package.json":        manifest,
			"npm-shrinkwrap.json": fmt.Sprintf(`{"name":"fixture","version":"1.0.0","lockfileVersion":1,"dependencies":{"checker-fixture":{"version":"1.0.0","from":"file:/removed-unused-directory","resolved":%q}}}`, server.URL+"/package.tgz"),
		}},
		{"shrinkwrap precedence", "", "", map[string]string{
			"package.json":        manifest,
			"npm-shrinkwrap.json": fmt.Sprintf(`{"name":"fixture","version":"1.0.0","lockfileVersion":3,"packages":{"":{"name":"fixture","version":"1.0.0","dependencies":{"checker-fixture":"1.0.0"}},"node_modules/checker-fixture":{"version":"1.0.0","resolved":%q}}}`, server.URL+"/package.tgz"),
			"package-lock.json":   `{"packages":{"node_modules/obsolete":{"link":true}}}`,
		}},
		{"packages map ignores legacy", "", "", map[string]string{
			"package.json":        manifest,
			"npm-shrinkwrap.json": fmt.Sprintf(`{"name":"fixture","version":"1.0.0","lockfileVersion":2,"packages":{"":{"name":"fixture","version":"1.0.0","dependencies":{"checker-fixture":"1.0.0"}},"node_modules/checker-fixture":{"version":"1.0.0","resolved":%q},"node_modules/removed-fixture":{"version":"1.0.0","resolved":"file:/removed-fixture.tgz"}},"dependencies":{"removed-fixture":{"version":"file:/removed-fixture.tgz"}}}`, server.URL+"/package.tgz"),
		}},
		{"workspace and independent project", "web", "packages/ui/node_modules", map[string]string{
			"package.json":             `{"workspaces":["packages/*"]}`,
			"packages/ui/package.json": remoteManifest,
			"web/package.json":         remoteManifest,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := testutil.NewTestRepoWithCommit(t)
			for name, data := range tc.files {
				repo.CommitFile(name, data, "package metadata")
			}
			var log bytes.Buffer
			prepareCICheckPackages(t.Context(), repo.Root, t.TempDir(), os.Getenv("PATH"), nil, &log)
			if tc.name == "shrinkwrap precedence" {
				assert.NoFileExists(t, lifecycleMarker, log.String())
			}
			assert.FileExists(t, filepath.Join(repo.Root, tc.module, "node_modules", "checker-fixture", "index.js"), log.String())
			if tc.missing != "" {
				assert.NoDirExists(t, filepath.Join(repo.Root, tc.missing), log.String())
			}
		})
	}
}
