package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/kit/tui/splitlayout"
)

func TestPickLayout(t *testing.T) {
	tests := []struct {
		name string
		w, h int
		want splitlayout.Mode
	}{
		{"exactly at breakpoint", 140, 36, splitlayout.Split},
		{"one column short", 139, 36, splitlayout.Stacked},
		{"one row short", 140, 35, splitlayout.Stacked},
		{"intermediate terminal", 180, 40, splitlayout.Split},
		{"very wide", 300, 80, splitlayout.Split},
		{"default init size", 80, 24, splitlayout.Stacked},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitlayout.PickLayout(tt.w, tt.h)
			t.Logf("%dx%d -> %v", tt.w, tt.h, got)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestSplitGeometry(t *testing.T) {
	assert := assert.New(t)

	// At 140 wide: list = clamp(140-100, 50, 90) = 50, detail = 90.
	g := splitLayoutConfig.Geometry(140, 36, 2)
	assert.Equal(50, g.ListOuterW)
	assert.Equal(90, g.DetailOuterW)
	assert.Equal(140, g.ListOuterW+g.DetailOuterW)
	// bodyH = height - title(1) - info(1) - footerLines
	assert.Equal(32, g.BodyH)
	assert.Equal(48, g.ListInnerW)
	assert.Equal(30, g.ListInnerH)
	assert.Equal(88, g.DetailInnerW)
	assert.Equal(30, g.DetailInnerH)

	// Very wide: list caps at 90, detail absorbs the rest.
	g = splitLayoutConfig.Geometry(300, 50, 1)
	assert.Equal(90, g.ListOuterW)
	assert.Equal(210, g.DetailOuterW)
	assert.Equal(47, g.BodyH)
	assert.Equal(88, g.ListInnerW)
	assert.Equal(45, g.ListInnerH)
	assert.Equal(208, g.DetailInnerW)
	assert.Equal(45, g.DetailInnerH)

	// The intermediate width allocates the list remainder without reaching
	// its maximum.
	g = splitLayoutConfig.Geometry(180, 40, 3)
	assert.Equal(80, g.ListOuterW)
	assert.Equal(100, g.DetailOuterW)
	assert.Equal(35, g.BodyH)
	assert.Equal(78, g.ListInnerW)
	assert.Equal(33, g.ListInnerH)
	assert.Equal(98, g.DetailInnerW)
	assert.Equal(33, g.DetailInnerH)

	// Wide-but-tight: list floor 50 holds even if detail dips below 100.
	// (Cannot happen above the 140 breakpoint, but geometry must not panic.)
	g = splitLayoutConfig.Geometry(120, 40, 1)
	assert.Equal(50, g.ListOuterW)
	assert.Equal(70, g.DetailOuterW)
	assert.Equal(37, g.BodyH)
	assert.Equal(48, g.ListInnerW)
	assert.Equal(35, g.ListInnerH)
	assert.Equal(68, g.DetailInnerW)
	assert.Equal(35, g.DetailInnerH)
}

func TestSplitGeometryWithDetailFocus(t *testing.T) {
	m := splitModel(withReview(splitTestReview()), withDimensions(180, 40))
	m.focus = focusDetail
	footerLines := len(reflowHelpRows(m.splitFooterRows(), m.width))
	g := splitLayoutConfig.Geometry(m.width, m.height, footerLines)

	assert.Equal(t, 2, footerLines)
	assert.Equal(t, 80, g.ListOuterW)
	assert.Equal(t, 100, g.DetailOuterW)
	assert.Equal(t, 36, g.BodyH)
	assert.Equal(t, 78, g.ListInnerW)
	assert.Equal(t, 34, g.ListInnerH)
	assert.Equal(t, 98, g.DetailInnerW)
	assert.Equal(t, 34, g.DetailInnerH)
}

func TestResolveLayoutLocking(t *testing.T) {
	assert := assert.New(t)
	m := initTestModel(withDimensions(150, 40))

	// Unlocked: follows the breakpoint.
	assert.Equal(splitlayout.Split, m.resolveLayout())
	m.width, m.height = 100, 30
	assert.Equal(splitlayout.Stacked, m.resolveLayout())

	// Locked to stacked: never splits.
	m.width, m.height = 200, 50
	m.layoutLocked = true
	m.preferredLayout = splitlayout.Stacked
	assert.Equal(splitlayout.Stacked, m.resolveLayout())

	// Locked to split: engages only when it fits, degrades when not,
	// re-engages when the terminal grows again.
	m.preferredLayout = splitlayout.Split
	assert.Equal(splitlayout.Split, m.resolveLayout())
	m.width = 100
	assert.Equal(splitlayout.Stacked, m.resolveLayout())
	m.width = 200
	assert.Equal(splitlayout.Split, m.resolveLayout())
}
