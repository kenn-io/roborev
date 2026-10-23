//go:build !windows

package kata

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func currentUserID() uint32 {
	// The kernel represents effective user IDs as unsigned 32-bit values even
	// though os.Geteuid returns an int.
	return uint32(os.Geteuid()) // #nosec G115 -- kernel UID width is uint32
}

func fileOwnerID(info os.FileInfo) (uint32, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return stat.Uid, true
}

// validateSecureSocket requires a socket that is not a symlink, is owned by the
// user and not writable by anyone else, and sits in a private directory owned
// by the same user, so another account cannot stand in for the Kata server and
// read the requests. A socket in a private directory may itself be readable:
// Kata's local daemon creates its socket with mode 0755 inside a 0700
// directory.
func validateSecureSocket(path string, owner uint32) error {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: Unix socket is missing", ErrUnreachable)
		}
		return fmt.Errorf("%w: cannot inspect the Unix socket", ErrInsecureEndpoint)
	}
	if info.Mode()&os.ModeSymlink != 0 || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%w: Unix socket must be a non-symlinked socket that only its owner can write", ErrInsecureEndpoint)
	}
	if uid, ok := fileOwnerID(info); !ok || uid != owner {
		return fmt.Errorf("%w: Unix socket is owned by another user", ErrInsecureEndpoint)
	}
	parent, err := os.Lstat(filepath.Dir(path))
	if err != nil || parent.Mode()&os.ModeSymlink != 0 || !parent.IsDir() || parent.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%w: Unix socket directory must be private and not a symlink", ErrInsecureEndpoint)
	}
	if uid, ok := fileOwnerID(parent); !ok || uid != owner {
		return fmt.Errorf("%w: Unix socket directory is owned by another user", ErrInsecureEndpoint)
	}
	return nil
}
