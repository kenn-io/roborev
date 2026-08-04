package agent

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// identityProbeTimeout is the context deadline for version probes used to
// disambiguate executables that share a common command name (notably Cursor's
// "agent" vs Grok Build's installer alias "agent"). Overridable in tests.
//
// Combined with identityProbeWaitDelay, the maximum wall-clock wait for a
// single probe invocation is approximately timeout + WaitDelay: CommandContext
// kills the process at the deadline, and WaitDelay bounds how long Wait may
// block on unclosed stdout/stderr pipes held by orphan descendants.
var identityProbeTimeout = 2 * time.Second

// identityProbeWaitDelay is the post-kill Wait bound for identity probes.
// WaitDelay == 0 (the CombinedOutput default) can leave Wait blocked forever
// when a child process inherits and keeps the output pipes open after the
// parent is killed. See os/exec Command.WaitDelay.
var identityProbeWaitDelay = 250 * time.Millisecond

// identityProbeMaxOutput is the maximum version output retained while probing.
// Captured during I/O (not after), so a misbehaving binary cannot force large
// allocations. Oversize output is treated as identityUnknown.
const identityProbeMaxOutput = 4096

// errProbeOutputExceeded is returned when a probe emits more than
// identityProbeMaxOutput bytes. Callers treat it as identityUnknown.
var errProbeOutputExceeded = errors.New("identity probe output exceeded limit")

// identityProbeArgs is the ordered list of version invocations tried when
// disambiguating binaries. Only global version flags are used: Cursor
// documents -v/--version as version options and treats positional arguments
// as the agent prompt, so "agent version" / "agent v" must never be run.
// The official Grok installer also validates freshly downloaded binaries
// with --version before creating the agent alias.
var identityProbeArgs = []string{"--version", "-v"}

// commandIdentity is the tri-state result of identifying an ambiguous
// command. Unknown must fail closed for Cursor availability.
type commandIdentity uint8

const (
	identityUnknown commandIdentity = iota
	identityGrok
	identityNotGrok
)

// identityProbeCache avoids repeated subprocess probes for the same binary.
// Keyed by resolved path; invalidated when size or modtime change.
// Only conclusive identities (Grok / NotGrok) are stored — unknown is
// never cached so a transient timeout cannot pin "Cursor" forever.
type identityProbeCacheEntry struct {
	size     int64
	modTime  int64 // UnixNano
	identity commandIdentity
}

var (
	identityProbeCacheMu sync.Mutex
	identityProbeCache   = map[string]identityProbeCacheEntry{}
)

// clearIdentityProbeCache is for tests that need a cold probe path.
func clearIdentityProbeCache() {
	identityProbeCacheMu.Lock()
	defer identityProbeCacheMu.Unlock()
	identityProbeCache = map[string]identityProbeCacheEntry{}
}

// commandLooksLikeGrok reports whether command is the Grok Build CLI
// (or a symlink/copy of it). Used so Cursor's "agent" command is not
// claimed when only Grok's agent alias is installed.
//
// Resolution order:
//  1. LookPath / absolute path existence
//  2. SameFile against a resolved "grok" binary (Unix symlinks)
//  3. Bounded version probe for Windows copies and ambiguous paths
//
// Does not hash the binary. False when command is unavailable or identity
// is inconclusive.
func commandLooksLikeGrok(command string) bool {
	return resolveCommandIdentity(command) == identityGrok
}

// commandIsUsableCursorCandidate reports whether command can be treated as a
// Cursor agent candidate: it exists, answers a version flag with non-empty
// short output, and that output is not Grok. It does not prove the binary is
// Cursor — only that it is not Grok's agent alias and is conclusively
// identifiable. Inconclusive probes (timeout, crash, empty/oversize output)
// fail closed (return false).
func commandIsUsableCursorCandidate(command string) bool {
	return resolveCommandIdentity(command) == identityNotGrok
}

func resolveCommandIdentity(command string) commandIdentity {
	resolved, err := resolveExecutable(command)
	if err != nil {
		return identityUnknown
	}
	if grokPath, err := exec.LookPath(defaultGrokCommand); err == nil {
		if sameFile(resolved, grokPath) {
			return identityGrok
		}
	}
	return versionProbeIdentity(resolved)
}

func resolveExecutable(command string) (string, error) {
	if command == "" {
		return "", os.ErrNotExist
	}
	return exec.LookPath(command)
}

