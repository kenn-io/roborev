package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPickLayout(t *testing.T) {
	tests := []struct {
		name string
		w, h int
		want layoutMode
	}{
		{"exactly at breakpoint", 140, 36, layoutSplit},
		{"one column short", 139, 36, layoutStacked},
		{"one row short", 140, 35, layoutStacked},
		{"very wide", 300, 80, layoutSplit},
		{"default init size", 80, 24, layoutStacked},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, pickLayout(tt.w, tt.h))
		})
	}
}

func TestSplitGeometry(t *testing.T) {
	assert := assert.New(t)

	// At 140 wide: list = clamp(140-100, 50, 90) = 50, detail = 90.
	g := splitGeometry(140, 36, 2)
	assert.Equal(50, g.listOuterW)
	assert.Equal(90, g.detailOuterW)
	assert.Equal(140, g.listOuterW+g.detailOuterW)
	// bodyH = height - title(1) - info(1) - footerLines
	assert.Equal(32, g.bodyH)
	assert.Equal(48, g.listInnerW)
	assert.Equal(30, g.listInnerH)
	assert.Equal(88, g.detailInnerW)
	assert.Equal(30, g.detailInnerH)

	// Very wide: list caps at 90, detail absorbs the rest.
	g = splitGeometry(300, 50, 1)
	assert.Equal(90, g.listOuterW)
	assert.Equal(210, g.detailOuterW)

	// Wide-but-tight: list floor 50 holds even if detail dips below 100.
	// (Cannot happen above the 140 breakpoint, but geometry must not panic.)
	g = splitGeometry(120, 40, 1)
	assert.Equal(50, g.listOuterW)
	assert.Equal(70, g.detailOuterW)
}

func TestResolveLayoutLocking(t *testing.T) {
	assert := assert.New(t)
	m := initTestModel(withDimensions(150, 40))

	// Unlocked: follows the breakpoint.
	assert.Equal(layoutSplit, m.resolveLayout())
	m.width, m.height = 100, 30
	assert.Equal(layoutStacked, m.resolveLayout())

	// Locked to stacked: never splits.
	m.width, m.height = 200, 50
	m.layoutLocked = true
	m.preferredLayout = layoutStacked
	assert.Equal(layoutStacked, m.resolveLayout())

	// Locked to split: engages only when it fits, degrades when not,
	// re-engages when the terminal grows again.
	m.preferredLayout = layoutSplit
	assert.Equal(layoutSplit, m.resolveLayout())
	m.width = 100
	assert.Equal(layoutStacked, m.resolveLayout())
	m.width = 200
	assert.Equal(layoutSplit, m.resolveLayout())
}
