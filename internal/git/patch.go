package git

import (
	"context"
	"fmt"
	"strings"

	gitworktree "go.kenn.io/kit/git/worktree"
)

// ApplyPatch applies a binary patch using the user's persistent Git config.
func ApplyPatch(ctx context.Context, repoPath, patch string) error {
	return applyPatch(ctx, repoPath, patch, false)
}

// CheckPatch checks whether a binary patch applies without modifying files.
// Conflicts return a *gitworktree.PatchConflictError.
func CheckPatch(ctx context.Context, repoPath, patch string) error {
	return applyPatch(ctx, repoPath, patch, true)
}

func applyPatch(ctx context.Context, repoPath, patch string, checkOnly bool) error {
	if patch == "" {
		return nil
	}
	args := []string{"apply"}
	if checkOnly {
		args = append(args, "--check")
	}
	args = append(args, "--binary", "-")
	// Use the same config as checkout and patch capture. kit's standalone patch
	// helpers hide global/system config, including core.autocrlf, so an LF patch
	// can fail to match a CRLF working tree even when checkout honored that key.
	_, stderr, err := gitRunner.Run(ctx, repoPath, strings.NewReader(patch), args...)
	if err == nil {
		return nil
	}
	msg := strings.TrimSpace(string(stderr))
	// Git exposes patch conflicts only through stderr, not distinct exit codes.
	if checkOnly && (strings.Contains(msg, "patch failed") || strings.Contains(msg, "does not apply")) { //nolint:kennlint // errtext: Git's conflict diagnostics are the only signal
		return &gitworktree.PatchConflictError{Detail: msg}
	}
	return fmt.Errorf("git apply: %w", err)
}
