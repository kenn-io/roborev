package git

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// CurrentHeadSHA returns the full HEAD commit without consulting the working tree.
func CurrentHeadSHA(ctx context.Context, repoPath string) (string, error) {
	cmd := newGitCmdContext(ctx, "rev-parse", "--verify", "HEAD")
	cmd.Dir = repoPath
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse HEAD: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func TrackedFilesAt(ctx context.Context, repoPath, sha string) ([]string, error) {
	cmd := newGitCmdContext(ctx, "ls-tree", "-r", "-z", sha)
	cmd.Dir = repoPath
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-tree: %w", err)
	}
	var files []string
	for entry := range strings.SplitSeq(strings.TrimSuffix(string(out), "\x00"), "\x00") {
		meta, path, ok := strings.Cut(entry, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) < 2 || fields[1] != "blob" {
			continue
		}
		files = append(files, filepath.ToSlash(path))
	}
	sort.Strings(files)
	return files, nil
}

func ReadBlobAt(ctx context.Context, repoPath, sha, path string) (string, error) {
	cmd := newGitCmdContext(ctx, "show", sha+":"+filepath.ToSlash(path))
	cmd.Dir = repoPath
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git show %s:%s: %w", sha, path, err)
	}
	return string(out), nil
}

func CommitExists(ctx context.Context, repoPath, sha string) (bool, error) {
	cmd := newGitCmdContext(ctx, "cat-file", "-e", sha+"^{commit}")
	cmd.Dir = repoPath
	if err := cmd.Run(); err == nil {
		return true, nil
	} else if ctx.Err() != nil {
		return false, ctx.Err()
	} else if _, ok := errors.AsType[*exec.ExitError](err); ok {
		return false, nil
	} else {
		return false, fmt.Errorf("git cat-file %s: %w", sha, err)
	}
}

func ChangedFilesBetween(ctx context.Context, repoPath, old, current string) (map[string]struct{}, error) {
	cmd := newGitCmdContext(ctx, "diff", "--name-only", "-z", old, current, "--")
	cmd.Dir = repoPath
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git diff %s..%s: %w", old, current, err)
	}
	changed := make(map[string]struct{})
	for path := range strings.SplitSeq(strings.TrimSuffix(string(out), "\x00"), "\x00") {
		if path != "" {
			changed[filepath.ToSlash(path)] = struct{}{}
		}
	}
	return changed, nil
}

// IsSourceFile is the shared analysis source-file policy.
func IsSourceFile(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".go", ".py", ".js", ".ts", ".tsx", ".jsx", ".rs", ".c", ".h", ".cpp", ".hpp", ".cc", ".java", ".kt", ".scala", ".rb", ".php", ".swift", ".m", ".cs", ".fs", ".vb", ".sh", ".bash", ".zsh", ".fish", ".sql", ".graphql", ".proto", ".yaml", ".yml", ".toml", ".json", ".md", ".txt", ".html", ".css", ".scss":
		return true
	default:
		return false
	}
}
