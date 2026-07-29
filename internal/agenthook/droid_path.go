package agenthook

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	gitpkg "go.kenn.io/roborev/internal/git"
)

var droidPathCaseInsensitive = runtime.GOOS == "windows"

func defaultDroidHooksPath() string {
	home := os.Getenv("HOME")
	if home == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil || home == "" {
			return ""
		}
	}
	return filepath.Join(home, ".factory", "hooks.json")
}

func validateDroidHooksPath(path string) error {
	if isUserDroidHooksPath(path) {
		if resolved, ok := evalExistingParentPath(path); ok &&
			!isUserDroidHooksPath(resolved) && isProjectDroidHooksPath(resolved) {
			return projectDroidPathError()
		}
		return nil
	}
	if isProjectDroidHooksPath(path) {
		return projectDroidPathError()
	}
	if resolved, ok := evalExistingParentPath(path); ok && isProjectDroidHooksPath(resolved) {
		return projectDroidPathError()
	}
	return nil
}

func projectDroidPathError() error {
	return fmt.Errorf("project-scoped Factory Droid hook config is not supported; use the user-scoped Factory hooks path instead")
}

func isUserDroidHooksPath(path string) bool {
	return sameCleanAbsPath(path, defaultDroidHooksPath())
}

func isProjectDroidHooksPath(path string) bool {
	path = strings.TrimSpace(path)
	if path == "" {
		return false
	}
	clean := filepath.Clean(path)
	projectRel := filepath.Join(".factory", "hooks.json")
	if sameDroidPath(clean, projectRel) || isTargetRepoDroidHooksPath(clean) {
		return true
	}
	if repoRoot, err := gitpkg.GetRepoRoot("."); err == nil && repoRoot != "" &&
		sameCleanAbsPath(clean, filepath.Join(repoRoot, projectRel)) {
		return true
	}
	if !filepath.IsAbs(clean) {
		return false
	}
	wd, err := os.Getwd()
	if err != nil || wd == "" {
		return false
	}
	projectAbs, err := filepath.Abs(filepath.Join(wd, projectRel))
	return err == nil && sameCleanAbsPath(clean, projectAbs)
}

func isTargetRepoDroidHooksPath(path string) bool {
	abs, ok := cleanAbsPath(path)
	if !ok || !sameDroidPathName(filepath.Base(abs), "hooks.json") {
		return false
	}
	factoryDir := filepath.Dir(abs)
	if !sameDroidPathName(filepath.Base(factoryDir), ".factory") {
		return false
	}
	candidateRoot := filepath.Dir(factoryDir)
	repoRoot, err := gitpkg.GetRepoRoot(candidateRoot)
	return err == nil && repoRoot != "" && sameCleanAbsPath(candidateRoot, repoRoot)
}

func sameCleanAbsPath(a, b string) bool {
	a = strings.TrimSpace(a)
	b = strings.TrimSpace(b)
	if a == "" || b == "" {
		return false
	}
	aAbs, okA := cleanAbsPath(a)
	bAbs, okB := cleanAbsPath(b)
	if okA && okB {
		return sameDroidPath(filepath.Clean(aAbs), filepath.Clean(bAbs))
	}
	return sameDroidPath(filepath.Clean(a), filepath.Clean(b))
}

func cleanAbsPath(path string) (string, bool) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", false
	}
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", false
	}
	return canonicalExistingPath(filepath.Clean(abs)), true
}

func canonicalExistingPath(path string) string {
	path = filepath.Clean(path)
	volume := filepath.VolumeName(path)
	rest := strings.TrimPrefix(path, volume)
	rooted := strings.HasPrefix(rest, string(filepath.Separator))
	rest = strings.TrimPrefix(rest, string(filepath.Separator))
	parts := strings.Split(rest, string(filepath.Separator))

	current := volume
	if rooted {
		current += string(filepath.Separator)
	}
	for i, part := range parts {
		if part == "" {
			continue
		}
		next := filepath.Join(current, part)
		if _, err := os.Lstat(next); err != nil {
			for _, remaining := range parts[i:] {
				if remaining != "" {
					current = filepath.Join(current, remaining)
				}
			}
			return filepath.Clean(current)
		}
		if actual := actualDirEntryName(current, part); actual != "" {
			part = actual
		}
		current = filepath.Join(current, part)
	}
	return filepath.Clean(current)
}

func actualDirEntryName(parent, name string) string {
	if parent == "" {
		parent = "."
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if entry.Name() == name {
			return name
		}
	}
	for _, entry := range entries {
		if strings.EqualFold(entry.Name(), name) {
			return entry.Name()
		}
	}
	return ""
}

func sameDroidPath(a, b string) bool {
	if droidPathCaseInsensitive {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func sameDroidPathName(a, b string) bool {
	return strings.EqualFold(a, b)
}

func evalExistingParentPath(path string) (string, bool) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", false
	}
	clean := filepath.Clean(path)
	existing := clean
	remaining := ""
	for existing != "." && existing != string(filepath.Separator) {
		if _, err := os.Lstat(existing); err == nil {
			break
		}
		remaining = filepath.Join(filepath.Base(existing), remaining)
		existing = filepath.Dir(existing)
	}
	if existing == "." || existing == string(filepath.Separator) {
		return cleanAbsPath(clean)
	}
	resolved, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return cleanAbsPath(clean)
	}
	return cleanAbsPath(filepath.Join(resolved, remaining))
}
