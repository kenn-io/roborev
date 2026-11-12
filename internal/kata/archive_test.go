package kata

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func stubArchiveIdentity(c *CLIClient, help ...string) {
	run := c.run
	c.run = func(ctx context.Context, c *CLIClient, args []string, stdin io.Reader) ([]byte, error) {
		if len(args) == 4 && args[3] == "--help" {
			if len(help) > 0 {
				return []byte(help[0]), nil
			}
			return []byte("Usage: add"), nil
		}
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
	require.NoError(t, c.ArchiveRubric(t.Context(), "demo", "uid1", "roborev-rubric.example.0123456789abcdef.md", []byte("rubric\n")))
	assert.Equal(t, 2, calls)
}

func TestArchiveRubricReportsAddFailure(t *testing.T) {
	for _, tc := range []struct {
		name, help string
		operations []string
	}{
		{"legacy", "Usage: add", []string{"ls", "add"}},
		{"reuse", "  --reuse-existing  reuse\n", []string{"add"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := NewCLIClient(t.TempDir())
			var operations []string
			c.run = func(_ context.Context, _ *CLIClient, args []string, _ io.Reader) ([]byte, error) {
				operations = append(operations, args[2])
				if args[2] == "ls" {
					return []byte(`{"attachments":{}}`), nil
				}
				return nil, ErrDocbankNotConfigured
			}
			stubArchiveIdentity(c, tc.help)
			require.ErrorIs(t, c.ArchiveRubric(t.Context(), "demo", "uid1", "rubric.md", []byte("rubric\n")), ErrDocbankNotConfigured)
			assert.Equal(t, tc.operations, operations)
		})
	}
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
	first, err := c.ResolveArchiveIssue(t.Context(), "demo", "uid1")
	require.NoError(t, err)
	second, err := c.ResolveArchiveIssue(t.Context(), "demo", "abc4")
	require.NoError(t, err)
	require.Equal(t, first.UID, second.UID)
	synctest.Test(t, func(t *testing.T) {
		firstList = make(chan struct{})
		release = make(chan struct{})
		result := make(chan error, 2)
		go func() { result <- c.ArchiveRubric(t.Context(), "demo", first.UID, "rubric.md", []byte("rubric\n")) }()
		<-firstList
		go func() { result <- c.ArchiveRubric(t.Context(), "demo", second.UID, "rubric.md", []byte("rubric\n")) }()
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
	go func() { done <- c.ArchiveRubric(t.Context(), "demo", "uid1", "rubric.md", []byte("rubric\n")) }()
	<-started
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, c.ArchiveRubric(ctx, "demo", "uid1", "rubric.md", []byte("rubric\n")), context.Canceled)
	close(release)
	require.NoError(t, <-done)
}

func TestArchiveRubricRejectsIdentityChanges(t *testing.T) {
	for _, tc := range []struct {
		name, issue, failure string
		changedResolve       int
		reuse                bool
	}{
		{"moved initially", `{"project_id":2,"uid":"uid1","short_id":"abc4"}`, "moved outside", 1, false},
		{"moved before add", `{"project_id":2,"uid":"uid1","short_id":"abc4"}`, "moved outside", 2, false},
		{"missing identity", `{"short_id":"abc4"}`, "complete identity", 1, false},
		{"changed UID before add", `{"project_id":1,"uid":"other","short_id":"abc4"}`, "identity changed", 2, false},
		{"changed UID with reuse", `{"project_id":1,"uid":"other","short_id":"abc4"}`, "identity changed", 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := NewCLIClient(t.TempDir())
			resolves, adds := 0, 0
			c.run = func(_ context.Context, _ *CLIClient, args []string, _ io.Reader) ([]byte, error) {
				if len(args) == 4 && args[3] == "--help" {
					if tc.reuse {
						return []byte("  --reuse-existing  reuse\n"), nil
					}
					return []byte("Usage: add"), nil
				}
				switch args[0] {
				case "projects":
					return []byte(`{"project":{"id":1}}`), nil
				case "show":
					resolves++
					if resolves == tc.changedResolve {
						return []byte(`{"issue":` + tc.issue + `}`), nil
					}
					return []byte(`{"issue":{"project_id":1,"uid":"uid1","short_id":"abc4"}}`), nil
				default:
					if args[2] == "add" {
						adds++
					}
					return []byte(`{"attachments":{}}`), nil
				}
			}
			err := c.ArchiveRubric(t.Context(), "demo", "uid1", "rubric.md", []byte("rubric"))
			require.ErrorContains(t, err, tc.failure)
			assert.Equal(t, tc.changedResolve, resolves)
			assert.Equal(t, 0, adds)
		})
	}
}

func TestAttachmentReuseDetection(t *testing.T) {
	for _, tc := range []struct {
		help string
		want bool
	}{
		{"  --reuse-existing   reuse an attachment\n", true},
		{"  --reuse-existing-extra  no\n", false},
		{"Use --reuse-existing to reuse\n", false},
		{"  --other string   mentions --reuse-existing\n", false},
		{"", false},
	} {
		t.Run(tc.help, func(t *testing.T) {
			c := NewCLIClient(t.TempDir())
			calls := 0
			c.run = func(_ context.Context, _ *CLIClient, args []string, _ io.Reader) ([]byte, error) {
				calls++
				assert.Equal(t, []string{"docbank", "attachments", "add", "--help"}, args)
				return []byte(tc.help), nil
			}
			got, err := c.supportsAttachmentReuse(t.Context())
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
			got, err = c.supportsAttachmentReuse(t.Context())
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, 1, calls)
		})
	}
}

