// Package githook manages roborev's git hook installation,
// upgrade, and removal. It supports both standalone hooks
// (fresh installs) and embedded snippets that coexist with
// existing hook scripts.
package githook

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"go.kenn.io/roborev/internal/git"
)

// ErrNonShellHook is returned when a hook uses a non-shell
// interpreter and cannot be safely modified.
var ErrNonShellHook = errors.New("non-shell interpreter")

// BinaryResolution describes the roborev binary path to bake into hooks.
type BinaryResolution struct {
	Path   string
	Notice string
}

// InstallOptions configures hook installation.
type InstallOptions struct {
	Force      bool
	BinaryPath string
}

type binaryResolverDeps struct {
	executable  func() (string, error)
	lookPath    func(string) (string, error)
	userHomeDir func() (string, error)
}

func defaultBinaryResolverDeps() binaryResolverDeps {
	return binaryResolverDeps{
		executable:  os.Executable,
		lookPath:    exec.LookPath,
		userHomeDir: os.UserHomeDir,
	}
}

// HasRealErrors returns true if err contains any error that
// is not ErrNonShellHook. Use this instead of errors.Is when
// checking joined errors that may contain both non-shell
// warnings and real failures.
func HasRealErrors(err error) bool {
	if err == nil {
		return false
	}
	type unwrapper interface{ Unwrap() []error }
	if joined, ok := err.(unwrapper); ok {
		for _, e := range joined.Unwrap() {
			if !errors.Is(e, ErrNonShellHook) {
				return true
			}
		}
		return false
	}
	return !errors.Is(err, ErrNonShellHook)
}

// Version markers identify the current hook template version.
// Bump these when the hook template changes to trigger
// upgrade warnings and auto-upgrades.
const PostCommitVersionMarker = "post-commit hook v4"
const PostRewriteVersionMarker = "post-rewrite hook v2"

// VersionMarker returns the current version marker for a hook.
func VersionMarker(hookName string) string {
	switch hookName {
	case "post-commit":
		return PostCommitVersionMarker
	case "post-rewrite":
		return PostRewriteVersionMarker
	default:
		return ""
	}
}

// ReadFile is the function used to re-read a hook file after
// cleanup during upgrade. Replaceable in tests to simulate
// read failures.
var ReadFile = os.ReadFile

// NeedsUpgrade checks whether a repo's named hook contains
// roborev but is outdated (missing the given version marker).
func NeedsUpgrade(repoPath, hookName, versionMarker string) bool {
	hooksDir, err := git.GetHooksPath(repoPath)
	if err != nil {
		return false
	}
	content, err := os.ReadFile(filepath.Join(hooksDir, hookName))
	if err != nil {
		return false
	}
	s := string(content)
	return strings.Contains(strings.ToLower(s), "roborev") &&
		!strings.Contains(s, versionMarker)
}

// NotInstalled checks whether the named hook file is absent
// or does not contain any roborev content.
func NotInstalled(repoPath, hookName string) bool {
	hooksDir, err := git.GetHooksPath(repoPath)
	if err != nil {
		return false
	}
	content, err := os.ReadFile(
		filepath.Join(hooksDir, hookName),
	)
	if err != nil {
		return os.IsNotExist(err)
	}
	return !strings.Contains(
		strings.ToLower(string(content)), "roborev",
	)
}

// Missing checks whether a repo has roborev installed
// (post-commit hook present) but is missing the named hook.
func Missing(repoPath, hookName string) bool {
	hooksDir, err := git.GetHooksPath(repoPath)
	if err != nil {
		return false
	}
	pcContent, err := os.ReadFile(
		filepath.Join(hooksDir, "post-commit"),
	)
	if err != nil {
		return false
	}
	if !strings.Contains(
		strings.ToLower(string(pcContent)), "roborev",
	) {
		return false
	}
	content, err := os.ReadFile(filepath.Join(hooksDir, hookName))
	if err != nil {
		return os.IsNotExist(err)
	}
	return !strings.Contains(
		strings.ToLower(string(content)), "roborev",
	)
}

// ResolveRoborevPath returns the roborev path to bake into git hooks.
// When override is empty, it lightly probes common version managers and
// prefers their stable shim/symlink over a versioned install path.
func ResolveRoborevPath(override string) (BinaryResolution, error) {
	return resolveRoborevPathWithDeps(override, defaultBinaryResolverDeps())
}

