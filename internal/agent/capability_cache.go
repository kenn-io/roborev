package agent

import (
	"os"
	"path/filepath"
	"sync"

	"go.kenn.io/kit/pathresolve"
)

// capabilityProbeKey identifies the executable and invocation used by a probe.
// Resolve symlinks so package-manager upgrades invalidate cached capabilities
// even when the command name and the target's size and modtime stay the same.
type capabilityProbeKey struct {
	path             string
	size             int64
	modTime          int64
	ignoreUserConfig bool
}

func capabilityCacheKey(command string) capabilityProbeKey {
	path, err := resolveExecutable(command)
	if err != nil {
		return capabilityProbeKey{}
	}
	path, err = pathresolve.EvalSymlinks(path)
	if err != nil {
		return capabilityProbeKey{}
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return capabilityProbeKey{}
	}
	info, err := os.Stat(path)
	if err != nil {
		return capabilityProbeKey{}
	}
	return capabilityProbeKey{path: path, size: info.Size(), modTime: info.ModTime().UnixNano()}
}

// capabilityCache keeps successful probe results while the executable matches.
// An unresolved executable must never reuse or populate the cache.
type capabilityCache struct {
	entries sync.Map
}

func (c *capabilityCache) Load(key capabilityProbeKey) (any, bool) {
	if key.path == "" {
		return nil, false
	}
	return c.entries.Load(key)
}

func (c *capabilityCache) Store(key capabilityProbeKey, supported bool) {
	if key.path != "" {
		c.entries.Store(key, supported)
	}
}
