package kata

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func stubArchiveIdentity(c *CLIClient) {
	run := c.run
	c.run = func(ctx context.Context, c *CLIClient, args []string, stdin io.Reader) ([]byte, error) {
		switch args[0] {
		case "projects":
			return []byte(`{"project":{"id":1}}`), nil
		case "show":
			return []byte(`{"issue":{"project_id":1,"uid":"uid1","short_id":"abc4"}}`), nil
		default:
			return run(ctx, c, args, stdin)
		}
	}
}

func TestResolveArchiveIssueRequiresCurrentProjectIdentity(t *testing.T) {
	for _, tc := range []struct {
		name  string
		issue string
		want  kataArchiveIssueExpectation
	}{
		{"current", `{"project_id":1,"uid":"uid1","short_id":"new1"}`, kataArchiveIssueExpectation{uid: "uid1", short: "new1"}},
		{"moved", `{"project_id":2,"uid":"uid1","short_id":"new1"}`, kataArchiveIssueExpectation{failure: "moved outside"}},
		{"missing identity", `{"short_id":"new1"}`, kataArchiveIssueExpectation{failure: "complete identity"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := NewCLIClient(t.TempDir())
			c.run = func(_ context.Context, _ *CLIClient, args []string, _ io.Reader) ([]byte, error) {
				if args[0] == "projects" {
					return []byte(`{"project":{"id":1}}`), nil
				}
				assert.Equal(t, []string{"show", "old1", "--json", "--project", "demo"}, args)
				return []byte(`{"issue":` + tc.issue + `}`), nil
			}
			issue, err := c.ResolveArchiveIssue(t.Context(), "demo", "old1")
			if tc.want.failure != "" {
				require.ErrorContains(t, err, tc.want.failure)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want.uid, issue.UID)
			assert.Equal(t, tc.want.short, issue.ShortID)
		})
	}
}

type kataArchiveIssueExpectation struct{ uid, short, failure string }

func TestArchiveRubricListsAndAddsExactBytes(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".kata.toml"), []byte("[project]\nname = \"demo\"\n"), 0o600))
	c := NewCLIClient(dir)
	calls := 0
	c.run = func(_ context.Context, _ *CLIClient, args []string, _ io.Reader) ([]byte, error) {
		calls++
		if calls == 1 {
			assert.Equal(t, []string{"docbank", "attachments", "ls", "abc4", "--json", "--project", "demo"}, args)
			return []byte(`{"attachments":{}}`), nil
		}
		assert.Equal(t, []string{"docbank", "attachments", "add", "abc4", args[4], "--json", "--project", "demo"}, args)
		assert.Equal(t, "roborev-rubric.example.0123456789abcdef.md", filepath.Base(args[4]))
		data, err := os.ReadFile(args[4])
		require.NoError(t, err)
		assert.Equal(t, []byte("rubric\n"), data)
		return []byte(`{"results":[{"attachment_id":"attachment.test"}]}`), nil
	}
	stubArchiveIdentity(c)
	require.NoError(t, c.ArchiveRubric(t.Context(), "demo", "abc4", "roborev-rubric.example.0123456789abcdef.md", []byte("rubric\n")))
	assert.Equal(t, 2, calls)
}

func TestArchiveRubricSkipsExisting(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".kata.toml"), []byte("[project]\nname = \"demo\"\n"), 0o600))
	c := NewCLIClient(dir)
	c.run = func(_ context.Context, _ *CLIClient, _ []string, _ io.Reader) ([]byte, error) {
		return []byte(`{"attachments":{"attachment.test":{"name":"rubric.md"}}}`), nil
	}
	stubArchiveIdentity(c)
	require.NoError(t, c.ArchiveRubric(t.Context(), "demo", "abc4", "rubric.md", []byte("rubric\n")))
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
	stubArchiveIdentity(c)
	require.ErrorContains(t, c.ArchiveRubric(t.Context(), "demo", "abc4", "rubric.md", []byte("rubric\n")), "docbank_not_configured")
}

func TestArchiveRubricSerializesConcurrentAdds(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".kata.toml"), []byte("[project]\nname = \"demo\"\n"), 0o600))
	c := NewCLIClient(dir)
	var firstList, release chan struct{}
	calls := 0
	adds := 0
	var mu sync.Mutex
	c.run = func(_ context.Context, _ *CLIClient, args []string, _ io.Reader) ([]byte, error) {
		if args[2] == "ls" {
			mu.Lock()
			calls++
			first := calls == 1
			added := adds > 0
			mu.Unlock()
			if first {
				close(firstList)
				<-release
			}
			if added {
				return []byte(`{"attachments":{"one":{"name":"rubric.md"}}}`), nil
			}
			return []byte(`{"attachments":{}}`), nil
		}
		mu.Lock()
		adds++
		mu.Unlock()
		return []byte(`{"results":[{"attachment_id":"attachment.test"}]}`), nil
	}
	stubArchiveIdentity(c)
	synctest.Test(t, func(t *testing.T) {
		firstList = make(chan struct{})
		release = make(chan struct{})
		result := make(chan error, 2)
		go func() { result <- c.ArchiveRubric(t.Context(), "demo", "uid1", "rubric.md", []byte("rubric\n")) }()
		<-firstList
		go func() { result <- c.ArchiveRubric(t.Context(), "demo", "abc4", "rubric.md", []byte("rubric\n")) }()
		synctest.Wait()
		close(release)
		require.NoError(t, <-result)
		require.NoError(t, <-result)
	})
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
	stubArchiveIdentity(c)
	done := make(chan error, 1)
	go func() { done <- c.ArchiveRubric(t.Context(), "demo", "abc4", "rubric.md", []byte("rubric\n")) }()
	<-started
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, c.ArchiveRubric(ctx, "demo", "abc4", "rubric.md", []byte("rubric\n")), context.Canceled)
	close(release)
	require.NoError(t, <-done)
}