func resolveRoborevPathWithDeps(override string, deps binaryResolverDeps) (BinaryResolution, error) {
	override = strings.TrimSpace(override)
	if override != "" {
		path, err := normalizeOverridePath(override, deps)
		if err != nil {
			return BinaryResolution{}, err
		}
		return BinaryResolution{Path: path}, nil
	}

	current, err := deps.executable()
	roborevPath, _ := deps.lookPath("roborev")
	if err != nil {
		if roborevPath == "" {
			roborevPath = "roborev"
		}
		return BinaryResolution{Path: roborevPath}, nil
	}

	if roborevPath != "" {
		if manager, ok := managedBinaryShim(current, roborevPath); ok {
			return BinaryResolution{
				Path: roborevPath,
				Notice: fmt.Sprintf(
					"Detected roborev managed by %s; installing hooks with %s instead of versioned binary %s",
					manager, roborevPath, current,
				),
			}, nil
		}
	}

	if manager := versionedManagerInstall(current); manager != "" {
		return BinaryResolution{
			Path: current,
			Notice: fmt.Sprintf(
				"Warning: roborev appears to be running from a versioned %s install (%s); use --binary to install hooks with a stable shim if available",
				manager, current,
			),
		}, nil
	}

	return BinaryResolution{Path: current}, nil
}

// resolveRoborevPath returns the path to use in generated hooks. It is kept
// for callers that only need the legacy default behavior.
func resolveRoborevPath() string {
	resolution, err := ResolveRoborevPath("")
	if err == nil {
		return resolution.Path
	}
	roborevPath, _ := exec.LookPath("roborev")
	if roborevPath == "" {
		roborevPath = "roborev"
	}
	return roborevPath
}

func normalizeOverridePath(raw string, deps binaryResolverDeps) (string, error) {
	path := expandHome(raw, deps)
	if !hasPathSeparator(path) {
		resolved, err := deps.lookPath(path)
		if err != nil {
			return "", fmt.Errorf("find %s on PATH: %w", path, err)
		}
		path = resolved
	} else if !filepath.IsAbs(path) {
		abs, err := filepath.Abs(path)
		if err != nil {
			return "", fmt.Errorf("resolve %s: %w", raw, err)
		}
		path = abs
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", path, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s is a directory, not a binary", path)
	}
	if runtime.GOOS != "windows" && info.Mode()&0111 == 0 {
		return "", fmt.Errorf("%s is not executable", path)
	}
	return path, nil
}

