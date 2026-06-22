package termstyle

import (
	"image/color"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/assert"
)

func resetAdaptiveColorState(t *testing.T) {
	t.Helper()

	t.Setenv("ROBOREV_COLOR_MODE", "")
	detectedDarkBackground.Store(false)
	detectedDarkBackgroundSet.Store(false)
}

func colorTuple(c color.Color) [4]uint32 {
	r, g, b, a := c.RGBA()
	return [4]uint32{r, g, b, a}
}

func TestAdaptiveColorDefaultsToDarkPalette(t *testing.T) {
	resetAdaptiveColorState(t)

	got := AdaptiveColor("1", "2")

	assert.Equal(t, colorTuple(lipgloss.Color("2")), colorTuple(got))
}

func TestAdaptiveColorRespectsExplicitLightMode(t *testing.T) {
	resetAdaptiveColorState(t)
	t.Setenv("ROBOREV_COLOR_MODE", "light")

	got := AdaptiveColor("1", "2")

	assert.Equal(t, colorTuple(lipgloss.Color("1")), colorTuple(got))
}

func TestAdaptiveColorUsesDetectedAutoBackground(t *testing.T) {
	resetAdaptiveColorState(t)
	SetDarkBackground(false)

	got := AdaptiveColor("1", "2")

	assert.Equal(t, colorTuple(lipgloss.Color("1")), colorTuple(got))
}
