package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Removing either consumer's drain must fail the padded-stream cases, while
// ignoring a failed producer or extractor must fail the corresponding cases.
func TestDocsArchiveStreams(t *testing.T) {
	for _, flow := range []string{"hydrate", "stage"} {
		for _, mode := range []string{"padding", "producer-failure", "extractor-failure"} {
			t.Run(flow+"/"+mode, func(t *testing.T) {
				repo := t.TempDir()
				docs := filepath.Join(repo, "docs")
				bin := t.TempDir()
				realTar, err := exec.LookPath("tar")
				require.NoError(t, err)
				writeExecutable := func(name, script string) {
					require.NoError(t, os.WriteFile(filepath.Join(bin, name), []byte("#!/usr/bin/env bash\nset -euo pipefail\n"+script), 0o755))
				}
				// Keep real archives and extraction. Eight MiB of legal zero
				// padding amplifies the normal producer tail beyond pipe capacity
				// so the producer outlives extraction deterministically. This is
				// fixture amplification, not normal producer padding. Failure
				// cases omit it so they test error propagation independently.
				producerTail := `
if [[ "$DOCS_TEST_MODE" == producer-failure ]]; then
  echo 'fixture producer failed' >&2
  exit 41
fi
if [[ "$DOCS_TEST_MODE" == padding ]]; then
  dd if=/dev/zero bs=1048576 count=8 2>/dev/null
  echo 'fixture padding completed' >&2
fi
`
				extractor := `
if [[ "$DOCS_TEST_MODE" == extractor-failure ]]; then
  echo 'fixture extractor failed' >&2
  exit 42
fi
exec "$DOCS_TEST_TAR" "$@"
`
				env := append(os.Environ(),
					"DOCS_TEST_TAR="+bashPath(t, realTar),
					"DOCS_TEST_MODE="+mode,
				)
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
					writeExecutable("tar", extractor)
					scriptRel = filepath.Join("docs", "assets", "hydrate-assets.sh")
				} else {
					require.NoError(t, os.MkdirAll(docs, 0o755))
					require.NoError(t, os.WriteFile(filepath.Join(docs, "index.md"), []byte("staged documentation\n"), 0o644))
					require.NoError(t, os.WriteFile(filepath.Join(docs, "zensical.toml"), []byte("docs_dir = \"docs\"\nsite_dir = \"site\"\n"), 0o644))
					writeExecutable("tar", `
case " $* " in
  *" -xf "*)
`+extractor+`
    ;;
  *)
    "$DOCS_TEST_TAR" "$@"
`+producerTail+`
    ;;
esac
`)
					// Exercise serve's staging without launching a real server.
					writeExecutable("zensical", `
[[ "$1" == serve && "$2" == --config-file ]]
staged=$(sed -n 's/^docs_dir = "\(.*\)"$/\1/p' "$3")
cat "$staged/index.md"
printf 'zensical reached\n'
`)
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
				if mode != "padding" {
					require.Error(t, err, string(output))
					var exitError *exec.ExitError
					require.ErrorAs(t, err, &exitError)
					wantCode := 41
					if mode == "extractor-failure" {
						wantCode = 42
					}
					assert.Equal(t, wantCode, exitError.ExitCode(), string(output))
					assert.Contains(t, string(output), "fixture "+strings.TrimSuffix(mode, "-failure")+" failed")
					if flow == "stage" {
						assert.NotContains(t, string(output), "zensical reached")
					}
					return
				}
				require.NoError(t, err, string(output))
				assert.Contains(t, string(output), "fixture padding completed")
				if flow == "hydrate" {
					for path, want := range map[string]string{
						"static/logo.svg": "static asset\n", "generated/tui-hero.svg": "generated asset\n",
					} {
						content, err := os.ReadFile(filepath.Join(docs, "assets", path))
						require.NoError(t, err)
						assert.Equal(t, want, string(content))
					}
				} else {
					assert.Contains(t, string(output), "staged documentation\nzensical reached\n")
				}
			})
		}
	}
}
