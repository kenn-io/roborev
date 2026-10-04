package requestsigning

import (
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenerateKeyFailureCleansOnlyCreatedFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("durable signing keys require POSIX private files and directory fsync")
	}
	path := filepath.Join(t.TempDir(), "reader.key")
	failure := errors.New("fixture directory sync failure")
	require.ErrorIs(t, generateKey(path, func(dir string) error {
		assert.Equal(t, filepath.Dir(path), dir)
		return failure
	}), failure)
	assert.NoFileExists(t, path)
	require.NoError(t, GenerateKey(path), "failed new key initialization must be retryable")
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Error(t, generateKey(path, func(string) error { return failure }))
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, before, after, "pre-existing signing keys must never be removed")
}

func TestNativeSecretInput(t *testing.T) {
	k := testKey(t)
	t.Setenv("TEST_SIGNING_SECRET", hex.EncodeToString(k.Secret))
	got, err := ReadKey(k.ID, "", "TEST_SIGNING_SECRET")
	require.NoError(t, err)
	assert.Equal(t, k.Secret, got.Secret)
	_, err = ReadKey(k.ID, "file", "TEST_SIGNING_SECRET")
	require.Error(t, err)
	t.Setenv("TEST_SIGNING_SECRET", "invalid")
	_, err = ReadKey(k.ID, "", "TEST_SIGNING_SECRET")
	require.Error(t, err)
	t.Run("private file", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("POSIX file mode assertions")
		}
		p := filepath.Join(t.TempDir(), "key")
		require.NoError(t, os.WriteFile(p, []byte(hex.EncodeToString(k.Secret)+"\n"), 0o600))
		_, err := ReadKey(k.ID, p, "")
		require.NoError(t, err)
		require.NoError(t, os.Chmod(p, 0o644))
		_, err = ReadKey(k.ID, p, "")
		require.Error(t, err)
		require.NoError(t, os.Chmod(p, 0o600))
		require.NoError(t, os.WriteFile(p, []byte(strings.Repeat("0", 130)), 0o600))
		_, err = ReadKey(k.ID, p, "")
		require.Error(t, err)
	})
}
