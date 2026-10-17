package agent

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMuseBuildArgs(t *testing.T) {
	for _, tc := range []struct {
		name    string
		agentic bool
		unsafe  bool
		model   string
		want    []string
	}{
		{"review", false, false, "", []string{"--trust-workspace", "--disable-write", "--disable-shell", "--disable-web-tools"}},
		{"agentic", true, false, "", []string{"--yolo"}},
		{"unsafe", false, true, "muse-spark-1.3-contributor", []string{"--yolo"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withUnsafeAgents(t, tc.unsafe)
			a := NewMuseAgent("").WithModel(tc.model).WithAgentic(tc.agentic).(*MuseAgent)
			model := "muse-spark-1.3"
			if tc.model != "" {
				model = tc.model
			}
			want := append([]string{"exec", "--no-session-log", "--contributor-mode", "off", "--model", model}, tc.want...)
			want = append(want, "--prompt-file", "prompt.md")
			assert.Equal(t, want, a.buildArgs(effectiveAgentic(t.Context(), a.Agentic), "prompt.md"))
			assert.Contains(t, a.CommandLine(), "--prompt-file <prompt-file>")
		})
	}
}

func TestMuseReviewPromptAndOutput(t *testing.T) {
	skipIfWindows(t)
	capturePath := filepath.Join(t.TempDir(), "prompt-path")
	t.Setenv("MUSE_PROMPT_CAPTURE", capturePath)
	command := writeTempCommand(t, `#!/bin/sh
case "$1" in *etxtbsy*) exit 0;; esac
prev=""
for arg in "$@"; do
 if [ "$prev" = "--prompt-file" ]; then
  test -f "$arg" || exit 2
  test "$(cat "$arg")" = 'prompt body' || exit 3
  printf '%s' "$arg" > "$MUSE_PROMPT_CAPTURE"
 fi
 prev="$arg"
done
printf 'Reading files.\n\nNo issues found.\n'
printf 'status message\n' >&2
`)
	var output bytes.Buffer
	result, err := NewMuseAgent(command).Review(t.Context(), t.TempDir(), "HEAD", "prompt body", &output)
	require.NoError(t, err)
	assert := assert.New(t)
	assert.Equal("Reading files.\n\nNo issues found.", result)
	assert.Contains(output.String(), "Reading files.\n\nNo issues found.\n")
	assert.Contains(output.String(), "status message\n")
	assert.NotContains(result, "status message")
	path, err := os.ReadFile(capturePath)
	require.NoError(t, err)
	_, err = os.Stat(string(path))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestMuseReviewFailure(t *testing.T) {
	command := writeTempCommand(t, `#!/bin/sh
case "$1" in *etxtbsy*) exit 0;; esac
printf 'run ended with Failed: unknown model\n' >&2
exit 1
`)
	_, err := NewMuseAgent(command).Review(t.Context(), t.TempDir(), "HEAD", "review", nil)
	require.ErrorContains(t, err, "muse failed:")
	require.ErrorContains(t, err, "run ended with Failed: unknown model")
}

func TestMuseReviewEmptyOutput(t *testing.T) {
	command := writeTempCommand(t, "#!/bin/sh\nprintf '  \\n'\n")
	result, err := NewMuseAgent(command).Review(t.Context(), t.TempDir(), "HEAD", "review", nil)
	require.NoError(t, err)
	assert.Equal(t, "No review output generated", result)
}
