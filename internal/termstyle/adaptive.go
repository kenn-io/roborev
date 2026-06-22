package termstyle

import (
	"image/color"
	"os"
	"strings"
	"sync/atomic"

	"charm.land/lipgloss/v2"
)

var (
	detectedDarkBackground    atomic.Bool
	detectedDarkBackgroundSet atomic.Bool
)

type adaptiveColor struct {
	light color.Color
	dark  color.Color
}

// AdaptiveColor returns a light/dark color without performing terminal I/O.
func AdaptiveColor(light, dark string) color.Color {
	return adaptiveColor{
		light: lipgloss.Color(light),
		dark:  lipgloss.Color(dark),
	}
}

// SetDarkBackground records terminal background detection performed by callers
// that already have a safe point to query the terminal.
func SetDarkBackground(isDark bool) {
	detectedDarkBackground.Store(isDark)
	detectedDarkBackgroundSet.Store(true)
}

// DarkBackground returns the palette choice for adaptive colors.
func DarkBackground() bool {
	switch strings.ToLower(os.Getenv("ROBOREV_COLOR_MODE")) {
	case "light":
		return false
	case "dark", "none":
		return true
	}
	if detectedDarkBackgroundSet.Load() {
		return detectedDarkBackground.Load()
	}
	return true
}

func (c adaptiveColor) RGBA() (uint32, uint32, uint32, uint32) {
	if DarkBackground() {
		return c.dark.RGBA()
	}
	return c.light.RGBA()
}
