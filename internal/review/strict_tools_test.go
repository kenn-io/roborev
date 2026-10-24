package review

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/testutil"
)

func TestStrictReviewDirectDispatch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("deterministic POSIX CLI fixture")
	}
	for _, synthesis := range []bool{false, true} {
		for _, fail := range []bool{false, true} {
			t.Run(fmt.Sprintf("synthesis=%t/fail=%t", synthesis, fail), func(t *testing.T) {
				repo := testutil.NewGitRepo(t)
				sha := repo.CommitFile("draft.md", "Draft\n", "draft")
				require.NoError(t, os.WriteFile(filepath.Join(repo.Path(), "review.md"), []byte("Check citations."), 0o600))
				dir := t.TempDir()
				command := filepath.Join(dir, "claude-fixture")
				capture := filepath.Join(dir, "config-path")
				exit := 0
				if fail {
					exit = 1
				}
				script := fmt.Sprintf(`#!/bin/sh
if [ "$1" = "--help" ]; then
 echo '--restricted --tools --strict-mcp-config --mcp-config --permission-mode --permission-prompts --allowedTools --no-session-persistence --disable-slash-commands --effort --json-schema'
 exit 0
fi
previous=''
for arg do
 if [ "$previous" = '--mcp-config' ]; then
  test -f "$arg" || exit 2
  printf '%%s' "$arg" > %q
 fi
 previous="$arg"
done
cat >/dev/null
echo '{"type":"system","subtype":"init","permissionMode":"dontAsk","tools":["Read","Glob","Grep","StructuredOutput"],"mcp_servers":[]}'
echo '{"type":"result","result":"{\"schema_version\":2,\"summary\":\"No findings.\",\"verdict\":\"pass\",\"findings\":[]}"}'
exit %d
`, capture, exit)
				require.NoError(t, os.WriteFile(command, []byte(script), 0o700))
				policy := &config.ReviewToolPolicy{Mode: "read-only"}
				cfg := config.DefaultConfig()
				cfg.Review.Types = map[string]config.ReviewTypeSpec{"citations": {Template: "review.md", Tools: policy}}
				a := agent.NewClaudeAgent(command).WithSessionID("old-session")
				if synthesis {
					_, err := RunSynthesisAgent(context.Background(), a, nil, "Combine reviews.", "", nil, SynthesisHooks{
						Tools: policy, GlobalConfig: cfg, ConfigRepoPath: repo.Path(),
						BeforeInvoke: func(prepared agent.Agent) {
							assert.Contains(t, prepared.CommandLine(), "--restricted")
							assert.NotContains(t, prepared.CommandLine(), "--resume")
						},
						Checkout: func() (SynthesisCheckout, error) { return SynthesisCheckout{RepoPath: repo.Path(), GitRef: sha}, nil },
					})
					if fail {
						require.Error(t, err)
					} else {
						require.NoError(t, err)
					}
				} else {
					result := runSingle(context.Background(), BatchConfig{RepoPath: repo.Path(), GitRef: sha, GlobalConfig: cfg, AgentRegistry: map[string]agent.Agent{"claude-code": a}}, "claude-code", "citations")
					if fail {
						assert.Equal(t, ResultFailed, result.Status)
					} else {
						assert.Equal(t, ResultDone, result.Status, result.Error)
					}
				}
				data, err := os.ReadFile(capture)
				require.NoError(t, err, "strict prepared config must reach actual adapter")
				require.NotEmpty(t, data)
				assert.NotContains(t, string(data), repo.Path())
				_, err = os.Stat(string(data))
				require.ErrorIs(t, err, os.ErrNotExist, "config must be cleaned after success and failure")
			})
		}
	}
}

func TestStrictSynthesisConfigCleanupOnCancel(t *testing.T) {
	command, err := os.Executable()
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var path string
	_, err = RunSynthesisAgent(ctx, agent.NewClaudeAgent(command), nil, "Combine reviews.", "", nil, SynthesisHooks{
		Tools: &config.ReviewToolPolicy{Mode: "read-only"},
		BeforeInvoke: func(prepared agent.Agent) {
			_, rest, _ := strings.Cut(prepared.CommandLine(), "--mcp-config ")
			path, _, _ = strings.Cut(rest, " --permission-mode")
			require.NotEmpty(t, path)
			assert.FileExists(t, path)
			cancel()
		},
	})
	require.ErrorIs(t, err, context.Canceled)
	require.NotEmpty(t, path)
	_, err = os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist)
}
