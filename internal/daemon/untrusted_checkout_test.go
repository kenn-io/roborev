package daemon

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
	"go.kenn.io/roborev/internal/tokens"
)

// TestProcessJobCIReviewSkipsProjectAgentConfig runs a CI and a local review
// through a fake Claude and checks which setting sources each was given. CI
// reviews run in the pull request checkout, whose .claude settings and
// .mcp.json the pull request author controls.
func TestProcessJobCIReviewSkipsProjectAgentConfig(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fake command uses POSIX permissions")
	}
	for _, tt := range []struct {
		name       string
		baseBranch string
		untrusted  bool
	}{
		{name: "ci", baseBranch: "main", untrusted: true},
		{name: "local"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tc := newWorkerTestContext(t, 1)
			bin := t.TempDir()
			argsFile := filepath.Join(bin, "args")
			stream := filepath.Join(bin, "stream.jsonl")
			require.NoError(t, os.WriteFile(stream, []byte(
				`{"type":"result","result":"{\"schema_version\":3,\"summary\":\"No findings.\",\"verdict\":\"pass\",\"findings\":[]}"}`+"\n",
			), 0o600))
			claude := filepath.Join(bin, "claude")
			require.NoError(t, os.WriteFile(claude, []byte(`#!/bin/sh
case "$1" in
  *etxtbsy*) exit 0 ;;
  --help) echo '--tools --effort'; exit 0 ;;
esac
for arg do printf '%s\n' "$arg"; done > '`+argsFile+`'
cat '`+stream+`'
`), 0o700))
			cfg := config.DefaultConfig()
			cfg.ClaudeCodeCmd = claude
			tc.reconfigurePool(cfg)
			tc.Pool.tokenUsageFetcher = func(context.Context, string) (*tokens.Usage, error) {
				return nil, tokens.ErrUsageProviderUnavailable
			}
			sha := testutil.GetHeadSHA(t, tc.TmpDir)
			commit, err := tc.DB.GetOrCreateCommit(tc.Repo.ID, sha, "Author", "Subject", time.Now())
			require.NoError(t, err)
			_, err = tc.DB.EnqueueJob(storage.EnqueueOpts{
				RepoID: tc.Repo.ID, CommitID: commit.ID, GitRef: sha, Agent: "claude-code",
				CIBaseBranch: tt.baseBranch,
			})
			require.NoError(t, err)
			claimed, err := tc.DB.ClaimJob(testWorkerID)
			require.NoError(t, err)

			tc.Pool.processJob(testWorkerID, claimed)

			tc.assertJobStatus(t, claimed.ID, storage.JobStatusDone)
			data, err := os.ReadFile(argsFile)
			require.NoError(t, err)
			args := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
			sources := slices.Index(args, "--setting-sources")
			if tt.untrusted {
				require.NotEqual(t, -1, sources, "args: %v", args)
				require.Less(t, sources+1, len(args))
				assert.Equal(t, "user", args[sources+1])
				assert.Contains(t, args, "--strict-mcp-config")
			} else {
				assert.Equal(t, -1, sources, "args: %v", args)
				assert.NotContains(t, args, "--strict-mcp-config")
			}
		})
	}
}
