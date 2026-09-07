package agent

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFormatStreamingCLIWaitErrorIncludesParseError(t *testing.T) {
	t.Parallel()

	err := formatStreamingCLIWaitError("codex", streamingCLIResult{
		WaitErr:  errors.New("exit status 1"),
		ParseErr: errors.New("bad json"),
	}, "stderr text")

	require.Error(t, err)
	assert.EqualError(t, err, "codex failed: exit status 1 (parse error: bad json)\nstderr: stderr text")
}

func TestFormatDetailedCLIWaitErrorFallsBackToOutputAndPreservesPartial(t *testing.T) {
	t.Parallel()

	err := formatDetailedCLIWaitError(streamingCLIResult{
		WaitErr:  errors.New("exit status 1"),
		ParseErr: errors.New("bad stream"),
	}, detailedCLIWaitErrorOptions{
		AgentName:      "kilo",
		FallbackOutput: "raw output",
		FallbackLabel:  "output",
		PartialOutput:  strings.Repeat("x", 501),
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "kilo failed")
	assert.Contains(t, err.Error(), "\nstream: bad stream")
	assert.Contains(t, err.Error(), "\noutput: raw output")
	assert.Contains(t, err.Error(), "\npartial output: ")
	assert.Contains(t, err.Error(), ": exit status 1")
	assert.Contains(t, err.Error(), strings.Repeat("x", 501))
}
