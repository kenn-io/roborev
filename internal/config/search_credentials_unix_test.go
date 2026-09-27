//go:build !windows

package config

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEmbeddingCredentialRejectsFIFOWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key.pipe")
	require.NoError(t, syscall.Mkfifo(path, 0o600))
	type result struct {
		credential EmbeddingCredential
		err        error
	}
	done := make(chan result, 1)
	go func() {
		credential, err := (EmbeddingConfig{APIKeyFile: path}).ResolveCredential()
		done <- result{credential: credential, err: err}
	}()
	var got result
	returned := false
	select {
	case got = <-done:
		returned = true
	case <-time.After(time.Second):
		// Unblock the old implementation so a failed regression leaves no
		// resolver goroutine waiting for a FIFO writer.
		writer, err := os.OpenFile(path, os.O_RDWR, 0o600)
		require.NoError(t, err)
		require.NoError(t, writer.Close())
		got = <-done
	}
	require.True(t, returned, "credential resolution must reject a FIFO without waiting for a writer")
	require.NoError(t, got.err)
	assert.Empty(t, got.credential.Key)
	assert.Contains(t, got.credential.Reason, "regular file")
}
