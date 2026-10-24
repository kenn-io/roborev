package main

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/testutil"
)

func TestLocalReviewToolPolicyRejectsUnsupported(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	marker := filepath.Join(t.TempDir(), "invoked")
	executable := filepath.Join(t.TempDir(), "claude-fixture")
	script := "#!/bin/sh\nprintf invoked > '" + marker + "'\nexit 1\n"
	if runtime.GOOS == "windows" {
		executable += ".cmd"
		script = "@echo off\r\necho invoked > \"" + marker + "\"\r\nexit /b 1\r\n"
	}
	require.NoError(t, os.WriteFile(executable, []byte(script), 0o700))
	cfg := config.DefaultConfig()
	cfg.ClaudeCodeCmd = executable
	require.NoError(t, config.SaveGlobal(cfg))
	repo := testutil.NewGitRepo(t)
	sha := repo.CommitFile("draft.md", "Draft\n", "draft")
	require.NoError(t, os.WriteFile(filepath.Join(repo.Path(), ".roborev.toml"), []byte("[review.types.citations]\ntemplate='review.md'\n[review.types.citations.tools]\nmode='read-only'\n"), 0o600))
	cmd := &cobra.Command{}
	cmd.SetOut(io.Discard)
	err := runLocalReview(cmd, repo.Path(), sha, "", nil, "claude-code", "", "", "standard", "citations", true, "")
	require.Error(t, err)
	// The exact primary rejection must precede any native CLI invocation.
	require.ErrorContains(t, err, "does not support strict review tools")
	require.NoFileExists(t, marker)
}
