package kata

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/kata/katafake"
)

func configWithKata(k config.KataIntegrationConfig) *config.Config {
	cfg := config.DefaultConfig()
	cfg.Integrations.Kata = k
	return cfg
}

func TestNewFromConfigKeepsTheCLIClientUnlessEnabled(t *testing.T) {
	dir := t.TempDir()
	for name, cfg := range map[string]*config.Config{
		"no config":         nil,
		"default config":    config.DefaultConfig(),
		"disabled settings": configWithKata(config.KataIntegrationConfig{Endpoint: "https://kata.example.com", APIKey: "k", DefaultProject: "p"}),
	} {
		t.Run(name, func(t *testing.T) {
			c, ok := NewFromConfig(dir, cfg).(*CLIClient)
			require.True(t, ok, "a disabled connection must keep today's CLI client")
			assert.Equal(t, dir, c.workdir)
			assert.Equal(t, "kata", c.bin)
			assert.Nil(t, c.env)
		})
	}
}

// A misconfigured connection must fail every call visibly. Falling back to the
// kata CLI would send the work to whatever Kata the process environment picks.
func TestNewFromConfigNeverFallsBackToTheCLIOnABadConfiguration(t *testing.T) {
	bad := kataConfigFor("ftp://kata.example.com")
	c := NewFromConfig(t.TempDir(), configWithKata(bad))
	_, isCLI := c.(*CLIClient)
	require.False(t, isCLI)

	_, err := c.Binding(t.Context())
	require.ErrorIs(t, err, ErrInsecureEndpoint)
	_, err = c.List(t.Context(), ListOpts{})
	require.ErrorIs(t, err, ErrInsecureEndpoint)
	_, err = c.Show(t.Context(), "abc4")
	require.ErrorIs(t, err, ErrInsecureEndpoint)
	_, err = c.Create(t.Context(), CreateReq{Title: "t"})
	require.ErrorIs(t, err, ErrInsecureEndpoint)
	_, err = c.Comment(t.Context(), CommentReq{Issue: "abc4", Body: "b", IdempotencyKey: "k"})
	require.ErrorIs(t, err, ErrInsecureEndpoint)
	_, err = c.Thread(t.Context(), ThreadReq{Issue: "abc4", Root: "C0000000000000000000000001"})
	require.ErrorIs(t, err, ErrInsecureEndpoint)

	missingKey := kataConfigFor("https://kata.example.com")
	missingKey.APIKey, missingKey.APIKeyFile = "", filepath.Join(t.TempDir(), "missing")
	_, err = NewFromConfig(t.TempDir(), configWithKata(missingKey)).List(t.Context(), ListOpts{})
	require.ErrorIs(t, err, ErrAuthenticationRequired)
}

type recordedRun struct {
	stdin string
}

// recordingCLI returns the client NewFromConfig builds for a disabled
// connection with its process runner replaced by a recorder.
func recordingCLI(t *testing.T, cfg *config.Config) (Client, *[]recordedRun) {
	t.Helper()
	c, ok := NewFromConfig(t.TempDir(), cfg).(*CLIClient)
	require.True(t, ok)
	var runs []recordedRun
	c.run = func(_ context.Context, _ *CLIClient, _ []string, stdin io.Reader) ([]byte, error) {
		var in string
		if stdin != nil {
			data, _ := io.ReadAll(stdin)
			in = string(data)
		}
		runs = append(runs, recordedRun{stdin: in})
		return []byte(`{"issues":[{"short_id":"abc4","body":"b"}],"issue":{"short_id":"abc4"},"reused":false}`), nil
	}
	return c, &runs
}

func TestDisabledConnectionPassesCreateBodyOnStdin(t *testing.T) {
	disabled := configWithKata(config.KataIntegrationConfig{Endpoint: "https://kata.example.com", APIKey: "k", DefaultProject: "p"})
	body := "review context stays on standard input"
	c, runs := recordingCLI(t, disabled)
	_, err := c.Create(t.Context(), CreateReq{Title: "t", Body: body})
	require.NoError(t, err)
	require.Len(t, *runs, 1)
	assert.Equal(t, body, (*runs)[0].stdin)
}

// With the connection enabled nothing may run the kata CLI. A kata on PATH
// that records its own invocation proves it is never started.
func TestEnabledConnectionNeverRunsTheKataCLI(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script as the kata executable")
	}
	bin := t.TempDir()
	marker := filepath.Join(t.TempDir(), "kata-was-run")
	script := "#!/bin/sh\necho \"$@\" >> " + marker + "\nexit 1\n"
	require.NoError(t, os.WriteFile(filepath.Join(bin, "kata"), []byte(script), 0o755))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	f := katafake.New(t)
	f.AddIssue(7, "abc4", "T", "B")
	cfg := configWithKata(kataConfigFor(f.URL()))
	work := bindWorkspace(t, "reviews")
	c := NewFromConfig(work, cfg)

	_, err := c.Binding(t.Context())
	require.NoError(t, err)
	_, err = c.List(t.Context(), ListOpts{Unlimited: true})
	require.NoError(t, err)
	_, err = c.Show(t.Context(), "abc4")
	require.NoError(t, err)
	_, err = c.Create(t.Context(), CreateReq{Title: "t", IdempotencyKey: "k"})
	require.NoError(t, err)

	broken := NewFromConfig(work, configWithKata(kataConfigFor("ftp://x")))
	_, _ = broken.List(t.Context(), ListOpts{})
	_, _ = broken.Create(t.Context(), CreateReq{Title: "t"})

	_, statErr := os.Stat(marker)
	require.ErrorIs(t, statErr, os.ErrNotExist, "the kata CLI was started: %s", func() string { b, _ := os.ReadFile(marker); return strings.TrimSpace(string(b)) }())
}

func TestAPIClientWithoutWorkspaceIgnoresBindingFiles(t *testing.T) {
	f := katafake.New(t)
	dir := bindWorkspace(t, "other")
	t.Chdir(dir)
	cfg := kataConfigFor(f.URL())
	c, err := NewAPIClient("", cfg, WithoutWorkspace())
	require.NoError(t, err)
	b, err := c.Binding(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "reviews", b.Project, "the daemon's own directory must not select a project")

	cfg.DefaultProject = ""
	c, err = NewAPIClient("", cfg, WithoutWorkspace())
	require.NoError(t, err)
	_, err = c.Binding(t.Context())
	require.ErrorIs(t, err, ErrNoBinding)
}

func TestAPIClientListProjectReadsTheNamedProject(t *testing.T) {
	f := katafake.New(t)
	f.AddIssue(7, "aaa1", "In reviews", "")
	f.AddIssue(8, "bbb2", "In other", "")
	c, err := NewAPIClient("", kataConfigFor(f.URL()), WithoutWorkspace())
	require.NoError(t, err)

	issues, err := c.ListProject(t.Context(), "other", ListOpts{})
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, "bbb2", issues[0].ShortID)

	_, err = c.ListProject(t.Context(), "ghost", ListOpts{})
	require.ErrorIs(t, err, ErrProjectNotFound)
	issues, err = c.ListProject(t.Context(), "", ListOpts{})
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, "aaa1", issues[0].ShortID, "an empty project is the default one")
}
