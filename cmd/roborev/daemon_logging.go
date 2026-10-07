package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"

	"github.com/gofrs/flock"
	"go.kenn.io/kit/atomicfile"
)

const maxDaemonLogBytes = 10 * 1024 * 1024

// daemonLogFile is owned by the standard logger, which serializes writes.
// setupDaemonLogging restores the logger before closing its file.
type daemonLogFile struct {
	file     *os.File
	path     string
	size     int64
	maxBytes int64
	closed   bool
}

// Unlike io.MultiWriter, daemonLogOutput attempts both destinations even when
// one fails, so stderr and saved diagnostics remain independent.
type daemonLogOutput struct {
	stderr io.Writer
	file   *daemonLogFile
}

func (w daemonLogOutput) Write(p []byte) (int, error) {
	n, err := w.stderr.Write(p)
	if n < len(p) && err == nil {
		err = io.ErrShortWrite
	}
	m, fileErr := w.file.Write(p)
	if m < len(p) && fileErr == nil {
		fileErr = io.ErrShortWrite
	}
	return min(n, m), errors.Join(err, fileErr)
}

func setupDaemonLogging(dir string, stderr io.Writer, maxBytes int64) (_ func() error, setupErr error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create daemon log directory: %w", err)
	}
	path := filepath.Join(dir, "daemon.log")
	// Database paths can differ while sharing DataDir and these log files.
	// Hold a separate lock before inspecting or changing either file.
	owner := flock.New(path + ".lock")
	locked, err := owner.TryLock()
	if err != nil {
		return nil, errors.Join(fmt.Errorf("lock daemon logs: %w", err), owner.Close())
	}
	if !locked {
		return nil, errors.Join(fmt.Errorf("daemon logs are already owned by a daemon: %s", dir), owner.Close())
	}
	defer func() {
		if setupErr != nil {
			setupErr = errors.Join(setupErr, owner.Close())
		}
	}()
	for _, name := range []string{path, path + ".1"} {
		info, err := os.Stat(name)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("stat daemon log: %w", err)
		}
		if info.Size() > maxBytes {
			if err := os.Truncate(name, maxBytes); err != nil {
				return nil, fmt.Errorf("bound daemon log: %w", err)
			}
		}
	}
	writer := &daemonLogFile{path: path, maxBytes: maxBytes}
	if err := writer.open(); err != nil {
		return nil, err
	}
	oldWriter, oldFlags := log.Writer(), log.Flags()
	log.SetOutput(daemonLogOutput{stderr: stderr, file: writer})
	log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)
	return func() error {
		log.SetOutput(oldWriter)
		log.SetFlags(oldFlags)
		writer.closed = true
		var closeErr error
		if writer.file != nil {
			closeErr = writer.file.Close()
			writer.file = nil
		}
		return errors.Join(closeErr, owner.Close())
	}, nil
}

func (w *daemonLogFile) open() error {
	file, err := os.OpenFile(w.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open daemon log: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		return errors.Join(err, file.Close())
	}
	w.file, w.size = file, info.Size()
	return nil
}

func (w *daemonLogFile) Write(p []byte) (int, error) {
	if w.closed {
		return 0, os.ErrClosed
	}
	// Rotation can fail either before or after the rename. Reopen in append
	// mode and read the actual size so the next write retries without losing
	// existing records or mistaking a filesystem failure for shutdown.
	if w.file == nil {
		if err := w.open(); err != nil {
			return 0, err
		}
	}
	length := len(p)
	if int64(length) > w.maxBytes {
		const marker = "[truncated]\n"
		p = append(bytes.Clone(p[:w.maxBytes-int64(len(marker))]), marker...)
	}
	if w.size+int64(len(p)) > w.maxBytes {
		err := w.file.Close()
		w.file = nil
		if err != nil {
			return 0, err
		}
		if err := atomicfile.Replace(w.path, w.path+".1"); err != nil {
			return 0, err
		}
		if err := w.open(); err != nil {
			return 0, err
		}
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	if err != nil {
		return n, err
	}
	return length, nil
}
