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

				docs := filepath.Join(repo, "docs")
				require.NoError(t, os.MkdirAll(docs, 0o755))
				require.NoError(t, os.WriteFile(filepath.Join(docs, "index.md"), []byte("staged documentation\n"), 0o644))
				require.NoError(t, os.WriteFile(filepath.Join(docs, "zensical.toml"), []byte("docs_dir = \"docs\"\nsite_dir = \"site\"\n"), 0o644))
				bin := t.TempDir()
				realGit, err := exec.LookPath("git")
				require.NoError(t, err)
				realTar, err := exec.LookPath("tar")
				require.NoError(t, err)
				writeExecutable := func(name, script string) {
					require.NoError(t, os.WriteFile(filepath.Join(bin, name), []byte("#!/usr/bin/env bash\nset -euo pipefail\n"+script), 0o755))
				}
				// Keep the real archive and extractor. Only the producer adds
				// finite valid padding, larger than a pipe buffer, after the
				// archive's end markers. Failure cases omit this padding.
				writeExecutable("git", `
case " $* " in
  *" archive "*)
    "$DOCS_TEST_GIT" "$@"
    if [[ "$DOCS_TEST_MODE" == producer-failure ]]; then
      echo 'fixture producer failed' >&2
      exit 41
    fi
    if [[ "$DOCS_TEST_MODE" == padding ]]; then
      dd if=/dev/zero bs=1048576 count=8 2>/dev/null
    fi
    ;;
  *) exec "$DOCS_TEST_GIT" "$@" ;;
esac
`)
				writeExecutable("tar", `
case " $* " in
  *" -xf "*)
    if [[ "$DOCS_TEST_MODE" == extractor-failure ]]; then
      echo 'fixture extractor failed' >&2
      exit 42
    fi
    exec "$DOCS_TEST_TAR" "$@"
    ;;
  *)
    "$DOCS_TEST_TAR" "$@"
    if [[ "$DOCS_TEST_MODE" == producer-failure ]]; then
      echo 'fixture producer failed' >&2
      exit 41
    fi
    if [[ "$DOCS_TEST_MODE" == padding ]]; then
      dd if=/dev/zero bs=1048576 count=8 2>/dev/null
    fi
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
				scriptRel := filepath.Join("docs", "zensical-docs.sh")
				var args []string
				if flow == "hydrate" {
					scriptRel = filepath.Join("docs", "assets", "hydrate-assets.sh")
				} else {
					args = []string{"serve"}
				}
				scriptPath := installAssetScript(t, repo, scriptRel)
				cmd := exec.CommandContext(t.Context(), "bash", append([]string{bashPath(t, scriptPath)}, args...)...)
				cmd.Dir = repo
				cmd.Env = append(os.Environ(),
					"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
					"DOCS_TEST_GIT="+bashPath(t, realGit),
					"DOCS_TEST_TAR="+bashPath(t, realTar),
					"DOCS_TEST_MODE="+mode,
					"ROBOREV_DOCS_USE_LOCAL_ASSET_BRANCHES=true",
					"VIRTUAL_ENV=",
				)
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
					assert.NotContains(t, string(output), "zensical reached")
					return
				}
				require.NoError(t, err, string(output))
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
					stagingPaths, err := filepath.Glob(filepath.Join(docs, "zensical-public-docs.*"))
					require.NoError(t, err)
					assert.Empty(t, stagingPaths)
				}
			})
		}
	}
}
