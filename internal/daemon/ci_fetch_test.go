package daemon

import (
	"context"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/testutil"
)

func TestCIFetchPrunesRenamedRemoteBranches(t *testing.T) {
	for _, tc := range []struct {
		name string
		old  string
		new  string
	}{
		{name: "parent replaced by child", old: "updates", new: "updates/dependency"},
		{name: "child replaced by parent", old: "updates/dependency", new: "updates"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			remote := testutil.InitTestRepo(t)
			remote.RunGit("branch", tc.old)
			local := testutil.NewTestRepo(t)
			local.RunGit("remote", "add", "origin", remote.Root)
			local.RunGit("config", "fetch.prune", "false")
			local.RunGit("fetch", "--quiet", "origin")
			local.RunGit("pack-refs", "--all")

			remote.RunGit("branch", "-D", tc.old)
			remote.RunGit("branch", tc.new)

			require.NoError(t, gitFetchCtx(context.Background(), local.Root, nil))
			assert.Equal(t, remote.HeadSHA(), local.RevParse("refs/remotes/origin/"+tc.new))
			err := exec.Command("git", "-C", local.Root, "show-ref", "--verify", "--quiet",
				"refs/remotes/origin/"+tc.old).Run()
			var exitErr *exec.ExitError
			require.ErrorAs(t, err, &exitErr)
			assert.Equal(t, 1, exitErr.ExitCode())
		})
	}
}
