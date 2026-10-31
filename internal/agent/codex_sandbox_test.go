package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/sandbox"
)

func TestCodexSandboxPrepareRepoAccess(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	work, read := t.TempDir(), t.TempDir()
	wantWork, err := sandbox.ResolvePath(work)
	require.NoError(t, err)
	repo := filepath.Join(read, "repo")
	require.NoError(t, os.Mkdir(repo, 0o700))
	for _, tc := range []struct {
		name      string
		repo      string
		readPaths []string
		wantError bool
	}{
		{name: "writable repo", repo: work},
		{name: "read-only repo", repo: repo, readPaths: []string{repo}},
		{name: "repo under read path", repo: repo, readPaths: []string{read}},
		{name: "ungranted repo", repo: repo, readPaths: []string{t.TempDir()}, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := CodexSandbox{Job: sandbox.Job{WorkDir: work, ReadPaths: tc.readPaths}}
			job, cleanup, err := s.prepare(tc.repo)
			if tc.wantError {
				require.ErrorContains(t, err, "sandbox repo must be inside job work dir or a read path")
				return
			}
			require.NoError(t, err)
			t.Cleanup(cleanup)
			assert.Equal(t, wantWork, job.WorkDir)
		})
	}
}

func TestCodexSandboxPrepareSymlinkedTMPDIR(t *testing.T) {
	tmp := t.TempDir()
	alias := filepath.Join(t.TempDir(), "tmp")
	require.NoError(t, os.Symlink(tmp, alias))
	t.Setenv("TMPDIR", alias)
	t.Run("repo access", TestCodexSandboxPrepareRepoAccess)
}
