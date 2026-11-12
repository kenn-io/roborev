//go:build kata

package kata_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/kata"
)

// TestCLIClientE2E exercises the real kata binary against an isolated
// KATA_HOME so it never touches a developer's production kata data or
// daemon. Run with: go test -tags kata ./internal/kata/
//
// kata namespaces its database, runtime directory, and daemon socket under
// KATA_HOME (<KATA_HOME>/kata.db and <KATA_HOME>/runtime/<dbhash>), so a
// KATA_HOME-scoped `kata daemon stop` only stops the daemon this test starts.
func TestCLIClientE2E(t *testing.T) {
	if _, err := exec.LookPath("kata"); err != nil {
		t.Skip("kata not on PATH")
	}
	home := t.TempDir()
	// Remove inherited Kata routing and credentials from child processes.
	// The client appends overrides to os.Environ, so blank those same keys there.
	var childEnv, overrides []string
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "KATA_") {
			overrides = append(overrides, key+"=")
			continue
		}
		switch key {
		case "HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "CODEX_HOME", "CLAUDE_CONFIG_DIR", "PI_CODING_AGENT_DIR":
			continue
		}
		childEnv = append(childEnv, entry)
	}
	for _, key := range []string{"HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "CODEX_HOME", "CLAUDE_CONFIG_DIR", "PI_CODING_AGENT_DIR"} {
		overrides = append(overrides, key+"="+home)
	}
	overrides = append(overrides, "KATA_HOME="+home, "KATA_AUTHOR=fixture-actor")
	childEnv = append(childEnv, overrides...)
	repo := t.TempDir()

	t.Cleanup(func() {
		// Fresh bounded context: the test's ctx may already be expired or
		// canceled by the time cleanup runs.
		stopCtx, stopCancel := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Second)
		defer stopCancel()
		stop := exec.CommandContext(stopCtx, "kata", "daemon", "stop")
		stop.Dir = repo
		stop.Env = childEnv
		_ = stop.Run() // best-effort
	})

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	// Bootstrap: register the project and write the committed .kata.toml
	// binding. kata create rejects an unregistered project, so this must run
	// before any client call. Bounded by ctx so a hung kata cannot block until
	// the global test timeout.
	initCmd := exec.CommandContext(ctx, "kata", "init", "--project", "e2etest")
	initCmd.Dir = repo
	initCmd.Env = childEnv
	out, err := initCmd.CombinedOutput()
	require.NoError(t, err, "kata init: %s", out)

	c := kata.NewCLIClientWithEnv(repo, overrides)

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

	// Exercise native typed replies with separate teammate identities.
	run := func(args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, "kata", args...)
		cmd.Dir = repo
		cmd.Env = childEnv
		return cmd.CombinedOutput()
	}
	out, err = run("--teammate=lead", "comment", res.ShortID, "-m", "The draft states a different date from the exhibit.")
	require.NoError(t, err, "%s", out)
	out, err = run("show", res.ShortID, "--json")
	require.NoError(t, err, "%s", out)
	var shown struct {
		Comments []struct {
			UID string `json:"uid"`
		} `json:"comments"`
	}
	require.NoError(t, json.Unmarshal(out, &shown))
	require.Len(t, shown.Comments, 1)
	root := shown.Comments[0].UID
	require.NotEmpty(t, root)
	evidence := "The rubric checks the date and the exhibit gives a different date."
	out, err = run("--teammate=facts-codex", "comment", res.ShortID, "--confirm", root, "-m", evidence)
	require.NoError(t, err, "%s", out)
	out, err = run("--teammate=facts-claude-code", "comment", res.ShortID, "--refute", root, "-m", "The exhibit supports the draft date and the finding is mistaken.")
	require.NoError(t, err, "%s", out)
	out, err = run("--teammate=facts-codex", "comment", res.ShortID, "--confirm", root, "-m", evidence)
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr, "%s", out)
	assert.Equal(t, 5, exitErr.ExitCode(), "%s", out)
	assert.Contains(t, string(out), "a reply of this kind already exists")
}
