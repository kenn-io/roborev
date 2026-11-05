//go:build integration

package daemon

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
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
	wheels := t.TempDir()
	for _, pkg := range []struct{ name, version string }{
		{"checker_fixture", "1.0"}, {"driver_fixture", "1.0"}, {"driver_fixture", "2.0"},
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
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
	marker := filepath.Join(t.TempDir(), "lifecycle-ran")
	manifest := fmt.Sprintf(`{"name":"checker-fixture","version":"1.0.0","main":"index.js","bin":{"fixture-tool":"./index.js"},"scripts":{"install":"node -e \"require('fs').writeFileSync('%s','ran')\""}}`, marker)
	archive := ciCheckPackageArchive(t, map[string]string{"package/package.json": manifest, "package/index.js": "module.exports = 'javascript ready';\n"})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(archive) }))
	t.Cleanup(server.Close)
	repo := testutil.NewTestRepoWithCommit(t)
	repo.CommitFile("setup.py", fmt.Sprintf("from pathlib import Path\nPath(%q).write_text('build ran')\n", marker), "source build sentinel")
	repo.CommitFile("pyproject.toml", fmt.Sprintf("[project]\ndependencies = ['checker-fixture [test] >=1; python_version >= \"3\"', 'source-fixture @ file://%s', 'missing-source-only']\n[project.optional-dependencies]\na = ['driver-fixture==1.0']\nb = ['driver-fixture==2.0']\n", repo.Root), "python dependencies")
	repo.CommitFile("project_module.py", "value = 'project ready'\n", "project import")
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
	assert.NoFileExists(t, filepath.Join(run.Repo, ".venv", "pth-ran"), "populated Python must first start inside the sandbox")
	run.Job.Route = sandbox.Route{Name: "fixture", Upstream: server.URL, Path: "/responses", EnvVar: "FIXTURE_TOKEN", Capture: []string{"/bin/echo", "fixture-token"}}
	probe := fmt.Sprintf(`set -e
cd '%s'
PYTHONPATH=. python -c 'import checker_fixture,driver_fixture,project_module; assert driver_fixture.value == "python 1.0 ready"; print(checker_fixture.value); print(project_module.value)'
node -e "console.log(require('./web/node_modules/checker-fixture'))"
`, run.Repo)
	argv, env, cleanup, err := run.Job.Command(t.Context(), []string{"bash", "--norc", "--noprofile", "-c", probe})
	require.NoError(t, err)
	t.Cleanup(cleanup)
	cmd := exec.CommandContext(t.Context(), argv[0], argv[1:]...)
	cmd.Env = env
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s\n%s", output, log.String())
	assert.Contains(t, string(output), "python 1.0 ready\nproject ready\n")
	assert.Contains(t, string(output), "javascript ready\n")
	assert.FileExists(t, filepath.Join(run.Repo, ".venv", "pth-ran"))
	assert.NoFileExists(t, marker)
	t.Run("absolute Bun lock", func(t *testing.T) {
		bun, err := resolveCICheckCommand(t.Context(), "/", "bun")
		require.NoError(t, err)
		archivePath := filepath.Join(t.TempDir(), "package.tgz")
		require.NoError(t, os.WriteFile(archivePath, archive, 0o600))
		stage := t.TempDir()
		localManifest := fmt.Sprintf(`{"name":"fixture","version":"1.0.0","dependencies":{"checker-fixture":%q}}`, "file:"+archivePath)
		require.NoError(t, os.WriteFile(filepath.Join(stage, "package.json"), []byte(localManifest), 0o600))
		generate := exec.CommandContext(t.Context(), bun, "install", "--ignore-scripts", "--lockfile-only", "--offline", "--no-env-file")
		generate.Dir = stage
		for _, dir := range []string{"home", "tmp"} {
			require.NoError(t, os.Mkdir(filepath.Join(stage, dir), 0o700))
		}
		generate.Env = ciCheckPackageEnv(stage, os.Getenv("PATH"), nil)
		output, err := generate.CombinedOutput()
		require.NoError(t, err, "%s", output)
		lock, err := os.ReadFile(filepath.Join(stage, "bun.lock"))
		require.NoError(t, err)
		registryLock := strings.ReplaceAll(string(lock), fmt.Sprintf("%q", "file:"+archivePath), `"1.0.0"`)
		require.Contains(t, registryLock, "checker-fixture@"+archivePath)
		for _, tc := range []struct{ name, source string }{
			{"escaped", `\u002f` + archivePath[1:]},
			{"registry", server.URL + "/package.tgz"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				localRepo := testutil.NewTestRepoWithCommit(t)
				localRepo.CommitFile("package.json", `{"name":"fixture","version":"1.0.0","dependencies":{"checker-fixture":"1.0.0"}}`, "registry dependency")
				sourceLock := strings.ReplaceAll(registryLock, "checker-fixture@"+archivePath, "checker-fixture@"+tc.source)
				localRepo.CommitFile("bun.lock", sourceLock, "locked archive")
				var preparationLog bytes.Buffer
				prepareCICheckPackages(t.Context(), localRepo.Root, t.TempDir(), os.Getenv("PATH"), nil, &preparationLog)
				if strings.HasPrefix(tc.source, "http:") {
					assert.FileExists(t, filepath.Join(localRepo.Root, "node_modules", "checker-fixture", "index.js"), preparationLog.String())
				} else {
					assert.NoDirExists(t, filepath.Join(localRepo.Root, "node_modules"), preparationLog.String())
					assert.Contains(t, preparationLog.String(), "skips JavaScript local dependencies")
				}
			})
		}
	})
	t.Run("workspace members", func(t *testing.T) {
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
	})
	t.Run("transitive local archive", func(t *testing.T) {
		hostArchive := filepath.Join(t.TempDir(), "host-fixture.tgz")
		require.NoError(t, os.WriteFile(hostArchive, ciCheckPackageArchive(t, map[string]string{
			"package/package.json": `{"name":"host-fixture","version":"1.0.0"}`,
			"package/index.js":     "module.exports = 'host-only fixture';\n",
		}), 0o600))
		remoteArchive := ciCheckPackageArchive(t, map[string]string{
			"package/package.json": fmt.Sprintf(`{"name":"remote-fixture","version":"1.0.0","dependencies":{"host-fixture":%q}}`, "file:"+hostArchive),
		})
		remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(remoteArchive) }))
		t.Cleanup(remote.Close)
		project, stage, private := filepath.Join(run.Repo, "transitive"), t.TempDir(), t.TempDir()
		require.NoError(t, os.Mkdir(project, 0o700))
		for _, dir := range []string{"home", "tmp"} {
			require.NoError(t, os.Mkdir(filepath.Join(private, dir), 0o700))
		}
		metadata := map[string][]byte{"package.json": []byte(fmt.Sprintf(`{"dependencies":{"remote-fixture":%q}}`, remote.URL+"/package.tgz"))}
		var preparationLog bytes.Buffer
		prepareCICheckJS(t.Context(), project, stage, metadata, nil, ciCheckPackageEnv(private, os.Getenv("PATH"), nil), &preparationLog)
		assert.NoDirExists(t, filepath.Join(project, "node_modules"), preparationLog.String())
		const probe = `if cat "$1" >/dev/null 2>&1; then echo host-readable; else echo host-denied; fi
node -e 'try { console.log(require(process.argv[1])); } catch { console.log("host-copy-blocked"); }' "$2"`
		argv, env, cleanup, err := run.Job.Command(t.Context(), []string{"bash", "--norc", "--noprofile", "-c", probe, "archive-probe", hostArchive, filepath.Join(project, "node_modules", "host-fixture")})
		require.NoError(t, err)
		t.Cleanup(cleanup)
		command := exec.CommandContext(t.Context(), argv[0], argv[1:]...)
		command.Env = env
		output, err := command.CombinedOutput()
		require.NoError(t, err, "%s", output)
		assert.Contains(t, string(output), "host-denied\n")
		assert.Contains(t, string(output), "host-copy-blocked\n")
	})
}

func ciCheckPackageArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	for name, data := range files {
		require.NoError(t, tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(data))}))
		_, err := io.WriteString(tw, data)
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	return archive.Bytes()
}
