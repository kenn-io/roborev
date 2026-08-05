package agenthook

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// commitAgentHookConfig commits the complete planned configuration in one
// atomic replacement, preserving the mode and target of an existing symlink.
func commitAgentHookConfig(path string, data []byte) error {
	writePath := path
	info, err := os.Lstat(path)
	mode := os.FileMode(0o600)
	switch {
	case err == nil && info.Mode()&os.ModeSymlink != 0:
		writePath, err = filepath.EvalSymlinks(path)
		if err != nil {
			return fmt.Errorf("resolve agent hook config symlink %s: %w", path, err)
		}
		if targetInfo, statErr := os.Stat(writePath); statErr == nil {
			mode = targetInfo.Mode().Perm()
		}
	case err == nil:
		mode = info.Mode().Perm()
	case !errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("inspect agent hook config %s: %w", path, err)
	}
	dir := filepath.Dir(writePath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create agent hook config directory %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(writePath)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary agent hook config: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	closeWithError := func(writeErr error) error {
		_ = tmp.Close()
		return writeErr
	}
	if err := tmp.Chmod(mode); err != nil {
		return closeWithError(fmt.Errorf("set temporary agent hook config mode: %w", err))
	}
	if _, err := bytes.NewReader(data).WriteTo(tmp); err != nil {
		return closeWithError(fmt.Errorf("write temporary agent hook config: %w", err))
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary agent hook config: %w", err)
	}
	if err := replaceAgentHookConfigFile(tmpPath, writePath); err != nil {
		return fmt.Errorf("replace agent hook config %s: %w", path, err)
	}
	return nil
}
