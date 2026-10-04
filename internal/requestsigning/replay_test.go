package requestsigning

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInitializeReplayFailureCleansOnlyCreatedFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("durable replay requires POSIX private files and directory fsync")
	}
	path := filepath.Join(t.TempDir(), "replay.json")
	failure := errors.New("fixture directory sync failure")
	require.ErrorIs(t, initializeReplay(path, func(string) error { return failure }), failure)
	assert.NoFileExists(t, path)
	require.NoError(t, InitializeReplay(path), "a failed new initialization must be retryable")
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Error(t, initializeReplay(path, func(string) error { return failure }))
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, before, after, "pre-existing replay state must never be removed")
}

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

func TestReplayConcurrentAndRestart(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("durable replay requires POSIX owner-only files and directory fsync")
	}
	p := filepath.Join(t.TempDir(), "replay.json")
	require.NoError(t, InitializeReplay(p))
	store, err := OpenReplay(p, 20)
	require.NoError(t, err)
	now := time.Now().Unix()
	var accepted atomic.Int64
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			if store.Admit("reader", "nonce", now+30, now) == nil {
				accepted.Add(1)
			}
		})
	}
	wg.Wait()
	assert.Equal(t, int64(1), accepted.Load())
	_, err = OpenReplay(p, 20)
	require.Error(t, err, "a second verifier must not share the state")
	require.NoError(t, store.Close())
	store, err = OpenReplay(p, 20)
	require.NoError(t, err)
	defer store.Close()
	require.Error(t, store.Admit("reader", "nonce", now+30, now))
	require.Error(t, store.Admit("reader", "other", now+30, now-1), "clock rollback fails closed")
	require.NoError(t, store.Admit("reader", "fresh", now+30, now))
}

func TestReplayCapacityCorruptionAndMissingState(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("durable replay requires POSIX owner-only files and directory fsync")
	}
	p := filepath.Join(t.TempDir(), "replay.json")
	_, err := OpenReplay(p, 2)
	require.Error(t, err)
	require.NoError(t, InitializeReplay(p))
	require.Error(t, InitializeReplay(p))
	store, err := OpenReplay(p, 2)
	require.NoError(t, err)
	now := time.Now().Unix()
	for n := range 2 {
		require.NoError(t, store.Admit("key", fmt.Sprint(n), now+30, now))
	}
	require.Error(t, store.Admit("key", "full", now+30, now))
	require.NoError(t, store.Admit("key", "after-expiry", now+61, now+31))
	require.NoError(t, store.Close())
	require.NoError(t, os.WriteFile(p, []byte("invalid"), 0o600))
	_, err = OpenReplay(p, 2)
	require.Error(t, err)
}

func TestReplayKeyRotationKeepsAdmissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("durable replay requires POSIX owner-only files and directory fsync")
	}
	p := filepath.Join(t.TempDir(), "replay.json")
	require.NoError(t, InitializeReplay(p))
	store, err := OpenReplay(p, 20)
	require.NoError(t, err)
	defer store.Close()
	old := testKey(t)
	fresh := testKey(t)
	fresh.ID = "reader-2"
	reused := old
	reused.ID = "same-secret-different-scope"
	require.Error(t, store.BindKeys(map[string]Key{old.ID: old, reused.ID: reused}))
	require.NoError(t, store.BindKeys(map[string]Key{old.ID: old}))
	now := time.Now().Unix()
	require.NoError(t, store.Admit(old.ID, "used", now+30, now))
	require.NoError(t, store.BindKeys(map[string]Key{old.ID: old, fresh.ID: fresh}))
	require.Error(t, store.Admit(old.ID, "used", now+30, now))
	require.NoError(t, store.BindKeys(map[string]Key{fresh.ID: fresh}))
	replacement := testKey(t)
	require.Error(t, store.BindKeys(map[string]Key{replacement.ID: replacement}))
}

func TestReplayDurableFenceAfterNonceExpiry(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("durable replay requires POSIX owner-only files and directory fsync")
	}
	p := filepath.Join(t.TempDir(), "replay.json")
	require.NoError(t, InitializeReplay(p))
	store, err := OpenReplay(p, 2)
	require.NoError(t, err)
	now := time.Now().Unix()
	require.NoError(t, store.Admit("reader", "old", now+30, now))
	require.NoError(t, store.Admit("reader", "new", now+61, now+31))
	require.NoError(t, store.Close())
	store, err = OpenReplay(p, 2)
	require.NoError(t, err)
	defer store.Close()
	require.Error(t, store.Admit("reader", "old", now+30, now), "pruned nonce cannot become fresh after rollback and restart")
}

func TestReplayPostPublicationTimeFence(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("durable replay requires POSIX owner-only files and directory fsync")
	}
	p := filepath.Join(t.TempDir(), "replay.json")
	require.NoError(t, InitializeReplay(p))
	store, err := OpenReplay(p, 2)
	require.NoError(t, err)
	defer store.Close()
	now := time.Now().Unix()
	v := Verified{KeyID: "reader", Nonce: "fresh", Created: now, Expires: now + Lifetime}
	require.NoError(t, store.Admit(v.KeyID, v.Nonce, v.Expires, now))
	require.NoError(t, store.checkClock(v, func() int64 { return now }))
	require.Error(t, store.checkClock(v, func() int64 { return now - 1 }))
	require.Error(t, store.checkClock(v, func() int64 { return now + Lifetime + 1 }))
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

func TestReplayRuntimeSubsecondRollback(t *testing.T) {
	store := &ReplayStore{}
	require.NoError(t, store.observeWallClock(time.Unix(1000, 500)))
	require.Error(t, store.observeWallClock(time.Unix(1000, 499)))
	require.NoError(t, store.observeWallClock(time.Unix(1000, 501)))
}