func expandHome(path string, deps binaryResolverDeps) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := deps.userHomeDir()
		if err == nil && home != "" {
			if path == "~" {
				return home
			}
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

func hasPathSeparator(path string) bool {
	return strings.ContainsRune(path, os.PathSeparator) ||
		(os.PathSeparator != '/' && strings.ContainsRune(path, '/'))
}

func managedBinaryShim(current, pathRoborev string) (string, bool) {
	current = filepath.ToSlash(current)
	pathRoborev = filepath.ToSlash(pathRoborev)
	switch {
	case isMiseManagedRoborev(current) &&
		strings.Contains(pathRoborev, "/mise/shims/roborev"):
		return "mise", true
	case strings.Contains(current, "/Cellar/roborev/") &&
		isHomebrewBin(pathRoborev):
		return "Homebrew", true
	default:
		return "", false
	}
}

func versionedManagerInstall(current string) string {
	current = filepath.ToSlash(current)
	switch {
	case isMiseManagedRoborev(current):
		return "mise"
	case strings.Contains(current, "/Cellar/roborev/"):
		return "Homebrew"
	default:
		return ""
	}
}

func isMiseManagedRoborev(path string) bool {
	return strings.Contains(path, "/mise/installs/") &&
		strings.HasSuffix(path, "/roborev")
}

func isHomebrewBin(path string) bool {
	return strings.HasSuffix(path, "/bin/roborev") &&
		(strings.HasPrefix(path, "/opt/homebrew/") ||
			strings.HasPrefix(path, "/usr/local/") ||
			strings.HasPrefix(path, "/home/linuxbrew/.linuxbrew/"))
}

// GeneratePostCommit returns a standalone post-commit hook
// (with shebang, suitable for fresh installs).
func GeneratePostCommit() string {
	return GeneratePostCommitWithBinary(resolveRoborevPath())
}

// GeneratePostCommitWithBinary returns a post-commit hook using roborevPath.
func GeneratePostCommitWithBinary(roborevPath string) string {
	return fmt.Sprintf(`#!/bin/sh
# roborev %s - auto-reviews every commit
ROBOREV=%q
if [ ! -x "$ROBOREV" ]; then
    ROBOREV=$(command -v roborev 2>/dev/null)
    [ -z "$ROBOREV" ] || [ ! -x "$ROBOREV" ] && exit 0
fi
"$ROBOREV" post-commit 2>/dev/null
`, PostCommitVersionMarker, roborevPath)
}

// GeneratePostRewrite returns a standalone post-rewrite hook
// (with shebang, suitable for fresh installs).
func GeneratePostRewrite() string {
	return GeneratePostRewriteWithBinary(resolveRoborevPath())
}

// GeneratePostRewriteWithBinary returns a post-rewrite hook using roborevPath.
func GeneratePostRewriteWithBinary(roborevPath string) string {
	return fmt.Sprintf(`#!/bin/sh
# roborev %s - remaps reviews after rebase/amend
ROBOREV=%q
if [ ! -x "$ROBOREV" ]; then
    ROBOREV=$(command -v roborev 2>/dev/null)
    [ -z "$ROBOREV" ] || [ ! -x "$ROBOREV" ] && exit 0
fi
"$ROBOREV" remap --quiet 2>/dev/null
`, PostRewriteVersionMarker, roborevPath)
}

// generateEmbeddablePostCommit returns a function-wrapped
// snippet without shebang, for embedding in existing hooks.
// Uses return instead of exit so it doesn't terminate the
// parent script.
func generateEmbeddablePostCommit() string {
	return generateEmbeddablePostCommitWithBinary(resolveRoborevPath())
}

func generateEmbeddablePostCommitWithBinary(roborevPath string) string {
	return fmt.Sprintf(`# roborev %s - auto-reviews every commit
_roborev_hook() {
ROBOREV=%q
if [ ! -x "$ROBOREV" ]; then
    ROBOREV=$(command -v roborev 2>/dev/null)
    [ -z "$ROBOREV" ] || [ ! -x "$ROBOREV" ] && return 0
fi
"$ROBOREV" post-commit 2>/dev/null
}
_roborev_hook
`, PostCommitVersionMarker, roborevPath)
}

// generateEmbeddablePostRewrite returns a function-wrapped
// snippet without shebang, for embedding in existing hooks.
func generateEmbeddablePostRewrite() string {
	return generateEmbeddablePostRewriteWithBinary(resolveRoborevPath())
}

func generateEmbeddablePostRewriteWithBinary(roborevPath string) string {
	return fmt.Sprintf(`# roborev %s - remaps reviews after rebase/amend
_roborev_remap() {
ROBOREV=%q
if [ ! -x "$ROBOREV" ]; then
    ROBOREV=$(command -v roborev 2>/dev/null)
    [ -z "$ROBOREV" ] || [ ! -x "$ROBOREV" ] && return 0
fi
"$ROBOREV" remap --quiet 2>/dev/null
}
_roborev_remap
`, PostRewriteVersionMarker, roborevPath)
}

func generateContentWithBinary(hookName, roborevPath string) string {
	switch hookName {
	case "post-commit":
		return GeneratePostCommitWithBinary(roborevPath)
	case "post-rewrite":
		return GeneratePostRewriteWithBinary(roborevPath)
	default:
		return ""
	}
}

func generateEmbeddableWithBinary(hookName, roborevPath string) string {
	switch hookName {
	case "post-commit":
		return generateEmbeddablePostCommitWithBinary(roborevPath)
	case "post-rewrite":
		return generateEmbeddablePostRewriteWithBinary(roborevPath)
	default:
		return ""
	}
}

// embedSnippet inserts snippet after the shebang line of
// existing, so it runs before any possible exit in the
// original script. If there is no shebang, the snippet is
// prepended.
func embedSnippet(existing, snippet string) string {
	lines := strings.SplitAfter(existing, "\n")
	if len(lines) > 0 &&
		strings.HasPrefix(strings.TrimSpace(lines[0]), "#!") {
		shebang := lines[0]
		if !strings.HasSuffix(shebang, "\n") {
			shebang += "\n"
		}
		return shebang + snippet + strings.Join(lines[1:], "")
	}
	return snippet + existing
}

// Install installs or upgrades a single hook. It handles:
//   - No existing hook: write standalone content
//   - Existing without roborev: embed snippet after shebang
//   - Existing with current version: skip (no-op)
//   - Existing with old version: remove old, embed new
//   - force=true: overwrite unconditionally
func Install(hooksDir, hookName string, force bool) error {
	return InstallWithOptions(hooksDir, hookName, InstallOptions{
		Force: force,
	})
}

// InstallWithOptions installs or upgrades a single hook with options.
func InstallWithOptions(hooksDir, hookName string, opts InstallOptions) error {
	hookPath := filepath.Join(hooksDir, hookName)
	versionMarker := VersionMarker(hookName)
	resolution, err := ResolveRoborevPath(opts.BinaryPath)
	if err != nil {
		return err
	}
	hookContent := generateContentWithBinary(hookName, resolution.Path)

	existing, err := os.ReadFile(hookPath)
	if err == nil && !opts.Force {
		existingStr := string(existing)
		if !strings.Contains(
			strings.ToLower(existingStr), "roborev",
		) {
			if !isShellHook(existingStr) {
				return fmt.Errorf(
					"%s hook: %w; add the roborev snippet "+
						"manually or use --force to overwrite",
					hookName, ErrNonShellHook)
			}
			hookContent = embedSnippet(
				existingStr,
				generateEmbeddableWithBinary(hookName, resolution.Path),
			)
		} else if strings.Contains(existingStr, versionMarker) {
			if !hookUsesBinary(existingStr, resolution.Path) {
				hookContent, err = renderUpdatedHookContent(
					hookPath,
					hookName,
					existingStr,
					resolution.Path,
				)
				if err != nil {
					return err
				}
			} else {
				fmt.Printf(
					"%s hook already installed (current)\n",
					hookName,
				)
				return nil
			}
		} else {
			hookContent, err = renderUpdatedHookContent(
				hookPath,
				hookName,
				existingStr,
				resolution.Path,
			)
			if err != nil {
				return err
			}
		}
	}

	if err := os.WriteFile(
		hookPath, []byte(hookContent), 0755,
	); err != nil {
		return fmt.Errorf("write %s hook: %w", hookName, err)
	}
	fmt.Printf("Installed %s hook at %s\n", hookName, hookPath)
	return nil
}

func hookUsesBinary(content, roborevPath string) bool {
	return strings.Contains(content, fmt.Sprintf("ROBOREV=%q", roborevPath))
}

func renderUpdatedHookContent(
	hookPath, hookName, existingStr, roborevPath string,
) (string, error) {
	if !isShellHook(existingStr) {
		return "", fmt.Errorf(
			"%s hook: %w; add the roborev snippet "+
				"manually or use --force to overwrite",
			hookName, ErrNonShellHook)
	}
	if rmErr := Uninstall(hookPath); rmErr != nil {
		return "", fmt.Errorf("upgrade %s: %w", hookName, rmErr)
	}
	updated, readErr := ReadFile(hookPath)
	if readErr != nil && !os.IsNotExist(readErr) {
		return "", fmt.Errorf(
			"re-read %s after cleanup: %w",
			hookName, readErr,
		)
	}
	if readErr == nil {
		return embedSnippet(
			string(updated),
			generateEmbeddableWithBinary(hookName, roborevPath),
		), nil
	}
	// If the old roborev block was the whole file, Uninstall removes it.
	return generateContentWithBinary(hookName, roborevPath), nil
}

// InstallAll installs both post-commit and post-rewrite hooks.
// It attempts all hooks and returns a joined error if any fail.
func InstallAll(hooksDir string, force bool) error {
	return InstallAllWithOptions(hooksDir, InstallOptions{
		Force: force,
	})
}

// InstallAllWithOptions installs both post-commit and post-rewrite hooks.
// It attempts all hooks and returns a joined error if any fail.
func InstallAllWithOptions(hooksDir string, opts InstallOptions) error {
	var errs []error
	for _, name := range []string{"post-commit", "post-rewrite"} {
		if err := InstallWithOptions(hooksDir, name, opts); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Uninstall removes the roborev block from a hook file, or
// deletes it entirely if nothing else remains.
func Uninstall(hookPath string) error {
	content, err := os.ReadFile(hookPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf(
			"read %s: %w", filepath.Base(hookPath), err,
		)
	}

	hookStr := string(content)
	if !strings.Contains(strings.ToLower(hookStr), "roborev") {
		return nil
	}

	lines := strings.Split(hookStr, "\n")

	blockStart := -1
	for i, line := range lines {
		if isRoborevMarker(line) {
			blockStart = i
			break
		}
	}
	if blockStart < 0 {
		return nil
	}

	blockEnd := blockStart
	inIfBlock := false
	inFuncBlock := false
	for i := blockStart + 1; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" {
			if i+1 < len(lines) &&
				isRoborevSnippetLine(lines[i+1]) {
				blockEnd = i
				continue
			}
			break
		}
		if isRoborevSnippetLine(trimmed) {
			blockEnd = i
			if strings.HasPrefix(trimmed, "if ") {
				inIfBlock = true
			}
			if strings.HasSuffix(trimmed, "() {") {
				inFuncBlock = true
			}
			continue
		}
		if trimmed == "fi" && inIfBlock {
			blockEnd = i
			inIfBlock = false
			continue
		}
		if trimmed == "}" && inFuncBlock {
			blockEnd = i
			inFuncBlock = false
			continue
		}
		break
	}

	remaining := make([]string, 0, len(lines))
	remaining = append(remaining, lines[:blockStart]...)
	remaining = append(remaining, lines[blockEnd+1:]...)

	hasContent := false
	for _, line := range remaining {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" && !strings.HasPrefix(trimmed, "#!") {
			hasContent = true
			break
		}
	}

	hookName := filepath.Base(hookPath)
	if hasContent {
		newContent := strings.Join(remaining, "\n")
		if !strings.HasSuffix(newContent, "\n") {
			newContent += "\n"
		}
		if err := os.WriteFile(
			hookPath, []byte(newContent), 0755,
		); err != nil {
			return fmt.Errorf("write %s: %w", hookName, err)
		}
		fmt.Printf("Removed roborev from %s\n", hookName)
	} else {
		if err := os.Remove(hookPath); err != nil {
			return fmt.Errorf("remove %s: %w", hookName, err)
		}
		fmt.Printf("Removed %s hook\n", hookName)
	}
	return nil
}

