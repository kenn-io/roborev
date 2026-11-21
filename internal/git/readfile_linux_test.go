package git

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestReadFileCancelsRunningGit(t *testing.T) {
	repo := NewTestRepoWithCommit(t)
	repo.CommitFile("blocked.txt", "content", "add file")
	oid := strings.TrimSpace(repo.Run("rev-parse", "HEAD:blocked.txt"))
	object := filepath.Join(repo.Dir, ".git", "objects", oid[:2], oid[2:])
	// Keep Git blocked while it reads the object from a pipe.
	require.NoError(t, os.Remove(object))
	require.NoError(t, unix.Mkfifo(object, 0600))

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		_, err := ReadFile(ctx, repo.Dir, "HEAD", "blocked.txt")
		done <- err
	}()
	t.Cleanup(func() {
		cancel()
		<-finished
	})

	var writer int
	require.Eventually(t, func() bool {
		var err error
		writer, err = unix.Open(object, unix.O_WRONLY|unix.O_NONBLOCK, 0)
		return err == nil
	}, 5*time.Second, 10*time.Millisecond, "git must open the object before cancellation")
	defer unix.Close(writer)

	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "git read did not stop after cancellation")
	}
}
