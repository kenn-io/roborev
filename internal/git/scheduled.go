package git

import (
	"context"
	"fmt"
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

func FilesUnchangedBetween(ctx context.Context, repoPath, old, current, path string) bool {
	args := []string{"diff", "--quiet", old, current, "--"}
	if path != "" {
		args = append(args, filepath.ToSlash(path))
	}
	cmd := newGitCmdContext(ctx, args...)
	cmd.Dir = repoPath
	return cmd.Run() == nil
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
