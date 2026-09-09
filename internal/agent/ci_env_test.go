package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/testutil"
)

func TestCIReviewGitCapabilities(t *testing.T) {
	skipIfWindows(t)
	repo := testutil.NewTestRepoWithCommit(t)
	remote := testutil.NewTestRepoWithCommit(t)
	repo.RunGit("push", remote.Path(), "HEAD:refs/heads/local-probe")
	helper := writeTempCommand(t, "#!/bin/sh\nprintf 'username=fixture\\npassword=helper-sentinel\\n'\n")
	// Both environment configuration and repository configuration must lose
	// their helper. Local foreground commands still use the fixture helper.
	repo.RunGit("config", "credential.helper", helper)
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "credential.helper")
	t.Setenv("GIT_CONFIG_VALUE_0", helper)
	t.Setenv("GIT_AUTHOR_NAME", "Inherited author")
	t.Setenv("GIT_AUTHOR_EMAIL", "author@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "Inherited committer")
	t.Setenv("GIT_COMMITTER_EMAIL", "committer@example.com")
	t.Setenv("GIT_ASKPASS", helper)
	t.Setenv("SSH_ASKPASS", helper)
	t.Setenv("SSH_AUTH_SOCK", filepath.Join(t.TempDir(), "agent.sock"))
	ciCtx, cleanup, err := WithCIReview(context.Background())
	require.NoError(t, err)
	t.Cleanup(cleanup)
	credentialInput := "protocol=https\nhost=example.com\n\n"
	for _, ci := range []bool{false, true} {
		name := "local"
		ctx := context.Background()
		if ci {
			name = "ci"
			ctx = ciCtx
		}
		t.Run(name, func(t *testing.T) {
			cmd := exec.CommandContext(ctx, "git", "credential", "fill")
			cmd.Dir = repo.Path()
			cmd.Stdin = strings.NewReader(credentialInput)
			configureSubprocess(ctx, cmd)
			out, err := cmd.CombinedOutput()
			if ci {
				require.Error(t, err)
				assert.NotContains(t, string(out), "helper-sentinel")
			} else {
				require.NoError(t, err, "%s", out)
				assert.Contains(t, string(out), "password=helper-sentinel")
			}
		})
	}
	ctx := ciCtx
	for _, args := range [][]string{{"log", "-1", "--oneline"}, {"diff", "HEAD"}, {"status", "--porcelain"}} {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = repo.Path()
		configureSubprocess(ctx, cmd)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "%v: %s", args, out)
	}
	for _, args := range [][]string{
		{"commit", "--allow-empty", "-m", "should fail"},
		{"push", remote.Path(), "HEAD:refs/heads/ci-probe"},
	} {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = repo.Path()
		configureSubprocess(ctx, cmd)
		out, err := cmd.CombinedOutput()
		require.Error(t, err, "%v unexpectedly succeeded: %s", args, out)
	}
}

func TestCIAgentEnvironment(t *testing.T) {
	skipIfWindows(t)
	for _, factory := range []func(string) Agent{
		func(p string) Agent { return NewCodexAgent(p) },
		func(p string) Agent { return NewClaudeAgent(p) },
		func(p string) Agent { return NewGeminiAgent(p) },
		func(p string) Agent { return NewCopilotAgent(p) },
		func(p string) Agent { return NewOpenCodeAgent(p) },
		func(p string) Agent { return NewCursorAgent(p) },
		func(p string) Agent { return NewKiroAgent(p) },
		func(p string) Agent { return NewKiloAgent(p) },
		func(p string) Agent { return NewDroidAgent(p) },
		func(p string) Agent { return NewGrokAgent(p) },
	} {
		t.Run(factory("").Name(), func(t *testing.T) {
			capture := filepath.Join(t.TempDir(), "env")
			t.Setenv("CI_ENV_CAPTURE", capture)
			t.Setenv("GH_TOKEN", "publisher-sentinel")
			t.Setenv("GITHUB_TOKEN", "publisher-sentinel")
			t.Setenv("OPENAI_API_KEY", "provider-sentinel")
			t.Setenv("COPILOT_GITHUB_TOKEN", "copilot-provider-sentinel")
			t.Setenv("GIT_ASKPASS", "askpass-sentinel")
			t.Setenv("SSH_AUTH_SOCK", "ssh-sentinel")
			command := writeTempCommand(t, `#!/bin/sh
for arg in "$@"; do
  case "$arg" in *etxtbsy*) exit 0;; esac
  if [ "$arg" = --help ]; then
    echo '--sandbox --full-auto --tools --effort --allow-all-tools --stream --output-format --disable-builtin-mcps'
    exit 0
  fi
done
env > "$CI_ENV_CAPTURE"
echo '> No issues found.'
`)
			ctx, cleanup, err := WithCIReview(context.Background())
			require.NoError(t, err)
			t.Cleanup(cleanup)
			_, runErr := factory(command).Review(ctx, t.TempDir(), "HEAD", "review", nil)
			// The fixture only captures environment; each CLI's output parser
			// has its own tests. Require that the actual subprocess ran.
			out, err := os.ReadFile(capture)
			require.NoError(t, err, "agent launch: %v", runErr)
			assert := assert.New(t)
			assert.NotContains(string(out), "publisher-sentinel")
			assert.NotContains(string(out), "askpass-sentinel")
			assert.NotContains(string(out), "ssh-sentinel")
			assert.Contains(string(out), "OPENAI_API_KEY=provider-sentinel")
			assert.Contains(string(out), "COPILOT_GITHUB_TOKEN=copilot-provider-sentinel")
			assert.Equal("publisher-sentinel", os.Getenv("GH_TOKEN"), "parent keeps publishing auth")
			if structured, ok := factory(command).(StructuredReviewAgent); ok {
				require.NoError(t, os.Remove(capture))
				_, runErr = structured.ReviewWithSchema(ctx, t.TempDir(), "HEAD", "review", []byte(`{"type":"object"}`), nil)
				out, err = os.ReadFile(capture)
				require.NoError(t, err, "structured agent launch: %v", runErr)
				assert.NotContains(string(out), "publisher-sentinel")
				assert.NotContains(string(out), "askpass-sentinel")
			}
		})
	}
}

func TestCIProbeEnvironment(t *testing.T) {
	skipIfWindows(t)
	t.Setenv("GH_TOKEN", "publisher-sentinel")
	t.Setenv("GIT_ASKPASS", "askpass-sentinel")
	t.Setenv("SSH_AUTH_SOCK", "ssh-sentinel")
	t.Setenv("OPENAI_API_KEY", "provider-sentinel")
	ctx, cleanup, err := WithCIReview(context.Background())
	require.NoError(t, err)
	t.Cleanup(cleanup)
	cmd := exec.CommandContext(ctx, "sh", "-c", "env")
	configureCapabilityProbe(ctx, cmd)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err)
	assert := assert.New(t)
	assert.NotContains(string(out), "publisher-sentinel")
	assert.NotContains(string(out), "askpass-sentinel")
	assert.NotContains(string(out), "ssh-sentinel")
	assert.Contains(string(out), "OPENAI_API_KEY=provider-sentinel")
	command := writeTempCommand(t, `#!/bin/sh
case "$1" in *etxtbsy*) exit 0;; esac
test -z "$GH_TOKEN$GIT_ASKPASS$SSH_AUTH_SOCK" || exit 1
echo 'fixture version'
`)
	assert.Equal(identityNotGrok, probeVersionIdentity(command), "identity probes do not inherit a review context")
}
