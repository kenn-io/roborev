package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/review/autotype"
)

type fakeSchemaAgent struct {
	name        string
	commandLine string
	result      json.RawMessage
	err         error
	logOutput   string
}

func (f *fakeSchemaAgent) Name() string {
	if f.name != "" {
		return f.name
	}
	return "fake"
}

func (f *fakeSchemaAgent) Review(context.Context, string, string, string, io.Writer) (string, error) {
	return "", nil
}
func (f *fakeSchemaAgent) WithReasoning(agent.ReasoningLevel) agent.Agent { return f }
func (f *fakeSchemaAgent) WithAgentic(bool) agent.Agent                   { return f }
func (f *fakeSchemaAgent) WithModel(string) agent.Agent                   { return f }
func (f *fakeSchemaAgent) CommandLine() string {
	if f.commandLine != "" {
		return f.commandLine
	}
	return "fake"
}

func (f *fakeSchemaAgent) ClassifyWithSchema(
	_ context.Context,
	_, _, _ string,
	_ json.RawMessage,
	out io.Writer,
) (json.RawMessage, error) {
	if f.logOutput != "" && out != nil {
		if _, err := io.WriteString(out, f.logOutput); err != nil {
			return nil, err
		}
	}
	return f.result, f.err
}

func TestClassifierAdapter_Yes(t *testing.T) {
	ad := newClassifierAdapter(&fakeSchemaAgent{
		result: []byte(`{"design_review": true, "reason": "new package"}`),
	}, 20*1024, nil)
	yes, reason, err := ad.Decide(context.Background(), autotype.Input{})
	require.NoError(t, err)
	assert.True(t, yes)
	assert.Equal(t, "new package", reason)
}

func TestClassifierAdapter_No(t *testing.T) {
	ad := newClassifierAdapter(&fakeSchemaAgent{
		result: []byte(`{"design_review": false, "reason": "local fix"}`),
	}, 20*1024, nil)
	yes, reason, err := ad.Decide(context.Background(), autotype.Input{})
	require.NoError(t, err)
	assert.False(t, yes)
	assert.Equal(t, "local fix", reason)
}

func TestClassifierAdapter_ForwardsProgressOutput(t *testing.T) {
	var out strings.Builder
	ad := newClassifierAdapter(&fakeSchemaAgent{
		result:    []byte(`{"design_review": false, "reason": "local fix"}`),
		logOutput: "classifier progress\n",
	}, 20*1024, &out)

	yes, reason, err := ad.Decide(context.Background(), autotype.Input{})

	require.NoError(t, err)
	assert.False(t, yes)
	assert.Equal(t, "local fix", reason)
	assert.Equal(t, "classifier progress\n", out.String())
}

func TestClassifierAdapter_AgentError(t *testing.T) {
	ad := newClassifierAdapter(&fakeSchemaAgent{err: errors.New("boom")}, 20*1024, nil)
	_, _, err := ad.Decide(context.Background(), autotype.Input{})
	assert.ErrorContains(t, err, "boom")
}

func TestClassifierAdapter_InvalidJSON(t *testing.T) {
	ad := newClassifierAdapter(&fakeSchemaAgent{result: []byte(`not json`)}, 20*1024, nil)
	_, _, err := ad.Decide(context.Background(), autotype.Input{})
	assert.ErrorContains(t, err, "invalid")
}

func TestClassifierAdapter_SanitizesReason_Length(t *testing.T) {
	long := strings.Repeat("a", 1000)
	ad := newClassifierAdapter(&fakeSchemaAgent{
		result: []byte(`{"design_review":false,"reason":"` + long + `"}`),
	}, 20*1024, nil)
	_, reason, err := ad.Decide(context.Background(), autotype.Input{})
	require.NoError(t, err)
	assert.LessOrEqual(t, len(reason), classifyReasonMaxLen)
}

func TestClassifierAdapter_SanitizesReason_StripsControlChars(t *testing.T) {
	ad := newClassifierAdapter(&fakeSchemaAgent{
		result: []byte(`{"design_review":false,"reason":"hello\u0007world\nlocal"}`),
	}, 20*1024, nil)
	_, reason, err := ad.Decide(context.Background(), autotype.Input{})
	require.NoError(t, err)
	// BEL control char dropped; \n folded to space.
	assert.NotContains(t, reason, "\x07")
	assert.NotContains(t, reason, "\n")
	assert.Contains(t, reason, "hello")
	assert.Contains(t, reason, "world")
}

func TestClassifierAdapter_RespectsMaxBytes(t *testing.T) {
	// MaxBytes must be large enough to hold the system prompt (~1.5KB
	// after the prompt-injection hardening); 4KB is well above that
	// while still smaller than the 6KB Diff below, so truncation is
	// exercised.
	ad := newClassifierAdapter(&fakeSchemaAgent{
		result: []byte(`{"design_review": false, "reason": "ok"}`),
	}, 4096, nil)
	_, _, err := ad.Decide(context.Background(), autotype.Input{
		Diff:    strings.Repeat("+line\n", 1000),
		Message: "feat: something",
	})
	require.NoError(t, err)
}
