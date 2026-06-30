package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
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
	runner := opts.Runner
	if runner.Env == nil {
		runner = gitcmd.New()
	}
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
		if err := pullGitLFS(ctx, runner, wt.Dir); err != nil {
			return nil, err
		}
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
		uses, err := gitAttributeFileUsesGitLFS(ctx, repoPath, attr)
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

const maxGitAttributeFileBytes = 1 << 20

type gitAttributeFile struct {
	Mode     string
	ObjectID string
	Path     string
}

func pullGitLFS(ctx context.Context, runner gitcmd.Runner, repoPath string) error {
	if _, _, err := runner.Run(ctx, repoPath, nil, "lfs", "env"); err != nil {
		return fmt.Errorf("git lfs unavailable: %w", err)
	}
	if _, _, err := runner.Run(ctx, repoPath, nil, "lfs", "pull"); err != nil {
		return fmt.Errorf("git lfs pull: %w", err)
	}
	return nil
}

func gitAttributeFiles(ctx context.Context, repoPath string) ([]gitAttributeFile, error) {
	out, err := gitOutput(ctx, repoPath, "ls-files", "-s", "-z", "--", ".gitattributes", "**/.gitattributes")
	if err != nil {
		return nil, err
	}
	parts := bytes.Split(out, []byte{0})
	files := make([]gitAttributeFile, 0, len(parts))
	for _, part := range parts {
		if len(part) == 0 {
			continue
		}
		meta, path, ok := strings.Cut(string(part), "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) < 2 {
			return nil, fmt.Errorf("parse tracked attributes entry %q", part)
		}
		files = append(files, gitAttributeFile{
			Mode:     fields[0],
			ObjectID: fields[1],
			Path:     path,
		})
	}
	return files, nil
}

func gitAttributeFileUsesGitLFS(ctx context.Context, repoPath string, attr gitAttributeFile) (bool, error) {
	if attr.Mode != "100644" && attr.Mode != "100755" {
		return false, fmt.Errorf("unsafe tracked attributes mode %s for %s", attr.Mode, attr.Path)
	}
	out, err := gitOutput(ctx, repoPath, "cat-file", "-s", attr.ObjectID)
	if err != nil {
		return false, err
	}
	size, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return false, err
	}
	if size > maxGitAttributeFileBytes {
		return false, fmt.Errorf("tracked attributes file %s is too large: %d bytes", attr.Path, size)
	}
	out, err = gitOutput(ctx, repoPath, "cat-file", "blob", attr.ObjectID)
	if err != nil {
		return false, err
	}
	if len(out) > maxGitAttributeFileBytes {
		return false, fmt.Errorf("tracked attributes file %s exceeded size cap", attr.Path)
	}
	return attributeContentUsesGitLFS(string(out)), nil
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
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("attributes file %s is not regular", path)
	}
	if info.Size() > maxGitAttributeFileBytes {
		return false, fmt.Errorf("attributes file %s is too large: %d bytes", path, info.Size())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	if len(data) > maxGitAttributeFileBytes {
		return false, fmt.Errorf("attributes file %s exceeded size cap", path)
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
