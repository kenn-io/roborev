//go:build integration

package main

import (
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
)

func TestLocalReviewReasoningLevels(t *testing.T) {
	tests := []struct {
		name      string
		reasoning string
		expected  string
	}{
		{name: "Fast", reasoning: string(agent.ReasoningFast), expected: "reasoning: " + string(agent.ReasoningFast)},
		{name: "Standard", reasoning: string(agent.ReasoningStandard), expected: "reasoning: " + string(agent.ReasoningStandard)},
		{name: "Medium", reasoning: string(agent.ReasoningMedium), expected: "reasoning: " + string(agent.ReasoningMedium)},
		{name: "Thorough", reasoning: string(agent.ReasoningThorough), expected: "reasoning: " + string(agent.ReasoningThorough)},
		{name: "Maximum", reasoning: string(agent.ReasoningMaximum), expected: "reasoning: " + string(agent.ReasoningMaximum)},
		{name: "Default", reasoning: "", expected: "reasoning: " + string(agent.ReasoningThorough)}, // default (agent defaults)
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newReviewHarness(t)
			err := h.run(runOpts{Agent: "test", Reasoning: tc.reasoning})
			require.NoError(t, err, "Expected no error, got: %v")

			h.assertOutputContains(tc.expected)
		})
	}
}
