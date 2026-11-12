package mcpserver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestToolAnnotations(t *testing.T) {
	session := connectSession(t, &fakeBackend{})
	result, err := session.ListTools(t.Context(), nil)
	require.NoError(t, err)

	// Search and Kata context may contact configured external services.
	writes := map[string]struct {
		destructive bool
		idempotent  bool
	}{
		"roborev_add_comment":  {false, false},
		"roborev_close_review": {true, false},
		"roborev_snooze":       {true, false},
		"roborev_complete_fix": {true, true},
	}
	require.Len(t, result.Tools, 13)
	for _, tool := range result.Tools {
		t.Run(tool.Name, func(t *testing.T) {
			assert := assert.New(t)
			annotations := tool.Annotations
			require.NotNil(t, annotations)
			require.NotNil(t, annotations.DestructiveHint)
			require.NotNil(t, annotations.OpenWorldHint)
			write, mutates := writes[tool.Name]
			assert.Equal(!mutates, annotations.ReadOnlyHint)
			assert.Equal(write.destructive, *annotations.DestructiveHint)
			assert.Equal(!mutates || write.idempotent, annotations.IdempotentHint)
			assert.Equal(tool.Name == "roborev_search_reviews" || tool.Name == "roborev_get_kata_context", *annotations.OpenWorldHint)
		})
	}
}
