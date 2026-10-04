package requestsigning

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNonceCacheAdmitsEachSignatureOnce(t *testing.T) {
	now := time.Unix(1000, 0)
	cache := NewNonceCache(now)
	v := Verified{KeyID: "reader", Nonce: "nonce", Created: now.Unix(), Expires: now.Unix() + Lifetime}
	var accepted atomic.Int64
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			if cache.Admit(v, now) == nil {
				accepted.Add(1)
			}
		})
	}
	wg.Wait()
	assert.Equal(t, int64(1), accepted.Load())
	other := v
	other.KeyID = "other-reader"
	require.NoError(t, cache.Admit(other, now), "nonces are scoped to their key")
}

func TestNonceCacheRejectsStaleAndPreStartSignatures(t *testing.T) {
	now := time.Unix(1000, 0)
	cache := NewNonceCache(now)
	before := Verified{KeyID: "reader", Nonce: "before", Created: now.Unix() - 1, Expires: now.Unix() - 1 + Lifetime}
	require.Error(t, cache.Admit(before, now), "a restart must not admit signatures the previous process could have seen")
	fresh := Verified{KeyID: "reader", Nonce: "fresh", Created: now.Unix(), Expires: now.Unix() + Lifetime}
	require.Error(t, cache.Admit(fresh, now.Add(time.Duration(Lifetime+1)*time.Second)))
	require.NoError(t, cache.Admit(fresh, now))
}

func TestNonceCachePrunesExpiredNonces(t *testing.T) {
	now := time.Unix(1000, 0)
	cache := NewNonceCache(now)
	old := Verified{KeyID: "reader", Nonce: "old", Created: now.Unix(), Expires: now.Unix() + Lifetime}
	require.NoError(t, cache.Admit(old, now))
	later := now.Add(time.Duration(Lifetime+1) * time.Second)
	next := Verified{KeyID: "reader", Nonce: "next", Created: later.Unix(), Expires: later.Unix() + Lifetime}
	require.NoError(t, cache.Admit(next, later))
	assert.Len(t, cache.seen, 1)
}
