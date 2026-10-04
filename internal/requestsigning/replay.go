package requestsigning

import (
	"errors"
	"sync"
	"time"
)

// NonceCache admits each signature once while it can still verify. It rejects
// signatures created at or before its start plus FutureSkew: the previous
// process may have admitted any of those, including future-dated signatures
// and ones from the same whole second, so a restart cannot replay them.
type NonceCache struct {
	mu      sync.Mutex
	started int64
	seen    map[string]int64
}

// NewNonceCache starts the restart cutoff at now.
func NewNonceCache(now time.Time) *NonceCache {
	return &NonceCache{started: now.Unix(), seen: map[string]int64{}}
}

// Admit consumes the signature's nonce if the signature is fresh at now.
func (c *NonceCache) Admit(v Verified, now time.Time) error {
	if v.Created <= c.started+FutureSkew {
		return errors.New("signature may predate verifier start")
	}
	if !v.Fresh(now.Unix()) {
		return ErrInvalid
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, expires := range c.seen {
		if now.Unix() > expires {
			delete(c.seen, id)
		}
	}
	id := v.KeyID + "\x00" + v.Nonce
	if _, ok := c.seen[id]; ok {
		return errors.New("request signature replayed")
	}
	c.seen[id] = v.Expires
	return nil
}
