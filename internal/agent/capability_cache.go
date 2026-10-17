package agent

import (
	"os"
	"os/exec"
	"sync"
)

// capabilityCache remembers what an agent CLI's help output reported.
//
// The daemon runs for days, and agent CLIs upgrade themselves or get upgraded
// underneath it. An entry is reused only while the command still resolves to
// an executable with the size and modification time it had when it was probed,
// so an upgraded CLI is probed again instead of being driven with the flags of
// the version that was installed when the daemon started.
type capabilityCache struct {
	entries sync.Map // key -> capabilityCacheEntry
}

type capabilityCacheEntry struct {
	executable executableFingerprint
	supported  bool
}

type executableFingerprint struct {
	path    string
	size    int64
	modTime int64
}

// fingerprintExecutable identifies the file that running command executes.
// os.Stat follows symlinks, so package managers that link a stable name to a
// versioned file are covered.
func fingerprintExecutable(command string) executableFingerprint {
	path, err := exec.LookPath(command)
	if err != nil {
		return executableFingerprint{path: command}
	}
	info, err := os.Stat(path)
	if err != nil {
		return executableFingerprint{path: path}
	}
	return executableFingerprint{path: path, size: info.Size(), modTime: info.ModTime().UnixNano()}
}

// Load returns the cached probe result for key, if command still resolves to
// the executable that produced it.
func (c *capabilityCache) Load(command string, key any) (supported, ok bool) {
	cached, found := c.entries.Load(key)
	if !found {
		return false, false
	}
	entry := cached.(capabilityCacheEntry)
	if entry.executable != fingerprintExecutable(command) {
		return false, false
	}
	return entry.supported, true
}

// Store records a probe result for key against the executable command
// currently resolves to.
func (c *capabilityCache) Store(command string, key any, supported bool) {
	c.entries.Store(key, capabilityCacheEntry{
		executable: fingerprintExecutable(command),
		supported:  supported,
	})
}
