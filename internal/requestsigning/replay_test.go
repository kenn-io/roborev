package requestsigning

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// signedAt returns verified metadata for a signature created at created.
func signedAt(nonce string, created time.Time) Verified {
	return Verified{KeyID: "reader", Nonce: nonce, Created: created.Unix(), Expires: created.Unix() + Lifetime}
}

func TestNonceCacheAdmitsEachSignatureOnce(t *testing.T) {
	started := time.Unix(1000, 0)
	cache := NewNonceCache(started)
	now := started.Add(time.Duration(FutureSkew+1) * time.Second)
	v := signedAt("nonce", now)
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

func TestNonceCacheRejectsSignaturesThePreviousProcessCouldHaveAdmitted(t *testing.T) {
	started := time.Unix(1000, 500_000_000)
	cache := NewNonceCache(started)
	for _, tc := range []struct {
		name    string
		created time.Time
	}{
		{"before start", started.Add(-time.Second)},
		{"same second as start", started.Add(-400 * time.Millisecond)},
		{"future-dated within skew", started.Add(time.Duration(FutureSkew) * time.Second)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Error(t, cache.Admit(signedAt(tc.name, tc.created), started))
		})
	}
	after := started.Add(time.Duration(FutureSkew+1) * time.Second)
	require.NoError(t, cache.Admit(signedAt("after skew", after), after))
}

func TestNonceCacheRejectsExpiredSignatures(t *testing.T) {
	started := time.Unix(1000, 0)
	cache := NewNonceCache(started)
	created := started.Add(time.Duration(FutureSkew+1) * time.Second)
	require.Error(t, cache.Admit(signedAt("late", created), created.Add(time.Duration(Lifetime+1)*time.Second)))
}

func TestNonceCachePrunesExpiredNonces(t *testing.T) {
	started := time.Unix(1000, 0)
	cache := NewNonceCache(started)
	first := started.Add(time.Duration(FutureSkew+1) * time.Second)
	require.NoError(t, cache.Admit(signedAt("old", first), first))
	later := first.Add(time.Duration(Lifetime+1) * time.Second)
	require.NoError(t, cache.Admit(signedAt("next", later), later))
	assert.Len(t, cache.seen, 1)
}