// isShellHook returns true if the hook content starts with a
// POSIX-compatible shell shebang.
func isShellHook(content string) bool {
	first, _, _ := strings.Cut(content, "\n")
	first = strings.TrimSpace(first)
	for _, sh := range []string{
		"sh", "bash", "zsh", "ksh", "dash",
	} {
		if strings.HasPrefix(first, "#!/bin/"+sh) ||
			strings.HasPrefix(first, "#!/usr/bin/env "+sh) {
			return true
		}
	}
	return false
}

// isRoborevMarker returns true if the line is a generated
// roborev hook marker comment.
func isRoborevMarker(line string) bool {
	trimmed := strings.TrimSpace(strings.ToLower(line))
	return strings.HasPrefix(
		trimmed, "# roborev post-commit hook",
	) || strings.HasPrefix(
		trimmed, "# roborev post-rewrite hook",
	)
}

// hasCommandPrefix checks if line starts with prefix and the
// prefix is followed by end-of-string, whitespace, or a shell
// operator. Prevents "enqueue --quiet" from matching
// "enqueue --quietly".
func hasCommandPrefix(line, prefix string) bool {
	if !strings.HasPrefix(line, prefix) {
		return false
	}
	if len(line) == len(prefix) {
		return true
	}
	next := line[len(prefix)]
	return next == ' ' || next == '\t' || next == '>' ||
		next == '|' || next == '&' || next == ';'
}

