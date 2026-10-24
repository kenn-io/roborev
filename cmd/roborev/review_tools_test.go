package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/testutil"
)

func TestLocalReviewToolPolicyRejectsUnsupported(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	marker := filepath.Join(t.TempDir(), "invoked")
	executable := filepath.Join(t.TempDir(), "adapter-fixture")
	script := "#!/bin/sh\nprintf invoked > '" + marker + "'\nexit 1\n"
	if runtime.GOOS == "windows" {
		executable += ".cmd"
		script = "@echo off\r\necho invoked > \"" + marker + "\"\r\nexit /b 1\r\n"
	}
	require.NoError(t, os.WriteFile(executable, []byte(script), 0o700))
	cfg := config.DefaultConfig()
	cfg.CodexCmd = executable
	require.NoError(t, config.SaveGlobal(cfg))
	repo := testutil.NewGitRepo(t)
	sha := repo.CommitFile("draft.md", "Draft\n", "draft")
	require.NoError(t, os.WriteFile(filepath.Join(repo.Path(), ".roborev.toml"), []byte("[review.types.citations]\ntemplate='review.md'\n[review.types.citations.tools]\nmode='read-only'\n"), 0o600))
	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())
	cmd.SetOut(io.Discard)
	err := runLocalReview(cmd, repo.Path(), sha, "", nil, "codex", "", "", "standard", "citations", true, "")
	require.Error(t, err)
	// The exact primary rejection must precede any native CLI invocation.
	require.ErrorContains(t, err, "does not support strict review tools")
	require.NoFileExists(t, marker)
}

func TestLocalStrictReview(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("deterministic POSIX CLI fixture")
	}
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	dir := t.TempDir()
	command := filepath.Join(dir, "claude-fixture")
	capture := filepath.Join(dir, "mcp-path")
	script := fmt.Sprintf(`#!/bin/sh
if [ "$1" = '--help' ]; then
 echo '--restricted --tools --strict-mcp-config --mcp-config --permission-mode --permission-prompts --allowedTools --no-session-persistence --disable-slash-commands --json-schema'
 exit 0
fi
previous=''
for arg do
 if [ "$previous" = '--mcp-config' ]; then
  test -f "$arg" || exit 1
  printf '%%s' "$arg" > %q
 fi
 previous="$arg"
done
cat >/dev/null
echo '{"type":"system","subtype":"init","permissionMode":"dontAsk","tools":["Read","Glob","Grep","StructuredOutput"],"mcp_servers":[]}'
echo '{"type":"result","result":"{\"schema_version\":2,\"summary\":\"No findings.\",\"verdict\":\"pass\",\"findings\":[]}"}'
`, capture)
	require.NoError(t, os.WriteFile(command, []byte(script), 0o700))
	cfg := config.DefaultConfig()
	cfg.ClaudeCodeCmd = command
	require.NoError(t, config.SaveGlobal(cfg))
	repo := testutil.NewGitRepo(t)
	repo.CommitFile("review.md", "Check citations.\n", "add rubric")
	sha := repo.CommitFile("draft.md", "Draft.\n", "add draft")
	require.NoError(t, os.WriteFile(filepath.Join(repo.Path(), ".roborev.toml"), []byte("[review.types.citations]\ntemplate='review.md'\n[review.types.citations.tools]\nmode='read-only'\n"), 0o600))
	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())
	cmd.SetOut(io.Discard)
	require.NoError(t, runLocalReview(cmd, repo.Path(), sha, "", nil, "claude-code", "", "", "standard", "citations", true, ""))
	data, err := os.ReadFile(capture)
	require.NoError(t, err)
	require.NotEmpty(t, data)
	_, err = os.Stat(string(data))
	require.ErrorIs(t, err, os.ErrNotExist)
}
