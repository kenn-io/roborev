package requestsigning

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/gofrs/flock"
)

// ReplayStore has one process owner and an atomic snapshot. The lock is on a
// separate stable inode, so replacing the snapshot never releases exclusion.
type ReplayStore struct {
	mu           sync.Mutex
	path         string
	capacity     int
	lock         *flock.Flock
	state        replayState
	closed       bool
	failed       bool
	lastWallNano int64
}
type replayState struct {
	Version   int               `json:"version"`
	Highwater int64             `json:"highwater"`
	Nonces    map[string]int64  `json:"nonces"`
	Keys      map[string]string `json:"keys"`
}

// InitializeReplay is only for new keys. Missing state is never auto-created by
// a running verifier; recovery after losing state requires fresh signing keys.
func InitializeReplay(path string) error {
	return initializeReplay(path, syncDirectory)
}

func initializeReplay(path string, syncParent func(string) error) error {
	if runtime.GOOS == "windows" {
		return errors.New("private signing files require POSIX owner-only permissions")
	}
	state := replayState{Version: 1, Highwater: time.Now().Unix(), Nonces: map[string]int64{}, Keys: map[string]string{}}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return errors.New("replay initialization requires a new state file")
	}
	complete := false
	defer func() {
		if !complete {
			// This invocation exclusively created the file. Never remove
			// pre-existing state, including when initialization is retried.
			_ = os.Remove(path)
			_ = syncDirectory(filepath.Dir(path))
		}
	}()
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err = syncParent(filepath.Dir(path)); err != nil {
		return err
	}
	complete = true
	return nil
}

func privateFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return errors.New("signing files must be regular owner-only files")
	}
	return nil
}

// OpenReplay requires existing private state and an explicit admission capacity.
func OpenReplay(path string, capacity int) (*ReplayStore, error) {
	if capacity <= 0 {
		return nil, errors.New("replay capacity must be positive")
	}
	if err := privateFile(path); err != nil {
		return nil, err
	}
	// Refuse a pre-existing symlink or public lock before acquiring it.
	if _, err := os.Lstat(path + ".lock"); err == nil {
		if err = privateFile(path + ".lock"); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	lock := flock.New(path+".lock", flock.SetPermissions(0o600))
	locked, err := lock.TryLock()
	if err != nil || !locked {
		return nil, errors.New("replay state already in use or cannot be locked")
	}
	fail := func() (*ReplayStore, error) {
		_ = lock.Close()
		return nil, errors.New("invalid replay state; recover with fresh signing keys")
	}
	f, err := os.Open(path)
	if err != nil {
		return fail()
	}
	// Bound serialized state by its actual schema: each nonce entry contains
	// 64 hex chars, a signed int64 (20 chars), and 5 JSON delimiters. Each key
	// entry contains at most 128 ID chars, 64 hex chars, and 6 delimiters.
	// The fixed envelope is measured from the maximal empty-state encoding.
	envelope, _ := json.Marshal(replayState{Version: 1, Highwater: 9223372036854775807, Nonces: map[string]int64{}, Keys: map[string]string{}})
	maxStateBytes := int64(len(envelope)) + int64(capacity)*(64+20+5+128+64+6)
	data, err := io.ReadAll(io.LimitReader(f, maxStateBytes+1))
	closeErr := f.Close()
	if closeErr != nil || int64(len(data)) > maxStateBytes {
		return fail()
	}
	if err != nil {
		return fail()
	}
	var state replayState
	if err = json.Unmarshal(data, &state, json.RejectUnknownMembers(true)); err != nil || state.Version != 1 || state.Highwater < 0 || state.Nonces == nil || state.Keys == nil || len(state.Nonces) > capacity || len(state.Keys) > capacity {
		return fail()
	}
	for nonce, expiry := range state.Nonces {
		decoded, err := hex.DecodeString(nonce)
		if err != nil || len(decoded) != sha256.Size || expiry < 0 {
			return fail()
		}
	}
	for id, fingerprint := range state.Keys {
		decoded, err := hex.DecodeString(fingerprint)
		if !keyIDPattern.MatchString(id) || err != nil || len(decoded) != sha256.Size {
			return fail()
		}
	}
	return &ReplayStore{path: path, capacity: capacity, lock: lock, state: state}, nil
}

// BindKeys permanently binds each key ID to secret material, without clearing
// replay history. Removed key IDs cannot later be rebound to a different key.
func (s *ReplayStore) BindKeys(keys map[string]Key) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.failed {
		return errors.New("replay state unavailable")
	}
	owners := make(map[string]string, len(s.state.Keys))
	next := make(map[string]string, len(s.state.Keys)+len(keys))
	for id, fp := range s.state.Keys {
		owners[fp] = id
		next[id] = fp
	}
	newIDs := 0
	for id := range keys {
		if _, ok := s.state.Keys[id]; !ok {
			newIDs++
		}
	}
	if len(s.state.Keys)+newIDs > s.capacity {
		return errors.New("replay key identity capacity reached")
	}
	for id, k := range keys {
		if id != k.ID || k.Validate() != nil {
			return ErrInvalid
		}
		fingerprintMAC := hmac.New(sha256.New, k.Secret)
		_, _ = fingerprintMAC.Write([]byte("roborev signing key identity v1"))
		fingerprint := hex.EncodeToString(fingerprintMAC.Sum(nil))
		if owner, ok := owners[fingerprint]; ok && owner != id {
			return errors.New("each signing key requires independent fresh secret material")
		}
		owners[fingerprint] = id
		if previous, ok := s.state.Keys[id]; ok && previous != fingerprint {
			return errors.New("new signing secret requires a new key ID")
		}
		next[id] = fingerprint
	}
	s.state.Keys = next
	return s.persist()
}