// isRoborevSnippetLine returns true if the line is part of a
// generated roborev hook snippet (current or legacy versions).
func isRoborevSnippetLine(line string) bool {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return false
	}
	return strings.HasPrefix(trimmed, "ROBOREV=") ||
		strings.HasPrefix(trimmed, "ROBOREV=$(") ||
		hasCommandPrefix(
			trimmed, "\"$ROBOREV\" post-commit",
		) ||
		hasCommandPrefix(
			trimmed, "\"$ROBOREV\" enqueue --quiet",
		) ||
		hasCommandPrefix(
			trimmed, "\"$ROBOREV\" remap --quiet",
		) ||
		hasCommandPrefix(trimmed, "roborev post-commit") ||
		hasCommandPrefix(trimmed, "roborev enqueue") ||
		hasCommandPrefix(trimmed, "roborev remap") ||
		strings.HasPrefix(
			trimmed, "if [ ! -x \"$ROBOREV\"",
		) ||
		strings.HasPrefix(
			trimmed, "if [ -z \"$ROBOREV\"",
		) ||
		strings.HasPrefix(trimmed, "[ -z \"$ROBOREV\"") ||
		strings.HasPrefix(trimmed, "[ ! -x \"$ROBOREV\"") ||
		trimmed == "return 0" ||
		strings.HasPrefix(trimmed, "_roborev_hook") ||
		strings.HasPrefix(trimmed, "_roborev_remap")
}
