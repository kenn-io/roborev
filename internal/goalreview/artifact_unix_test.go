//go:build unix

package goalreview

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/kata"
	"go.kenn.io/roborev/internal/kata/katatest"
)

func TestFIFOArtifactDoesNotBlockCapture(t *testing.T) {
	root := t.TempDir()
	fifo := filepath.Join(root, "fifo.md")
	require.NoError(t, syscall.Mkfifo(fifo, 0o600))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := Capture(ctx, root, Selection{SpecFile: "fifo.md"}, &katatest.FakeClient{BindingErr: kata.ErrNoBinding})
		done <- err
	}()
	returned := false
	select {
	case <-done:
		returned = true
	case <-time.After(150 * time.Millisecond):
	}
	if !returned {
		writer, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0o600)
		require.NoError(t, err)
		require.NoError(t, writer.Close())
		unblocked := false
		select {
		case <-done:
			unblocked = true
		case <-time.After(time.Second):
		}
		require.True(t, unblocked, "capture failed to exit after FIFO unblocked")
	}
	assert.True(t, returned, "non-regular artifact blocks cancellation until a separate writer opens it")
}
