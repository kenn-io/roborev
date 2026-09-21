package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestScheduledPathMatchTreatsFiltersAsDescendants(t *testing.T) {
	assert.True(t, scheduledPathMatch("internal/daemon/server.go", []string{"internal/"}))
	assert.True(t, scheduledPathMatch("main.go", nil))
	assert.False(t, scheduledPathMatch("cmd/main.go", []string{"internal/"}))
	assert.False(t, scheduledPathMatch("internal.go", []string{"internal"}))
}
