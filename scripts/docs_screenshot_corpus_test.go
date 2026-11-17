package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/testenv"
)

// A corpus capture must reject a database that differs from its approved
// manifest before it builds or runs anything in Docker.
func TestDocsScreenshotCaptureRejectsChangedCorpus(t *testing.T) {
	requirePython(t)
	tempDir := t.TempDir()
	database := filepath.Join(tempDir, "prepared.db")
	require.NoError(t, os.WriteFile(database, []byte("approved reviews"), 0o600))
	corpus := filepath.Join(tempDir, "corpus")
	runCorpusScript(t, "write", database, corpus)

	// An unchanged corpus yields exactly the approved database.
	copied := filepath.Join(tempDir, "copied")
	runCorpusScript(t, "extract", filepath.Join(corpus, "manifest.json"), copied)
	content, err := os.ReadFile(filepath.Join(copied, "reviews.db"))
	require.NoError(t, err)
	assert.Equal(t, "approved reviews", string(content))

	require.NoError(t, os.WriteFile(filepath.Join(corpus, "reviews.db"), []byte("changed reviews"), 0o600))
	bin := filepath.Join(tempDir, "bin")
	require.NoError(t, os.MkdirAll(bin, 0o755))
	dockerCalls := filepath.Join(tempDir, "docker-calls")
	require.NoError(t, os.WriteFile(filepath.Join(bin, "docker"),
		[]byte("#!/usr/bin/env bash\necho \"$@\" >> "+shellQuote(bashPath(t, dockerCalls))+"\n"), 0o755))

	script := filepath.Join("..", "docs", "screenshots", "build-generated-assets.sh")
	cmd := exec.CommandContext(t.Context(), "bash", "-c",
		"export PATH="+shellQuote(bashPath(t, bin))+":\"$PATH\""+
			" DOCS_SCREENSHOTS_CORPUS="+shellQuote(bashPath(t, filepath.Join(corpus, "manifest.json")))+
			"; exec bash "+shellQuote(bashPath(t, script))+" "+shellQuote(bashPath(t, filepath.Join(tempDir, "output"))))
	cmd.Env = testenv.UserEnv()
	output, err := cmd.CombinedOutput()

	require.Error(t, err, string(output))
	assert.Contains(t, string(output), "reviews.db differs from the corpus manifest")
	assert.NoFileExists(t, dockerCalls, "a changed corpus must not reach Docker")
	assert.NoDirExists(t, filepath.Join(tempDir, "output", "assets"))
}

func runCorpusScript(t *testing.T, args ...string) {
	t.Helper()
	command := "exec python3 " + shellQuote(bashPath(t, filepath.Join("..", "docs", "screenshots", "corpus.py")))
	for _, arg := range args {
		command += " " + shellQuote(bashPath(t, arg))
	}
	cmd := exec.CommandContext(t.Context(), "bash", "-c", command)
	cmd.Env = testenv.UserEnv()
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
}

// requirePython skips where python3 is missing; capture workers and the
// Linux and macOS runners provide it.
func requirePython(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is not on PATH")
	}
}
