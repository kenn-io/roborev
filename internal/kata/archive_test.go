package kata

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestArchiveRubricListsAndAddsExactBytes(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".kata.toml"), []byte("[project]\nname = \"demo\"\n"), 0o600))
	c := NewCLIClient(dir)
	calls := 0
	c.run = func(_ context.Context, _ *CLIClient, args []string, _ io.Reader) ([]byte, error) {
		calls++
		if calls == 1 {
			return []byte(`{"attachments":{}}`), nil
		}
		assert.Equal(t, "roborev-rubric.example.0123456789abcdef.md", filepath.Base(args[4]))
		data, err := os.ReadFile(args[4])
		require.NoError(t, err)
		assert.Equal(t, []byte("rubric\n"), data)
		return []byte(`{"results":[{"attachment_id":"attachment.test"}]}`), nil
	}
	require.NoError(t, c.ArchiveRubric(t.Context(), "abc4", "roborev-rubric.example.0123456789abcdef.md", []byte("rubric\n")))
	assert.Equal(t, 2, calls)
}

func TestArchiveRubricSkipsExisting(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".kata.toml"), []byte("[project]\nname = \"demo\"\n"), 0o600))
	c := NewCLIClient(dir)
	c.run = func(_ context.Context, _ *CLIClient, _ []string, _ io.Reader) ([]byte, error) {
		return []byte(`{"attachments":{"attachment.test":{"name":"rubric.md"}}}`), nil
	}
	require.NoError(t, c.ArchiveRubric(t.Context(), "abc4", "rubric.md", []byte("rubric\n")))
}

func TestArchiveRubricReportsAddFailure(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".kata.toml"), []byte("[project]\nname = \"demo\"\n"), 0o600))
	c := NewCLIClient(dir)
	calls := 0
	c.run = func(_ context.Context, _ *CLIClient, _ []string, _ io.Reader) ([]byte, error) {
		calls++
		if calls == 1 {
			return []byte(`{"attachments":{}}`), nil
		}
		return []byte(`{"results":[{"error":"docbank_not_configured"}]}`), nil
	}
	require.ErrorContains(t, c.ArchiveRubric(t.Context(), "abc4", "rubric.md", []byte("rubric\n")), "docbank_not_configured")
}

func TestArchiveRubricSerializesConcurrentAdds(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".kata.toml"), []byte("[project]\nname = \"demo\"\n"), 0o600))
	c := NewCLIClient(dir)
	firstList := make(chan struct{})
	release := make(chan struct{})
	calls := 0
	adds := 0
	c.run = func(_ context.Context, _ *CLIClient, args []string, _ io.Reader) ([]byte, error) {
		if args[2] == "ls" {
			calls++
			if calls == 1 {
				close(firstList)
				<-release
				return []byte(`{"attachments":{}}`), nil
			}
			return []byte(`{"attachments":{"one":{"name":"rubric.md"}}}`), nil
		}
		adds++
		return []byte(`{"results":[{"attachment_id":"attachment.test"}]}`), nil
	}
	result := make(chan error, 2)
	go func() { result <- c.ArchiveRubric(t.Context(), "abc4", "rubric.md", []byte("rubric\n")) }()
	<-firstList
	go func() { result <- c.ArchiveRubric(t.Context(), "abc4", "rubric.md", []byte("rubric\n")) }()
	close(release)
	require.NoError(t, <-result)
	require.NoError(t, <-result)
	assert.Equal(t, 2, calls)
	assert.Equal(t, 1, adds)
}

func TestArchiveRubricCancelWaitingForLock(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".kata.toml"), []byte("[project]\nname = \"demo\"\n"), 0o600))
	c := NewCLIClient(dir)
	started := make(chan struct{})
	release := make(chan struct{})
	c.run = func(_ context.Context, _ *CLIClient, args []string, _ io.Reader) ([]byte, error) {
		if args[2] == "ls" {
			close(started)
			<-release
			return []byte(`{"attachments":{"one":{"name":"rubric.md"}}}`), nil
		}
		return nil, nil
	}
	done := make(chan error, 1)
	go func() { done <- c.ArchiveRubric(t.Context(), "abc4", "rubric.md", []byte("rubric\n")) }()
	<-started
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, c.ArchiveRubric(ctx, "abc4", "rubric.md", []byte("rubric\n")), context.Canceled)
	close(release)
	require.NoError(t, <-done)
}
