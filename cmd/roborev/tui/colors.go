package tui

import (
	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/compat"
)

func adaptiveColor(light, dark string) compat.AdaptiveColor {
	return compat.AdaptiveColor{
		Light: lipgloss.Color(light),
		Dark:  lipgloss.Color(dark),
	}
}
