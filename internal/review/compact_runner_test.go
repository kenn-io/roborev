package review

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
)

type compactReadOnlyAgent struct {
	agent.FakeAgent
	schema  jsontext.Value
	agentic bool
}

func (a *compactReadOnlyAgent) SupportsReadOnlyReview() bool                   { return true }
func (a *compactReadOnlyAgent) WithAgentic(v bool) agent.Agent                 { a.agentic = v; return a }
func (a *compactReadOnlyAgent) WithModel(string) agent.Agent                   { return a }
func (a *compactReadOnlyAgent) WithReasoning(agent.ReasoningLevel) agent.Agent { return a }

type compactNativeAgent struct{ *compactReadOnlyAgent }

func (a *compactNativeAgent) ReviewWithSchema(ctx context.Context, repo, ref, prompt string, schema jsontext.Value, out io.Writer) (jsontext.Value, error) {
	a.schema = schema
	raw, err := a.Review(ctx, repo, ref, prompt, out)
	return jsontext.Value(raw), err
}
func (a *compactNativeAgent) WithAgentic(v bool) agent.Agent { a.agentic = v; return a }

func TestCompactRunnerRejectsUnsupportedBeforeInvocation(t *testing.T) {
	repo, manifest := compactFixture(t)
	called := false
	a := &agent.FakeAgent{NameStr: "unsupported", ReviewFn: func(context.Context, string, string, string, io.Writer) (string, error) {
		called = true
		return "", nil
	}}
	_, _, err := RunCompactReview(t.Context(), a, repo, "HEAD", "prompt", "", nil, manifest, nil)
	require.ErrorContains(t, err, "read-only")
	assert.False(t, called)
}

func TestCompactRunnerNativeAndPromptOnlyAgree(t *testing.T) {
	repo, manifest := compactFixture(t)
	for _, native := range []bool{false, true} {
		a := &compactReadOnlyAgent{NameStr: "fixture", ReviewFn: func(context.Context, string, string, string, io.Writer) (string, error) {
			return `{"version":1,"summary":"uncertain","decisions":[]}`, nil
		}}
		var adapter agent.Agent = a
		if native {
			adapter = &compactNativeAgent{a}
		}
		got, eligible, err := RunCompactReview(t.Context(), adapter, repo, "HEAD", "prompt", "medium", nil, manifest, nil)
		require.NoError(t, err)
		assert.Len(t, got.Structured.Findings, 2)
		assert.Empty(t, eligible)
		assert.False(t, a.agentic)
		if native {
			assert.JSONEq(t, string(compactSchema), string(a.schema))
		}
	}
}

func TestCompactRunnerMalformedOutputFails(t *testing.T) {
	repo, manifest := compactFixture(t)
	for _, raw := range []string{`not JSON`, `{"version":1,"decisions":"not an array"}`, `{"version":1,"decisions":[],"extra":true}`, `{"version":2,"decisions":[]}`} {
		a := &compactReadOnlyAgent{NameStr: "fixture", ReviewFn: func(context.Context, string, string, string, io.Writer) (string, error) { return raw, nil }}
		got, eligible, err := RunCompactReview(t.Context(), a, repo, "HEAD", "prompt", "", nil, manifest, nil)
		require.Error(t, err)
		assert.Nil(t, got.Structured)
		assert.Empty(t, eligible)
	}
}

func TestCompactRunnerTimeoutNeverProducesClosureEligibility(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		repo, manifest := compactFixture(t)
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		a := &compactReadOnlyAgent{NameStr: "fixture", ReviewFn: func(ctx context.Context, _ string, _ string, _ string, _ io.Writer) (string, error) {
			<-ctx.Done()
			return "", ctx.Err()
		}}
		_, eligible, err := RunCompactReview(ctx, a, repo, "HEAD", "prompt", "", nil, manifest, nil)
		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.Empty(t, eligible)
	})
}

func TestCompactRunnerProviderFailurePreservesSources(t *testing.T) {
	repo, manifest := compactFixture(t)
	a := &compactReadOnlyAgent{NameStr: "fixture", ReviewFn: func(context.Context, string, string, string, io.Writer) (string, error) {
		return "", errors.New("synthetic provider failure")
	}}
	_, eligible, err := RunCompactReview(t.Context(), a, repo, "HEAD", "prompt", "", nil, manifest, nil)
	require.ErrorContains(t, err, "provider failure")
	assert.Empty(t, eligible)
}

func TestCompactReadOnlyOverridesUnsafePi(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fixture command")
	}
	repo, manifest := compactFixture(t)
	command := filepath.Join(t.TempDir(), "fixture-pi")
	argsFile := filepath.Join(t.TempDir(), "args")
	// Controlled executable records real adapter flags and supplies one complete
	// decision response. It never reads account data or contacts a provider.
	script := fmt.Sprintf(`#!/bin/sh
: > %q
prev=""
for arg in "$@"; do
 printf '%%s\n' "$arg" >> %q
 if [ "$prev" = "--json-output" ]; then printf '%%s\n' '{"version":1,"summary":"uncertain","decisions":[]}' > "$arg"; fi
 prev="$arg"
done
`, argsFile, argsFile)
	require.NoError(t, os.WriteFile(command, []byte(script), 0o700))
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PI_CODING_AGENT_DIR", t.TempDir())
	previous := agent.AllowUnsafeAgents()
	agent.SetAllowUnsafeAgents(true)
	t.Cleanup(func() { agent.SetAllowUnsafeAgents(previous) })
	a := agent.NewPiAgent(command)
	a.Agentic = true
	_, _, err := RunCompactReview(t.Context(), a, repo, "HEAD", "prompt", "", nil, manifest, nil)
	require.NoError(t, err)
	args, err := os.ReadFile(argsFile)
	require.NoError(t, err)
	assert.Contains(t, string(args), "--tools\nread,grep,find,ls,json_output\n")
	assert.Contains(t, string(args), "--no-extensions")
}
