package prompt

import (
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/mod/modfile"
)

// buildToolchainSection returns a short facts section stating the Go version
// declared by the repository's go.mod, or "" when the change touches no Go
// files. Reviewers judge version-gated language and stdlib availability from
// memory unless the declared version is in front of them, which produces
// false positives about recent features (issue #1202).
func buildToolchainSection(repoPath string, changedFiles []string) string {
	if !changeTouchesGo(changedFiles) {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(repoPath, "go.mod"))
	if err != nil {
		return ""
	}
	file, err := modfile.Parse("go.mod", data, nil)
	if err != nil || file.Go == nil || file.Go.Version == "" {
		return ""
	}
	return "## Toolchain\n\nThis project uses Go " + file.Go.Version +
		" (from the `go` directive in go.mod). Judge version-gated language" +
		" and standard-library features against this version.\n\n"
}

func changeTouchesGo(files []string) bool {
	for _, file := range files {
		base := filepath.Base(filepath.ToSlash(strings.TrimSpace(file)))
		if base == "go.mod" || strings.HasSuffix(base, ".go") {
			return true
		}
	}
	return false
}
