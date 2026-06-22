package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHydrateAssetsForceFetchesRemoteAssetBranches(t *testing.T) {
	tempDir := t.TempDir()
	remoteRepo := filepath.Join(tempDir, "remote")
	localRepo := filepath.Join(tempDir, "local")
	require.NoError(t, os.MkdirAll(remoteRepo, 0o755))
	require.NoError(t, os.MkdirAll(localRepo, 0o755))

	git(t, remoteRepo, "init")
	git(t, remoteRepo, "config", "user.name", "Test User")
	git(t, remoteRepo, "config", "user.email", "test@example.invalid")
	writeStaticAssets(t, remoteRepo, "old static")
	git(t, remoteRepo, "add", ".")
	git(t, remoteRepo, "commit", "-m", "old static assets")
	git(t, remoteRepo, "branch", "docs-assets")

	git(t, localRepo, "init")
	git(t, localRepo, "remote", "add", "origin", remoteRepo)
	git(t, localRepo, "fetch", "origin", "docs-assets:refs/remotes/origin/docs-assets")

	git(t, remoteRepo, "rm", "-r", ".")
	writeStaticAssets(t, remoteRepo, "new static")
	git(t, remoteRepo, "add", ".")
	git(t, remoteRepo, "commit", "-m", "new static assets")
	newStaticCommit := gitOutput(t, remoteRepo, "rev-parse", "HEAD")
	git(t, remoteRepo, "update-ref", "refs/heads/docs-assets", newStaticCommit)

	git(t, remoteRepo, "rm", "-r", ".")
	writeGeneratedAssets(t, remoteRepo, "generated")
	git(t, remoteRepo, "add", ".")
	git(t, remoteRepo, "commit", "-m", "generated assets")
	git(t, remoteRepo, "branch", "docs-generated-assets")

	git(t, localRepo, "fetch", "origin", "docs-generated-assets:refs/remotes/origin/docs-generated-assets")

	docsAssetsDir := filepath.Join(localRepo, "docs", "assets")
	require.NoError(t, os.MkdirAll(docsAssetsDir, 0o755))
	writeStaticAssets(t, filepath.Join(docsAssetsDir, "static"), "stale local static")
	writeGeneratedAssets(t, filepath.Join(docsAssetsDir, "generated"), "stale local generated")

	git(t, localRepo, "remote", "set-url", "origin", bashPath(t, remoteRepo))

	script := readShellScript(t, filepath.Join("..", "docs", "assets", "hydrate-assets.sh"))
	scriptPath := filepath.Join(docsAssetsDir, "hydrate-assets.sh")
	require.NoError(t, os.WriteFile(scriptPath, script, 0o755))

	cmd := exec.Command("bash", bashPath(t, scriptPath))
	cmd.Dir = localRepo
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))

	logo, err := os.ReadFile(filepath.Join(localRepo, "docs", "assets", "static", "logo.svg"))
	require.NoError(t, err)
	assert.Equal(t, "new static\n", string(logo))

	screenshot, err := os.ReadFile(filepath.Join(localRepo, "docs", "assets", "generated", "tui-hero.svg"))
	require.NoError(t, err)
	assert.Equal(t, "generated\n", string(screenshot))
}

func TestAssetPublishersRejectUnexpectedFiles(t *testing.T) {
	cases := []struct {
		name      string
		scriptRel string
		write     func(*testing.T, string, string)
	}{
		{
			name:      "static",
			scriptRel: filepath.Join("docs", "assets", "update-static-assets-branch.sh"),
			write:     writeStaticAssets,
		},
		{
			name:      "generated",
			scriptRel: filepath.Join("docs", "screenshots", "update-generated-assets-branch.sh"),
			write:     writeGeneratedAssets,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tempDir := t.TempDir()
			repo := filepath.Join(tempDir, "repo")
			sourceDir := filepath.Join(tempDir, "source")
			require.NoError(t, os.MkdirAll(repo, 0o755))
			git(t, repo, "init")
			tc.write(t, sourceDir, "asset")
			require.NoError(t, os.WriteFile(filepath.Join(sourceDir, ".env.local"), []byte("TOKEN=secret\n"), 0o600))

			scriptPath := installAssetScript(t, repo, tc.scriptRel)
			cmd := exec.Command("bash", bashPath(t, scriptPath), "--source", bashPath(t, sourceDir))
			cmd.Dir = repo
			output, err := cmd.CombinedOutput()

			require.Error(t, err, string(output))
			assert.Contains(t, string(output), "unexpected")
			assert.Contains(t, string(output), ".env.local")
		})
	}
}

func installAssetScript(t *testing.T, repo, scriptRel string) string {
	t.Helper()
	script := readShellScript(t, filepath.Join("..", scriptRel))
	scriptPath := filepath.Join(repo, scriptRel)
	require.NoError(t, os.MkdirAll(filepath.Dir(scriptPath), 0o755))
	require.NoError(t, os.WriteFile(scriptPath, script, 0o755))
	return scriptPath
}

func writeStaticAssets(t *testing.T, dir, content string) {
	t.Helper()
	files := []string{
		"agent-hook-feedback-loop.png",
		"agents/claude-code.svg",
		"agents/codex.svg",
		"agents/copilot.svg",
		"agents/cursor.svg",
		"agents/gemini.svg",
		"agents/opencode.svg",
		"agents/pi.svg",
		"architecture.svg",
		"claudechic-review-sidebar.png",
		"favicon-32.png",
		"favicon-64.png",
		"favicon.svg",
		"federation.svg",
		"logo-with-text-dark-bg.png",
		"logo-with-text-dark-bg.svg",
		"logo-with-text-light.png",
		"logo-with-text-light.svg",
		"logo-with-text.png",
		"logo-with-text.svg",
		"logo.png",
		"logo.svg",
		"og-image.png",
		"og-image.svg",
	}
	writeAssetFiles(t, dir, files, content)
}

func writeGeneratedAssets(t *testing.T, dir, content string) {
	t.Helper()
	files := []string{
		"tui-hero.svg",
		"tui-queue.svg",
		"tui-review.svg",
		"tui-copy.svg",
		"tui-respond.svg",
		"tui-help.svg",
		"tui-address.svg",
		"cli-help.svg",
		"cli-repo-list.svg",
		"cli-status.svg",
	}
	writeAssetFiles(t, dir, files, content)
}

func writeAssetFiles(t *testing.T, dir string, files []string, content string) {
	t.Helper()
	for _, file := range files {
		path := filepath.Join(dir, file)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content+"\n"), 0o644))
	}
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
	return string(output[:len(output)-1])
}
