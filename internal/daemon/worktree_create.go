package daemon

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	gitcmd "go.kenn.io/kit/git/cmd"
	gitworktree "go.kenn.io/kit/git/worktree"
)

func createWorkerWorktree(
	ctx context.Context,
	repoPath, ref string,
	opts gitworktree.Options,
) (*gitworktree.Worktree, error) {
	initSubmodules := opts.InitSubmodules
	pullLFS := opts.PullLFS
	opts.InitSubmodules = false
	opts.PullLFS = false

	wt, err := gitworktree.Create(ctx, repoPath, ref, opts)
	if err != nil {
		return nil, err
	}
	complete := false
	defer func() {
		if !complete {
			_ = wt.Close(context.Background())
		}
	}()

	if initSubmodules && checkoutHasGitmodules(wt.Dir) {
		if err := wt.InitSubmodules(ctx); err != nil {
			return nil, err
		}
	}
	if pullLFS && checkoutUsesGitLFS(ctx, wt.Dir) {
		wt.PullLFS(ctx)
	}

	complete = true
	return wt, nil
}

func checkoutHasGitmodules(repoPath string) bool {
	_, err := os.Stat(filepath.Join(repoPath, ".gitmodules"))
	return err == nil || !errors.Is(err, os.ErrNotExist)
}

func checkoutUsesGitLFS(ctx context.Context, repoPath string) bool {
	attrs, err := gitAttributeFiles(ctx, repoPath)
	if err != nil {
		return true
	}
	for _, attr := range attrs {
		uses, err := attributeFileUsesGitLFS(filepath.Join(repoPath, filepath.FromSlash(attr)))
		if err != nil {
			return true
		}
		if uses {
			return true
		}
	}

	uses, ok := gitPathAttributeFileUsesGitLFS(ctx, repoPath, "info/attributes")
	if !ok || uses {
		return true
	}
	uses, ok = configuredAttributeFileUsesGitLFS(ctx, repoPath)
	if !ok || uses {
		return true
	}
	return false
}

func gitAttributeFiles(ctx context.Context, repoPath string) ([]string, error) {
	out, err := gitOutput(ctx, repoPath, "ls-files", "-z", "--", ".gitattributes", "**/.gitattributes")
	if err != nil {
		return nil, err
	}
	parts := bytes.Split(out, []byte{0})
	files := make([]string, 0, len(parts))
	for _, part := range parts {
		if len(part) == 0 {
			continue
		}
		files = append(files, string(part))
	}
	return files, nil
}

func gitPathAttributeFileUsesGitLFS(ctx context.Context, repoPath, path string) (bool, bool) {
	out, err := gitOutput(ctx, repoPath, "rev-parse", "--git-path", path)
	if err != nil {
		return false, false
	}
	attrPath := strings.TrimSpace(string(out))
	if attrPath == "" {
		return false, true
	}
	if !filepath.IsAbs(attrPath) {
		attrPath = filepath.Join(repoPath, attrPath)
	}
	uses, err := attributeFileUsesGitLFS(attrPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, true
	}
	return uses, err == nil
}

func configuredAttributeFileUsesGitLFS(ctx context.Context, repoPath string) (bool, bool) {
	out, err := gitOutput(ctx, repoPath, "config", "--path", "--get", "core.attributesFile")
	if err != nil {
		if isGitExitCode(err, 1) {
			return defaultAttributeFileUsesGitLFS(ctx, repoPath)
		}
		return false, false
	}
	attrPath := strings.TrimSpace(string(out))
	if attrPath == "" {
		return defaultAttributeFileUsesGitLFS(ctx, repoPath)
	}
	return attributePathUsesGitLFS(repoPath, attrPath)
}

func defaultAttributeFileUsesGitLFS(ctx context.Context, repoPath string) (bool, bool) {
	out, err := gitOutput(ctx, repoPath, "var", "GIT_ATTR_GLOBAL")
	if err != nil {
		return false, false
	}
	attrPath := strings.TrimSpace(string(out))
	if attrPath == "" {
		return false, true
	}
	return attributePathUsesGitLFS(repoPath, attrPath)
}

func attributePathUsesGitLFS(repoPath, attrPath string) (bool, bool) {
	if !filepath.IsAbs(attrPath) {
		attrPath = filepath.Join(repoPath, attrPath)
	}
	uses, err := attributeFileUsesGitLFS(attrPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, true
	}
	return uses, err == nil
}

func attributeFileUsesGitLFS(path string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	return attributeContentUsesGitLFS(string(data)), nil
}

func attributeContentUsesGitLFS(content string) bool {
	for line := range strings.Lines(content) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if slices.Contains(strings.Fields(line), "filter=lfs") {
			return true
		}
	}
	return false
}

func gitOutput(ctx context.Context, repoPath string, args ...string) ([]byte, error) {
	return gitcmd.New().Output(ctx, repoPath, args...)
}

func isGitExitCode(err error, code int) bool {
	var gitErr *gitcmd.GitError
	if errors.As(err, &gitErr) {
		err = gitErr.Err
	}
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr) && exitErr.ExitCode() == code
}
