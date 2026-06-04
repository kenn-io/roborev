package procutil

import (
	"os"
	"path/filepath"
	"strings"
)

// GitRepoEnvKeys lists git environment variables that bind commands to a
// specific repository or worktree. These must be stripped when spawning daemon
// processes so they resolve refs from their request context.
var GitRepoEnvKeys = map[string]struct{}{
	"GIT_DIR":                          {},
	"GIT_WORK_TREE":                    {},
	"GIT_INDEX_FILE":                   {},
	"GIT_OBJECT_DIRECTORY":             {},
	"GIT_ALTERNATE_OBJECT_DIRECTORIES": {},
	"GIT_COMMON_DIR":                   {},
	"GIT_CEILING_DIRECTORIES":          {},
	"GIT_NAMESPACE":                    {},
	"GIT_PREFIX":                       {},
	"GIT_QUARANTINE_PATH":              {},
	"GIT_DISCOVERY_ACROSS_FILESYSTEM":  {},
	"GIT_CONFIG_PARAMETERS":            {},
	"GIT_CONFIG_COUNT":                 {},
	"GIT_CONFIG_GLOBAL":                {},
	"GIT_CONFIG_SYSTEM":                {},
	"GIT_EXTERNAL_DIFF":                {},
	"GIT_DIFF_OPTS":                    {},
}

var gitRepoEnvPrefixes = []string{
	"GIT_CONFIG_KEY_",
	"GIT_CONFIG_VALUE_",
}

func IsGitRepoEnvKey(entry string) bool {
	key, _, _ := strings.Cut(entry, "=")
	upper := strings.ToUpper(key)
	if _, ok := GitRepoEnvKeys[upper]; ok {
		return true
	}
	for _, prefix := range gitRepoEnvPrefixes {
		if strings.HasPrefix(upper, prefix) {
			return true
		}
	}
	return false
}

func FilterGitEnv(env []string) []string {
	result := make([]string, 0, len(env))
	for _, e := range env {
		if IsGitRepoEnvKey(e) {
			continue
		}
		result = append(result, e)
	}
	return result
}

func IsGoTestBinaryPath(exePath string) bool {
	base := strings.ToLower(filepath.Base(exePath))
	return strings.HasSuffix(base, ".test") ||
		strings.HasSuffix(base, ".test.exe")
}

func IsGoBuildCacheBinary(exePath string) bool {
	for seg := range strings.SplitSeq(exePath, string(filepath.Separator)) {
		if seg == "go-build" {
			return true
		}
		if after, ok := strings.CutPrefix(seg, "go-build"); ok &&
			after != "" && isAllDigits(after) {
			return true
		}
	}
	return false
}

func isAllDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func ShouldRefuseAutoStartDaemon(exePath string) bool {
	if IsGoBuildCacheBinary(exePath) {
		return true
	}
	if !IsGoTestBinaryPath(exePath) {
		return false
	}
	return os.Getenv("ROBOREV_TEST_ALLOW_AUTOSTART") != "1"
}
