//go:build integration

package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/testutil"
)

// Run with ROBOREV_TEST_CI_CONTAINER_IMAGE=node:24-bookworm. No provider or
// forge calls are made: a shell exercises the same boundary as the agent CLI.
func TestCIContainerBoundary(t *testing.T) {
	image := os.Getenv("ROBOREV_TEST_CI_CONTAINER_IMAGE")
	if image == "" {
		t.Skip("set ROBOREV_TEST_CI_CONTAINER_IMAGE to a local image with sh and git")
	}
	repo := testutil.NewTestRepoWithCommit(t)
	prepared := t.TempDir()
	snapshot := filepath.Join(prepared, "prompt.txt")
	require.NoError(t, os.WriteFile(snapshot, []byte("prepared prompt"), 0o600))
	private := filepath.Join(t.TempDir(), "credential")
	require.NoError(t, os.WriteFile(private, []byte("private sentinel"), 0o600))
	t.Setenv("ROBOREV_CI_AGENT_IMAGE", image)
	t.Setenv("ROBOREV_CI_REPO", repo.Path())
	t.Setenv("TMPDIR", prepared)
	t.Setenv("GH_TOKEN", "publishing-sentinel")
	t.Setenv("GITHUB_TOKEN", "publishing-sentinel")
	t.Setenv("SSH_AUTH_SOCK", private)
	t.Setenv("GIT_ASKPASS", private)
	t.Setenv("GIT_AUTHOR_NAME", "inherited-identity")
	t.Setenv("OPENAI_API_KEY", "provider-sentinel")
	t.Setenv("UNLISTED_SECRET", "other-sentinel")
	// A helper supplied by the caller must not reach the container.
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "credential.helper")
	t.Setenv("GIT_CONFIG_VALUE_0", "!echo password=helper-sentinel")

	for _, probe := range []bool{false, true} {
		name := "review and synthesis"
		if probe {
			name = "capability probe"
		}
		t.Run(name, func(t *testing.T) {
			cmd := exec.CommandContext(context.Background(), "sh", "-ec", `
git -C "$1" log -1 --format=%s
git -C "$1" diff HEAD
cat "$2"
test "$OPENAI_API_KEY" = provider-sentinel
test -z "$GH_TOKEN$GITHUB_TOKEN$SSH_AUTH_SOCK$GIT_ASKPASS$GIT_AUTHOR_NAME$UNLISTED_SECRET"
test ! -e "$3"
test ! -e /var/run/docker.sock
if git -C "$1" -c user.name=Fixture -c user.email=fixture@example.com commit --allow-empty -m forbidden; then exit 10; fi
if printf changed > "$2"; then exit 11; fi
if printf 'protocol=https\nhost=example.com\n\n' | git -C "$1" credential fill; then exit 12; fi
printf '\nboundary checked\n'
`, "probe", repo.Path(), snapshot, private)
			cmd.Dir = repo.Path()
			if probe {
				configureCapabilityProbe(cmd)
			} else {
				// Even adapters that normally retain GitHub auth must lose it.
				configureSubprocess(cmd, withGitHubCredentials())
			}
			out, err := cmd.CombinedOutput()
			require.NoError(t, err, "%s", out)
			assert.Contains(t, string(out), "prepared prompt")
			assert.Contains(t, string(out), "boundary checked")
			assert.NotContains(t, string(out), "helper-sentinel")
		})
	}
}
