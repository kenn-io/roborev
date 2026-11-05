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
	wheel, err := os.Create(filepath.Join(wheels, "checker_fixture-1.0-py3-none-any.whl"))
	require.NoError(t, err)
	zw := zip.NewWriter(wheel)
	for name, data := range map[string]string{
		"checker_fixture.py":                     "value = 'python ready'\n",
		"checker_fixture.pth":                    "import sys,pathlib; pathlib.Path(sys.prefix, 'pth-ran').write_text('started')\n",
		"checker_fixture-1.0.dist-info/METADATA": "Metadata-Version: 2.1\nName: checker-fixture\nVersion: 1.0\n",
		"checker_fixture-1.0.dist-info/WHEEL":    "Wheel-Version: 1.0\nGenerator: fixture\nRoot-Is-Purelib: true\nTag: py3-none-any\n",
		"checker_fixture-1.0.dist-info/RECORD":   "",
	} {
		w, err := zw.Create(name)
		require.NoError(t, err)
		_, err = io.WriteString(w, data)
		require.NoError(t, err)
	}
	require.NoError(t, zw.Close())
	require.NoError(t, wheel.Close())
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
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	manifest := fmt.Sprintf(`{"name":"checker-fixture","version":"1.0.0","main":"index.js","scripts":{"install":"node -e \"require('fs').writeFileSync('%s','ran')\""}}`, marker)
	for name, data := range map[string]string{"package/package.json": manifest, "package/index.js": "module.exports = 'javascript ready';\n"} {
		require.NoError(t, tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(data))}))
		_, err := io.WriteString(tw, data)
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(archive.Bytes()) }))
	t.Cleanup(server.Close)
	repo := testutil.NewTestRepoWithCommit(t)
	repo.CommitFile("setup.py", fmt.Sprintf("from pathlib import Path\nPath(%q).write_text('build ran')\n", marker), "source build sentinel")
	repo.CommitFile("pyproject.toml", fmt.Sprintf("[project]\ndependencies = ['checker-fixture==1.0', 'source-fixture @ file://%s']\n", repo.Root), "python dependencies")
	repo.CommitFile("project_module.py", "value = 'project ready'\n", "project import")
	repo.CommitFile("web/package.json", `{"name":"fixture","version":"1.0.0","dependencies":{"checker-fixture":"1.0.0"}}`, "javascript dependencies")
	repo.CommitFile("web/package-lock.json", fmt.Sprintf(`{"name":"fixture","version":"1.0.0","lockfileVersion":3,"packages":{"":{"name":"fixture","version":"1.0.0","dependencies":{"checker-fixture":"1.0.0"}},"node_modules/checker-fixture":{"version":"1.0.0","resolved":%q,"hasInstallScript":true}}}`, server.URL+"/package.tgz"), "javascript lock")
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
PYTHONPATH=. python -c 'import checker_fixture,project_module; print(checker_fixture.value); print(project_module.value)'
node -e "console.log(require('./web/node_modules/checker-fixture'))"
`, run.Repo)
	argv, env, cleanup, err := run.Job.Command(t.Context(), []string{"bash", "--norc", "--noprofile", "-c", probe})
	require.NoError(t, err)
	t.Cleanup(cleanup)
	cmd := exec.CommandContext(t.Context(), argv[0], argv[1:]...)
	cmd.Env = env
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s\n%s", output, log.String())
	assert.Contains(t, string(output), "python ready\nproject ready\n")
	assert.Contains(t, string(output), "javascript ready\n")
	assert.FileExists(t, filepath.Join(run.Repo, ".venv", "pth-ran"))
	assert.NoFileExists(t, marker)
}
