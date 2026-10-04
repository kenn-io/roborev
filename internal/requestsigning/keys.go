package requestsigning

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
)

func privateFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return errors.New("signing files must be regular owner-only files")
	}
	return nil
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
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