// Admit atomically consumes the nonce before dispatch. The caller supplies its
// current wall time under the request policy; pruning advances the durable clock
// watermark in the same transaction. An uncertain write poisons this verifier.
func (s *ReplayStore) Admit(keyID, nonce string, expiry, now int64) error {
	return s.admit(keyID, nonce, expiry, func() int64 { return now })
}

// AdmitVerified rechecks wall time inside the mutex after hashing or queuing.
func (s *ReplayStore) AdmitVerified(v Verified) error {
	return s.admit(v.KeyID, v.Nonce, v.Expires, s.wallClock)
}

// CheckVerified repeats the clock fence after durable publication, which can
// take long enough for a signature to expire or the clock to move backward.
func (s *ReplayStore) CheckVerified(v Verified) error {
	return s.checkClock(v, s.wallClock)
}

// observeWallClock compares Unix wall values, never Go's monotonic component.
// Callers hold mu; rollback remains fenced until the observed wall catches up.
func (s *ReplayStore) observeWallClock(now time.Time) error {
	wall := now.UnixNano()
	if wall < s.lastWallNano {
		return errors.New("wall clock moved backward")
	}
	s.lastWallNano = wall
	return nil
}

func (s *ReplayStore) wallClock() int64 {
	now := time.Now()
	if s.observeWallClock(now) != nil {
		return -1
	}
	return now.Unix()
}

func (s *ReplayStore) checkClock(v Verified, clock func() int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := clock()
	if s.closed || s.failed || now < s.state.Highwater || !v.Fresh(now) {
		return errors.New("signature expired or clock moved backward after publication")
	}
	return nil
}

func (s *ReplayStore) admit(keyID, nonce string, expiry int64, clock func() int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := clock()
	if s.closed || s.failed || now < s.state.Highwater || now > expiry || expiry > now+Lifetime+FutureSkew {
		return errors.New("replay state or signature time invalid")
	}
	sum := sha256.Sum256([]byte(keyID + "\x00" + nonce))
	hash := hex.EncodeToString(sum[:])
	if _, ok := s.state.Nonces[hash]; ok {
		return errors.New("request signature replayed")
	}
	for hash, until := range s.state.Nonces {
		if now > until {
			delete(s.state.Nonces, hash)
		}
	}
	if len(s.state.Nonces) >= s.capacity {
		return errors.New("replay admission capacity reached")
	}
	s.state.Highwater = now
	s.state.Nonces[hash] = expiry
	return s.persist()
}

func (s *ReplayStore) persist() error {
	data, err := json.Marshal(s.state)
	if err == nil {
		err = atomicSnapshot(s.path, data)
	}
	if err != nil {
		s.failed = true
		return errors.New("cannot persist replay admission")
	}
	return nil
}

func atomicSnapshot(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".replay-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (s *ReplayStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.lock.Close()
}

// ReadKey reads a 64-byte hex secret from exactly one private file or named env.
func ReadKey(id, file, environment string) (Key, error) {
	if (file == "") == (environment == "") {
		return Key{}, errors.New("signing key requires exactly one secret_file or secret_env")
	}
	var raw string
	if file != "" {
		if err := privateFile(file); err != nil {
			return Key{}, err
		}
		f, err := os.Open(file)
		if err != nil {
			return Key{}, errors.New("cannot read signing key file")
		}
		// The fixed secret encoding is exactly 128 hex bytes plus an optional newline.
		data, err := io.ReadAll(io.LimitReader(f, 130))
		closeErr := f.Close()
		if closeErr != nil || len(data) > 129 {
			return Key{}, errors.New("invalid signing key file length")
		}
		if err != nil {
			return Key{}, errors.New("cannot read signing key file")
		}
		raw = string(data)
	} else {
		raw = os.Getenv(environment)
	}
	raw = stringTrim(raw)
	if len(raw) != 128 {
		return Key{}, errors.New("signing secret must contain 128 lowercase hex characters")
	}
	secret, err := hex.DecodeString(raw)
	k := Key{ID: id, Secret: secret}
	if err != nil || hex.EncodeToString(secret) != raw || k.Validate() != nil {
		return Key{}, errors.New("signing secret must contain 128 lowercase hex characters")
	}
	return k, nil
}

func stringTrim(raw string) string {
	// Permit the single terminal newline produced by common secret generators.
	if len(raw) > 0 && raw[len(raw)-1] == '\n' {
		return raw[:len(raw)-1]
	}
	return raw
}

// GenerateKey writes a new secret file without ever putting the secret on stdout.
func GenerateKey(path string) error {
	return generateKey(path, syncDirectory)
}

func generateKey(path string, syncParent func(string) error) error {
	if runtime.GOOS == "windows" {
		return errors.New("private signing files require POSIX owner-only permissions")
	}
	k := make([]byte, 64)
	if _, err := rand.Read(k); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return errors.New("key generation requires a new secret file")
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.Remove(path)
			_ = syncDirectory(filepath.Dir(path))
		}
	}()
	_, err = fmt.Fprintln(f, hex.EncodeToString(k))
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err = syncParent(filepath.Dir(path)); err != nil {
		return err
	}
	complete = true
	return nil
}
