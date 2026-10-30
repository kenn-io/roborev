package procutil

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAppendPathDirsKeepsExistingEntriesFirst(t *testing.T) {
	root := t.TempDir()
	present := filepath.Join(root, "present")
	shims := filepath.Join(root, "shims")
	missing := filepath.Join(root, "missing")
	for _, dir := range []string{present, shims} {
		require.NoError(t, os.MkdirAll(dir, 0o755))
	}
	pathList := strings.Join([]string{"/usr/bin", present}, string(os.PathListSeparator))

	got, added := AppendPathDirs(pathList, []string{present, missing, shims, shims})

	assert.Equal(t, []string{"/usr/bin", present, shims}, filepath.SplitList(got))
	assert.Equal(t, []string{shims}, added)
}
