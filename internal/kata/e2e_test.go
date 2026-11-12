//go:build kata

package kata_test

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/kata"
	"go.kenn.io/roborev/internal/kata/katatest"
)

// TestCLIClientE2EIgnoresInheritedKataRouting proves the isolation: with
// KATA_SERVER pointing at a closed port and a token and identity in the
// environment, the full client flow still reaches only the disposable daemon.
func TestCLIClientE2EIgnoresInheritedKataRouting(t *testing.T) {
	if _, err := exec.LookPath("kata"); err != nil {
		t.Skip("kata not on PATH")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	closedAddr := listener.Addr().String()
	require.NoError(t, listener.Close())

	t.Setenv("KATA_SERVER", "http://"+closedAddr)
	t.Setenv("KATA_AUTH_TOKEN", "inherited-token")
	t.Setenv("KATA_AUTHOR", "inherited-author")
	t.Setenv("KATA_TEAMMATE", "inherited-teammate")
	t.Setenv("KATA_HOME", t.TempDir())
	runCLIClientE2E(t)
}

// TestCLIClientE2E exercises the real kata binary against an isolated home.
// Run with: go test -tags kata ./internal/kata/.
func TestCLIClientE2E(t *testing.T) {
	runCLIClientE2E(t)
}

func runCLIClientE2E(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	k := katatest.StartReal(t, ctx)
	c := kata.NewCLIClientWithEnv(k.Repo, k.Env)

	// Binding reads the .kata.toml that `kata init` wrote.
	b, err := c.Binding(ctx)
	require.NoError(t, err)
	assert.Equal(t, "e2etest", b.Project)

	// Create, then create again with the same idempotency key.
	req := kata.CreateReq{Title: "seed task", Body: "do the thing", Project: "e2etest", IdempotencyKey: "e2e:1"}
	res, err := c.Create(ctx, req)
	require.NoError(t, err)
	require.NotEmpty(t, res.ShortID)
	assert.False(t, res.Reused)

	res2, err := c.Create(ctx, req)
	require.NoError(t, err)
	assert.True(t, res2.Reused, "same idempotency-key must reuse")
	assert.Equal(t, res.ShortID, res2.ShortID)

	// List returns the created issue.
	issues, err := c.List(ctx, kata.ListOpts{Status: "open"})
	require.NoError(t, err)
	var found bool
	for _, iss := range issues {
		if iss.ShortID == res.ShortID {
			found = true
		}
	}
	assert.True(t, found, "created issue should appear in open list")

	// Show returns the issue detail.
	iss, err := c.Show(ctx, res.ShortID)
	require.NoError(t, err)
	assert.Equal(t, "seed task", iss.Title)
	assert.Contains(t, iss.Body, "do the thing")
}

// This contract test is opt-in: go test -tags kata ./internal/kata/.
func TestNativeTypedRepliesE2E(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	k := katatest.StartReal(t, ctx)
	repo, childEnv := k.Repo, k.Env
	c := kata.NewCLIClientWithEnv(repo, childEnv)
	res, err := c.Create(ctx, kata.CreateReq{Title: "synthetic review", Project: "e2etest"})
	require.NoError(t, err)
	body := "The draft states a different date from the exhibit."
	require.NoError(t, os.WriteFile(filepath.Join(repo, "finding.md"), []byte(body), 0o644))
	run := func(args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, "kata", args...)
		cmd.Dir = repo
		cmd.Env = childEnv
		return cmd.CombinedOutput()
	}
	rootArgs := []string{"--teammate=lead", "comment", res.ShortID, "--body-file", "finding.md", "--idempotency-key", "swarm:fixture:f1", "--agent"}
	out, err := run(rootArgs...)
	require.NoError(t, err, "%s", out)
	out, err = run(rootArgs...)
	require.NoError(t, err, "%s", out)
	out, err = run("--teammate=lead", "show", res.ShortID, "--json")
	require.NoError(t, err, "%s", out)
	var shown struct {
		Comments []struct {
			Handle string `json:"handle"`
		} `json:"comments"`
	}
	require.NoError(t, json.Unmarshal(out, &shown))
	require.Len(t, shown.Comments, 1)
	require.NotEmpty(t, shown.Comments[0].Handle)
	root := shown.Comments[0].Handle
	evidence := "The rubric checks the date and the exhibit gives a different date."
	confirm := []string{"--teammate=facts-codex", "comment", res.ShortID, "--confirm", root, "-m", evidence, "--idempotency-key", "swarm:fixture:f1:facts-codex:confirm", "--agent"}
	out, err = run(confirm...)
	require.NoError(t, err, "%s", out)
	out, err = run(confirm...)
	require.NoError(t, err, "%s", out)
	out, err = run("--teammate=facts-claude-code", "comment", res.ShortID, "--refute", root, "-m", "The exhibit supports the draft date and the finding is mistaken.", "--idempotency-key", "swarm:fixture:f1:facts-claude-code:refute", "--agent")
	require.NoError(t, err, "%s", out)
	out, err = run("--teammate=lead", "show", res.ShortID, "--thread", root, "--agent")
	require.NoError(t, err, "%s", out)
	assert.Equal(t, 1, strings.Count(string(out), evidence), "identical retry must not duplicate the reply")
	assert.Contains(t, string(out), "The exhibit supports the draft date")
	out, err = run("--teammate=facts-codex", "comment", res.ShortID, "--confirm", root, "-m", evidence, "--idempotency-key", "swarm:fixture:f1:duplicate", "--json")
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr, "%s", out)
	assert.Equal(t, 5, exitErr.ExitCode(), "%s", out)
	var conflict struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(out, &conflict))
	assert.Equal(t, "duplicate_reply", conflict.Error.Code)
}