func sameFile(a, b string) bool {
	// Resolve symlinks when possible so agent → grok links match.
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	if errA == nil {
		a = ra
	}
	if errB == nil {
		b = rb
	}
	if a == b {
		return true
	}
	fa, err := os.Stat(a)
	if err != nil {
		return false
	}
	fb, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(fa, fb)
}

func versionProbeIdentity(command string) commandIdentity {
	info, err := os.Stat(command)
	if err != nil {
		return identityUnknown
	}
	size := info.Size()
	modTime := info.ModTime().UnixNano()

	identityProbeCacheMu.Lock()
	if e, ok := identityProbeCache[command]; ok && e.size == size && e.modTime == modTime {
		id := e.identity
		identityProbeCacheMu.Unlock()
		return id
	}
	identityProbeCacheMu.Unlock()

	id := probeVersionIdentity(command)
	// Cache only conclusive results. Unknown must be re-probed so a
	// transient failure cannot permanently classify the binary as Cursor.
	if id == identityGrok || id == identityNotGrok {
		identityProbeCacheMu.Lock()
		identityProbeCache[command] = identityProbeCacheEntry{
			size:     size,
			modTime:  modTime,
			identity: id,
		}
		identityProbeCacheMu.Unlock()
	}
	return id
}

// versionOutputLooksLikeGrok is kept for tests that assert the probe path
// without going through resolveCommandIdentity's SameFile short-circuit.
func versionOutputLooksLikeGrok(command string) bool {
	return versionProbeIdentity(command) == identityGrok
}

func probeVersionIdentity(command string) commandIdentity {
	ctx, cancel := context.WithTimeout(context.Background(), identityProbeTimeout)
	defer cancel()

	for _, arg := range identityProbeArgs {
		if ctx.Err() != nil {
			return identityUnknown
		}
		out, err := runIdentityProbe(ctx, command, arg)
		// Timeout, WaitDelay, oversize dump, crash, or empty → try next flag
		// or fail closed as unknown. Never treat these as NotGrok.
		if errors.Is(err, errProbeOutputExceeded) ||
			errors.Is(err, exec.ErrWaitDelay) ||
			errors.Is(err, context.DeadlineExceeded) ||
			errors.Is(err, context.Canceled) {
			return identityUnknown
		}
		if err != nil || len(bytes.TrimSpace(out)) == 0 {
			continue
		}
		lower := strings.ToLower(string(out))
		// Grok Build: "grok 0.2.118 (...)"
		if strings.Contains(lower, "grok") {
			return identityGrok
		}
		// Non-empty short version output that is not Grok: conclusive not-Grok.
		return identityNotGrok
	}
	return identityUnknown
}

// limitedProbeWriter caps captured probe output and is safe for concurrent
// stdout+stderr writes (same writer for both pipes).
type limitedProbeWriter struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (w *limitedProbeWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.truncated {
		return len(p), nil
	}
	remaining := w.limit - w.buf.Len()
	if remaining <= 0 {
		w.truncated = true
		return len(p), nil
	}
	if len(p) > remaining {
		_, _ = w.buf.Write(p[:remaining])
		w.truncated = true
		return len(p), nil
	}
	return w.buf.Write(p)
}

func (w *limitedProbeWriter) Bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]byte(nil), w.buf.Bytes()...)
}

func runIdentityProbe(ctx context.Context, command string, arg string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, command, arg)
	configureCapabilityProbe(cmd)
	// No stdin. Environment is inherited (probes are not env-sanitized);
	// configureCapabilityProbe only hides the console on Windows and moves
	// the working directory to os.TempDir().
	cmd.Stdin = nil
	// Non-zero WaitDelay so Wait returns after context kill even when an
	// orphan descendant keeps stdout/stderr pipes open.
	cmd.WaitDelay = identityProbeWaitDelay
	// CommandContext only installs a default Kill cancel when Cancel is nil
	// and WaitDelay is zero. Setting WaitDelay suppresses that default.
	if cmd.Cancel == nil {
		cmd.Cancel = func() error {
			if cmd.Process == nil {
				return nil
			}
			return cmd.Process.Kill()
		}
	}

	w := &limitedProbeWriter{limit: identityProbeMaxOutput}
	cmd.Stdout = w
	cmd.Stderr = w
	err := cmd.Run()
	if w.truncated {
		return nil, errProbeOutputExceeded
	}
	if errors.Is(err, exec.ErrWaitDelay) {
		return w.Bytes(), err
	}
	return w.Bytes(), err
}
