package daemon

import (
	"path/filepath"
	"sync"
)

var gitMetadataLocks sync.Map

func canonicalPath(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	return filepath.Clean(path)
}

func lockGitMetadata(repoPath string) func() {
	key := canonicalPath(repoPath)
	value, _ := gitMetadataLocks.LoadOrStore(key, &sync.Mutex{})
	mu := value.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}
