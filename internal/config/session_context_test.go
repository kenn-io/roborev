package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSessionContextConfig(t *testing.T) {
	assert.Equal(t, "off", ResolveSessionContextFrom(nil, nil).Mode)
	g := DefaultConfig()
	g.SessionContext.Mode = "on"
	assert.Equal(t, "on", ResolveSessionContextFrom(nil, g).Mode)
	r := &RepoConfig{SessionContext: SessionContextConfig{Mode: "off"}}
	assert.Equal(t, "off", ResolveSessionContextFrom(r, g).Mode)
	g.SessionContext.Mode = "invalid"
	require.ErrorContains(t, g.Validate(), "session_context.mode")
	r.SessionContext.Mode = "invalid"
	require.ErrorContains(t, r.Validate(), "session_context.mode")
}
