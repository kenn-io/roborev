package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// Removing either consumer's drain must fail the padded-stream case.
func TestDocsArchiveStreams(t *testing.T) {
	for _, flow := range []string{"hydrate", "stage"} {
		t.Run(flow+"/padding", func(t *testing.T) {
			repo := t.TempDir()
			docs := filepath.Join(repo, "docs")
			bin := t.TempDir()
			writeExecutable := func(name, script string) {
				require.NoError(t, os.WriteFile(filepath.Join(bin, name), []byte("#!/usr/bin/env bash\nset -euo pipefail\n"+script), 0o755))
			}
			// Keep real archives and extraction. Eight MiB of legal zero
			// padding amplifies the normal producer tail beyond pipe capacity
			// so the producer outlives extraction deterministically. This is
			// fixture amplification, not normal producer padding.
			producerTail := `
dd if=/dev/zero bs=1048576 count=8 2>/dev/null
`
			env := os.Environ()
			scriptRel := filepath.Join("docs", "zensical-docs.sh")
			var args []string
			if flow == "hydrate" {
				git(t, repo, "init")
				git(t, repo, "config", "user.name", "Test User")
				git(t, repo, "config", "user.email", "test@example.invalid")
				writeStaticAssets(t, repo, "static asset")
				git(t, repo, "add", ".")
				git(t, repo, "commit", "-m", "static assets")
				git(t, repo, "branch", "docs-assets")
				git(t, repo, "rm", "-r", ".")
				writeGeneratedAssets(t, repo, "generated asset")
				git(t, repo, "add", ".")
				git(t, repo, "commit", "-m", "generated assets")
				git(t, repo, "branch", "docs-generated-assets")
				realGit, err := exec.LookPath("git")
				require.NoError(t, err)
				env = append(env, "DOCS_TEST_GIT="+bashPath(t, realGit), "ROBOREV_DOCS_USE_LOCAL_ASSET_BRANCHES=true")
				writeExecutable("git", `
case " $* " in
  *" archive "*)
    "$DOCS_TEST_GIT" "$@"
`+producerTail+`
    ;;
  *) exec "$DOCS_TEST_GIT" "$@" ;;
esac
`)
				scriptRel = filepath.Join("docs", "assets", "hydrate-assets.sh")
			} else {
				realTar, err := exec.LookPath("tar")
				require.NoError(t, err)
				env = append(env, "DOCS_TEST_TAR="+bashPath(t, realTar))
				require.NoError(t, os.MkdirAll(docs, 0o755))
				require.NoError(t, os.WriteFile(filepath.Join(docs, "zensical.toml"), []byte("docs_dir = \"docs\"\nsite_dir = \"site\"\n"), 0o644))
				writeExecutable("tar", `
case "$1" in
  -xf)
    exec "$DOCS_TEST_TAR" "$@"
    ;;
  *)
    "$DOCS_TEST_TAR" "$@"
`+producerTail+`
    ;;
esac
`)
				// Exercise serve's staging without launching a real server.
				writeExecutable("zensical", "")
				env = append(env, "VIRTUAL_ENV=")
				args = []string{"serve"}
			}
			scriptPath := installAssetScript(t, repo, scriptRel)
			// Git for Windows prepends its tools when Bash starts, so put
			// the fixture wrappers first from inside the running shell.
			cmd := exec.CommandContext(t.Context(), "bash", append([]string{
				"-c", `export PATH="$1:$PATH"; shift; exec bash "$@"`,
				"docs-archive-test", bashPath(t, bin), bashPath(t, scriptPath),
			}, args...)...)
			cmd.Dir = repo
			cmd.Env = env
			output, err := cmd.CombinedOutput()
			require.NoError(t, err, string(output))
		})
	}
}
