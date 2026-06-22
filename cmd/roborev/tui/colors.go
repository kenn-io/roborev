package tui

import (
	"image/color"

	"go.kenn.io/roborev/internal/termstyle"
)

func adaptiveColor(light, dark string) color.Color {
	return termstyle.AdaptiveColor(light, dark)
}
