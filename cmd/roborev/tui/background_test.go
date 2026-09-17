package tui

import (
	"image/color"
	"io"
	"testing"
	"testing/synctest"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/glamour/v2/styles"
	"github.com/muesli/termenv"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/streamfmt"
	"go.kenn.io/roborev/internal/termstyle"
)

func TestBackgroundColorUpdatesStyles(t *testing.T) {
	for _, tt := range []struct {
		name    string
		mode    string
		noColor string
		color   color.Color
		dark    bool
	}{
		{"auto light", "auto", "", color.White, false},
		{"auto dark", "auto", "", color.Black, true},
		{"default light", "", "", color.White, false},
		{"forced dark", "dark", "", color.White, true},
		{"forced light", "light", "", color.Black, false},
		{"disabled", "none", "", color.White, true},
		{"NO_COLOR", "auto", "1", color.White, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ROBOREV_COLOR_MODE", "dark")
			t.Setenv("NO_COLOR", tt.noColor)
			if tt.mode == "light" {
				t.Setenv("ROBOREV_COLOR_MODE", "light")
			}
			m := newModel(testEndpoint, withExternalIODisabled())
			t.Setenv("ROBOREV_COLOR_MODE", tt.mode)
			// Populate both caches before a late terminal response arrives.
			m.mdCache.colorProfile = termenv.TrueColor
			text := "# Heading\n\nReview text."
			before := m.mdCache.getReviewLines(text, 80, 80, 1)
			m.mdCache.getPromptLines(text, 80, 80, 1)

			updated, _ := m.Update(tea.BackgroundColorMsg{Color: tt.color})
			m = updated.(model)
			want := styles.DarkStyleConfig.Document.Color
			if !tt.dark {
				want = styles.LightStyleConfig.Document.Color
			}
			assert := assert.New(t)
			assert.Equal(want, m.glamourStyle.Document.Color)
			assert.Equal(want, m.mdCache.glamourStyle.Document.Color)
			assert.Equal(tt.dark, termstyle.DarkBackground())
			after := m.mdCache.getReviewLines(text, 80, 80, 1)
			assert.Equal(after, m.mdCache.getPromptLines(text, 80, 80, 1))
			if !tt.dark && tt.mode != "light" {
				assert.NotEqual(before, after, "cached Markdown must use the new palette")
			} else {
				assert.Equal(before, after)
			}
			require.NotEmpty(t, after)
		})
	}
}

func TestBackgroundColorRefreshesOpenLog(t *testing.T) {
	for _, view := range []string{"log", "help"} {
		t.Run(view, func(t *testing.T) {
			t.Setenv("ROBOREV_COLOR_MODE", "dark")
			t.Setenv("NO_COLOR", "")
			m := newModel(testEndpoint, withExternalIODisabled())
			m.currentView = viewLog
			m.logJobID = 1
			m.logOffset = 20
			m.logLines = []logLine{{text: "old palette"}}
			m.logFmtr = streamfmt.NewWithWidth(io.Discard, m.width, m.glamourStyle, nil)
			oldFormatter := m.logFmtr
			t.Setenv("ROBOREV_COLOR_MODE", "auto")
			if view == "help" {
				updated, _ := m.Update(tea.KeyPressMsg{Code: '?', Text: "?"})
				m = updated.(model)
			}

			updated, cmd := m.Update(tea.BackgroundColorMsg{Color: color.White})
			m = updated.(model)
			if view == "help" {
				assert.Nil(t, cmd, "defer the fetch until the log is visible")
				updated, cmd = m.Update(tea.KeyPressMsg{Code: '?', Text: "?"})
				m = updated.(model)
			}
			assert := assert.New(t)
			assert.Empty(m.logLines)
			assert.Zero(m.logOffset)
			assert.Equal(uint64(1), m.logFetchSeq)
			assert.NotSame(oldFormatter, m.logFmtr)
			require.NotNil(t, cmd, "refetch the log using the new palette")
		})
	}
}

func TestBackgroundColorInput(t *testing.T) {
	for _, tt := range []struct {
		name  string
		reply string
	}{
		{"BEL", "\x1b]11;rgb:ffff/ffff/ffff\a"},
		{"ST", "\x1b]11;rgb:ffff/ffff/ffff\x1b\\"},
		{"no reply", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ROBOREV_COLOR_MODE", "dark")
			t.Setenv("NO_COLOR", "")
			m := newModel(testEndpoint, withExternalIODisabled())
			t.Setenv("ROBOREV_COLOR_MODE", "auto")
			synctest.Test(t, func(t *testing.T) {
				r, w := io.Pipe()
				defer r.Close()
				var keys []string
				p := tea.NewProgram(m, tea.WithInput(r), tea.WithOutput(io.Discard),
					tea.WithoutRenderer(), tea.WithoutSignalHandler(),
					tea.WithFilter(func(_ tea.Model, msg tea.Msg) tea.Msg {
						// Exercise terminal input without running daemon requests or ticks.
						if _, ok := msg.(tea.BatchMsg); ok {
							return nil
						}
						if key, ok := msg.(tea.KeyPressMsg); ok {
							keys = append(keys, key.String())
						}
						return msg
					}))
				go func() {
					defer w.Close()
					_, _ = io.WriteString(w, "?")
					// A reply may arrive after the old startup query's timeout.
					time.Sleep(time.Second)
					_, _ = io.WriteString(w, tt.reply+"?q")
				}()
				result, err := p.Run()
				require.NoError(t, err)
				assert.Equal(t, []string{"?", "?", "q"}, keys)
				assert.Equal(t, viewQueue, result.(model).currentView)
				want := styles.LightStyleConfig.Document.Color
				if tt.reply == "" {
					want = styles.DarkStyleConfig.Document.Color
				}
				assert.Equal(t, want, result.(model).glamourStyle.Document.Color)
			})
		})
	}
}
