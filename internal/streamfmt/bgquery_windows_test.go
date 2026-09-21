package streamfmt

import (
	"testing"

	"charm.land/glamour/v2/styles"
	"github.com/stretchr/testify/assert"
)

func TestWindowsForegroundAndInitialStyles(t *testing.T) {
	t.Setenv("ROBOREV_COLOR_MODE", "auto")
	t.Setenv("NO_COLOR", "")
	original := platformHasDarkBackground
	t.Cleanup(func() { platformHasDarkBackground = original })
	queries := 0
	platformHasDarkBackground = func() bool {
		queries++
		return false
	}

	assert := assert.New(t)
	assert.Equal(styles.DarkStyleConfig.Document.Color, InitialGlamourStyle().Document.Color)
	assert.Zero(queries, "TUI initialization must not enter the foreground query")
	assert.Equal(styles.LightStyleConfig.Document.Color, GlamourStyle().Document.Color)
	assert.Equal(1, queries)
	t.Setenv("ROBOREV_COLOR_MODE", "dark")
	assert.Equal(styles.DarkStyleConfig.Document.Color, GlamourStyle().Document.Color)
	assert.Equal(1, queries, "explicit modes must skip detection")
}
