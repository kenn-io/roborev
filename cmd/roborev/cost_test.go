package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
)

func TestFormatCostLine(t *testing.T) {
	assert := assert.New(t)

	assert.Equal(
		"Approx cost: ~$1.75  (3/4 jobs reported cost)",
		formatCostLine(storage.CostAggregate{TotalUSD: 1.75, JobsWithCost: 3, JobsTotal: 4}),
	)
	assert.Equal(
		"Approx cost: ~$0.25",
		formatCostLine(storage.CostAggregate{TotalUSD: 0.25, JobsWithCost: 1, JobsTotal: 1, Complete: true}),
	)
	assert.Equal(
		"No eligible agent-spend jobs in scope yet.",
		formatCostLine(storage.CostAggregate{}),
	)
}

func TestCostCmdRejectsPositionalArgs(t *testing.T) {
	cmd := costCmd()

	require.NoError(t, cmd.Args(cmd, []string{}), "no args is valid")
	require.Error(t, cmd.Args(cmd, []string{"unexpected"}), "positional args rejected")
}