func TestAttachmentReuseProbeFailureRetries(t *testing.T) {
	for _, failure := range []error{ErrUnavailable, context.Canceled} {
		t.Run(failure.Error(), func(t *testing.T) {
			c := NewCLIClient(t.TempDir())
			calls := 0
			c.run = func(_ context.Context, _ *CLIClient, _ []string, _ io.Reader) ([]byte, error) {
				calls++
				if calls == 1 {
					return nil, failure
				}
				return []byte("  --reuse-existing  reuse\n"), nil
			}
			_, err := c.supportsAttachmentReuse(t.Context())
			require.ErrorIs(t, err, failure)
			got, err := c.supportsAttachmentReuse(t.Context())
			require.NoError(t, err)
			assert.True(t, got)
			assert.Equal(t, 2, calls)
		})
	}
}

func TestArchiveRubricAtomicConcurrentClients(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		entered := make(chan []string, 2)
		release := make(chan struct{})
		results := make(chan error, 2)
		run := func(_ context.Context, _ *CLIClient, args []string, _ io.Reader) ([]byte, error) {
			if args[2] != "add" {
				return nil, fmt.Errorf("unexpected attachment command: %v", args)
			}
			data, err := os.ReadFile(args[4])
			if err != nil {
				return nil, err
			}
			assert.Equal(t, []byte("rubric\n"), data)
			entered <- append([]string(nil), args...)
			<-release
			return []byte(`{"results":[{"attachment_id":"attachment.test"}]}`), nil
		}
		for range 2 {
			c := NewCLIClient(t.TempDir())
			c.run = run
			stubArchiveIdentity(c, "  --reuse-existing  reuse\n")
			go func() { results <- c.ArchiveRubric(t.Context(), "demo", "uid1", "rubric.md", []byte("rubric\n")) }()
		}
		synctest.Wait()
		count := len(entered)
		close(release)
		require.NoError(t, <-results)
		require.NoError(t, <-results)
		require.Equal(t, 2, count)
		for range 2 {
			args := <-entered
			assert.Equal(t, []string{"docbank", "attachments", "add", "abc4", args[4], "--json", "--project", "demo", "--reuse-existing"}, args)
			assert.Equal(t, "rubric.md", filepath.Base(args[4]))
			_, err := os.Stat(args[4])
			assert.ErrorIs(t, err, os.ErrNotExist)
		}
	})
}

func TestArchiveRubricProbeUsesCallerDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := NewCLIClient(t.TempDir())
		var calls []string
		c.run = func(ctx context.Context, _ *CLIClient, args []string, _ io.Reader) ([]byte, error) {
			calls = append(calls, args[0])
			<-ctx.Done()
			return nil, ctx.Err()
		}
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		err := c.ArchiveRubric(ctx, "demo", "uid1", "rubric.md", nil)
		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.Equal(t, []string{"docbank"}, calls)
	})
}
