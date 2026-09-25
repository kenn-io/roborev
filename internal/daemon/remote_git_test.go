package daemon

import (
	"bytes"
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/testutil"
)

const (
	shaA = "0123456789abcdef0123456789abcdef01234567"
	shaB = "89abcdef0123456789abcdef0123456789abcdef"
)

func TestParseRemoteGitRef(t *testing.T) {
	tests := []struct {
		ref     string
		want    []string
		wantErr bool
	}{
		{ref: shaA, want: []string{shaA}},
		{ref: shaA + ".." + shaB, want: []string{shaA, shaB}},
		{ref: shaA + "^.." + shaB, want: []string{shaA, shaB}},
		{ref: "HEAD", wantErr: true},
		{ref: "main.." + shaB, wantErr: true},
		{ref: shaA[:12], wantErr: true},
		{ref: "dirty", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.ref, func(t *testing.T) {
			got, err := parseRemoteGitRef(tt.ref)
			if tt.wantErr {
				require.ErrorIs(t, err, errRemoteRefNotSHA)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// gitOut runs git in dir with a fixed test identity and returns trimmed
// stdout. The fixtures use plain directories because testutil.InitTestGitRepo
// copies a template into its directory and would overwrite a clone.
func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-c", "user.name=test", "-c", "user.email=test@example.com", "-C", dir}, args...)
	out, err := exec.Command("git", full...).CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
	return strings.TrimSpace(string(out))
}

// remoteGitFixture has a bare upstream, a daemon clone of it, and a laptop
// clone with one extra unpushed commit.
type remoteGitFixture struct {
	daemonDir, laptopDir string
	unpushed             string
}

func newRemoteGitFixture(t *testing.T) remoteGitFixture {
	t.Helper()
	root := t.TempDir()
	upstream := filepath.Join(root, "upstream.git")
	gitOut(t, root, "init", "-q", "--bare", "-b", "main", upstream)
	seed := testutil.NewGitRepo(t)
	seed.CommitFile("base.txt", "base", "base")
	gitOut(t, seed.Path(), "push", "-q", upstream, "HEAD:refs/heads/main")

	f := remoteGitFixture{
		daemonDir: filepath.Join(root, "daemon"),
		laptopDir: filepath.Join(root, "laptop"),
	}
	gitOut(t, root, "clone", "-q", upstream, f.daemonDir)
	gitOut(t, root, "clone", "-q", upstream, f.laptopDir)
	gitOut(t, f.laptopDir, "commit", "-q", "--allow-empty", "-m", "unpushed")
	f.unpushed = gitOut(t, f.laptopDir, "rev-parse", "HEAD")
	return f
}

func buildPack(t *testing.T, dir string, tips, exclude []string) []byte {
	t.Helper()
	var revs strings.Builder
	for _, sha := range tips {
		revs.WriteString(sha + "\n")
	}
	for _, sha := range exclude {
		revs.WriteString("^" + sha + "\n")
	}
	cmd := exec.Command("git", "-C", dir, "pack-objects", "--revs", "--stdout", "-q")
	cmd.Stdin = strings.NewReader(revs.String())
	out, err := cmd.Output()
	require.NoError(t, err)
	return out
}

func TestEnsureRemoteCommitsFetchesMissing(t *testing.T) {
	f := newRemoteGitFixture(t)
	ctx := context.Background()

	missing, err := ensureRemoteCommits(ctx, f.daemonDir, []string{f.unpushed})
	require.NoError(t, err)
	assert.Equal(t, []string{f.unpushed}, missing, "unpushed commit stays missing")

	gitOut(t, f.laptopDir, "push", "-q", "origin", "HEAD:refs/heads/main")
	missing, err = ensureRemoteCommits(ctx, f.daemonDir, []string{f.unpushed})
	require.NoError(t, err)
	assert.Empty(t, missing, "fetch picks up the pushed commit")
}

func TestMissingCommitsReportsBrokenRepo(t *testing.T) {
	_, err := missingCommits(context.Background(), t.TempDir(), []string{shaA})
	require.Error(t, err, "a directory that is not a repo is an error, not a missing commit")
}

func TestDaemonHavesPeelsTags(t *testing.T) {
	f := newRemoteGitFixture(t)
	head := gitOut(t, f.daemonDir, "rev-parse", "HEAD")
	gitOut(t, f.daemonDir, "tag", "-a", "v1", "-m", "annotated")
	tree := gitOut(t, f.daemonDir, "rev-parse", "HEAD^{tree}")
	gitOut(t, f.daemonDir, "tag", "tree-tag", tree)

	haves, err := daemonHaves(context.Background(), f.daemonDir)
	require.NoError(t, err)
	assert.Contains(t, haves, head)
	assert.NotContains(t, haves, tree)
	for _, sha := range haves {
		assert.Equal(t, "commit", gitOut(t, f.daemonDir, "cat-file", "-t", sha))
	}
}

func TestImportPackPinsTips(t *testing.T) {
	f := newRemoteGitFixture(t)
	ctx := context.Background()
	haves, err := daemonHaves(ctx, f.daemonDir)
	require.NoError(t, err)

	pack := buildPack(t, f.laptopDir, []string{f.unpushed}, haves)
	require.NoError(t, importPack(ctx, f.daemonDir, bytes.NewReader(pack), []string{f.unpushed}))

	assert.Equal(t, f.unpushed, gitOut(t, f.daemonDir, "rev-parse", uploadRefPrefix+f.unpushed))
	missing, err := ensureRemoteCommits(ctx, f.daemonDir, []string{f.unpushed})
	require.NoError(t, err)
	assert.Empty(t, missing)
}

func TestImportPackMissingBase(t *testing.T) {
	f := newRemoteGitFixture(t)
	gitOut(t, f.laptopDir, "commit", "-q", "--allow-empty", "-m", "second unpushed")
	second := gitOut(t, f.laptopDir, "rev-parse", "HEAD")
	// Exclude the first unpushed commit, which the daemon does not have.
	pack := buildPack(t, f.laptopDir, []string{second}, []string{f.unpushed})

	err := importPack(context.Background(), f.daemonDir, bytes.NewReader(pack), []string{second})
	var baseErr *missingBaseError
	require.ErrorAs(t, err, &baseErr)
	assert.Contains(t, err.Error(), "daemon clone lacks base commits for "+second)
	require.Error(t, baseErr.err, "git's report is kept for logging")
	assert.Empty(t, gitOut(t, f.daemonDir, "for-each-ref", uploadRefPrefix))
}

func TestPruneUploadRefsAfterPush(t *testing.T) {
	f := newRemoteGitFixture(t)
	ctx := context.Background()
	haves, err := daemonHaves(ctx, f.daemonDir)
	require.NoError(t, err)
	pack := buildPack(t, f.laptopDir, []string{f.unpushed}, haves)
	require.NoError(t, importPack(ctx, f.daemonDir, bytes.NewReader(pack), []string{f.unpushed}))

	gitOut(t, f.laptopDir, "push", "-q", "origin", "HEAD:refs/heads/main")
	// The daemon prunes upload refs after the fetch it runs for a remote
	// enqueue of a missing commit.
	fetchRemotes(ctx, f.daemonDir)
	assert.Empty(t, gitOut(t, f.daemonDir, "for-each-ref", uploadRefPrefix))
}

func TestImportPackLeavesCheckoutUnchanged(t *testing.T) {
	f := newRemoteGitFixture(t)
	ctx := context.Background()
	snapshot := func() []string {
		return []string{
			gitOut(t, f.daemonDir, "for-each-ref", "refs/heads", "refs/remotes"),
			gitOut(t, f.daemonDir, "rev-parse", "HEAD"),
			gitOut(t, f.daemonDir, "status", "--porcelain"),
		}
	}
	before := snapshot()
	haves, err := daemonHaves(ctx, f.daemonDir)
	require.NoError(t, err)
	pack := buildPack(t, f.laptopDir, []string{f.unpushed}, haves)
	require.NoError(t, importPack(ctx, f.daemonDir, bytes.NewReader(pack), []string{f.unpushed}))
	assert.Equal(t, before, snapshot())
}

func TestImportPackRejectsNonSHATip(t *testing.T) {
	f := newRemoteGitFixture(t)
	err := importPack(context.Background(), f.daemonDir, strings.NewReader(""), []string{"HEAD"})
	require.ErrorIs(t, err, errRemoteRefNotSHA)
}
