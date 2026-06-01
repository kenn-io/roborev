//go:build integration

package main

import (
	"os/exec"
	"strings"
	"testing"
	"time"

	"go.kenn.io/roborev/internal/testutil"
)

func initTestGitRepo(t *testing.T) string {
	t.Helper()
	tmpDir := t.TempDir()
	testutil.InitTestGitRepo(t, tmpDir)
	return tmpDir
}

func gitRevParse(t *testing.T, dir string, ref string) string {
	t.Helper()
	cmd := exec.Command("git", "-C", dir, "rev-parse", ref)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git rev-parse failed: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func defaultTestRunContext(t *testing.T, repoDir string) RunContext {
	t.Helper()

	return RunContext{
		Context:         t.Context(),
		WorkingDir:      repoDir,
		PollInterval:    1 * time.Millisecond,
		PostCommitDelay: 1 * time.Millisecond,
	}
}

func execGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}
