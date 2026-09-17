package tui

import (
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/glamour/v2/styles"
	"github.com/mattn/go-runewidth"
	"github.com/muesli/termenv"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/tui/helplayout"
	"go.kenn.io/kit/tui/helprender"

	"go.kenn.io/roborev/internal/storage"
)

var testANSIRegex = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func stripTestANSI(s string) string {
	return testANSIRegex.ReplaceAllString(s, "")
}

func TestRenderMarkdownLinesPreservesNewlines(t *testing.T) {
	// Verify that single newlines in plain text are preserved (not collapsed into one paragraph)
	lines := renderMarkdownLines("Line 1\nLine 2\nLine 3", 80, 80, styles.DarkStyleConfig, 2, termenv.TrueColor)

	found := 0
	for _, line := range lines {
		trimmed := strings.TrimSpace(stripTestANSI(line))
		if trimmed == "Line 1" || trimmed == "Line 2" || trimmed == "Line 3" {
			found++
		}
	}
	assert.Equal(t, 3, found)
}

func TestRenderMarkdownLinesFallsBackOnEmpty(t *testing.T) {
	lines := renderMarkdownLines("", 80, 80, styles.DarkStyleConfig, 2, termenv.TrueColor)
	// Should not panic and should produce some output (even if empty)
	assert.NotNil(t, lines)
}

func TestMarkdownCacheBehavior(t *testing.T) {
	baseText := "Hello\nWorld"
	baseWidth := 80
	baseID := int64(1)

	tests := []struct {
		name          string
		text          string
		width         int
		id            int
		expectHit     bool
		expectedMatch string
	}{
		{"SameInputs", baseText, baseWidth, int(baseID), true, ""},
		{"DiffText", "Different", baseWidth, int(baseID), false, "Different"},
		{"DiffWidth", baseText, 40, int(baseID), false, ""},
		{"DiffID", baseText, baseWidth, 2, false, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Always start with a fresh cache to ensure isolation
			c := &markdownCache{}
			// Prime cache with base state
			lines1 := c.getReviewLines(baseText, baseWidth, baseWidth, baseID)

			// Exercise the cache with the test case inputs
			lines2 := c.getReviewLines(tt.text, tt.width, tt.width, int64(tt.id))

			// Check if the underlying array is the same (cache hit vs miss)
			assert.NotEmpty(t, lines1, "Unexpected empty lines from render")
			assert.NotEmpty(t, lines2, "Unexpected empty lines from render")
			isSameObject := &lines1[0] == &lines2[0]

			if tt.expectHit {
				assert.True(t, isSameObject)
			} else {
				assert.False(t, isSameObject)
			}

			if tt.expectedMatch != "" {
				var combined strings.Builder
				for _, line := range lines2 {
					combined.WriteString(stripTestANSI(line))
				}
				assert.Contains(t, combined.String(), tt.expectedMatch)
			}
		})
	}
}

func TestMarkdownCachePromptSeparateFromReview(t *testing.T) {
	c := &markdownCache{}

	// Review and prompt caches are independent
	reviewLines := c.getReviewLines("Review text", 80, 80, 1)
	promptLines := c.getPromptLines("Prompt text", 80, 80, 1)

	reviewContent := strings.TrimSpace(reviewLines[len(reviewLines)-1])
	promptContent := strings.TrimSpace(promptLines[len(promptLines)-1])

	assert.NotEqual(t, reviewContent, promptContent)
}

func TestRenderViewSafety_NilCache(t *testing.T) {
	tests := []struct {
		name   string
		view   viewKind
		setup  func(*storage.Review)
		render func(model) string
		want   string
	}{
		{
			name:   "ReviewView",
			view:   viewReview,
			setup:  func(r *storage.Review) { r.Output = "output text" },
			render: func(m model) string { return m.renderReviewView() },
			want:   "output text",
		},
		{
			name:   "PromptView",
			view:   viewKindPrompt,
			setup:  func(r *storage.Review) { r.Prompt = "prompt text" },
			render: func(m model) string { return m.renderPromptView() },
			want:   "prompt text",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := model{
				width:       80,
				height:      24,
				currentView: tt.view,
				currentReview: &storage.Review{
					ID:  1,
					Job: &storage.ReviewJob{GitRef: "abc1234"},
				},
			}
			tt.setup(m.currentReview)

			// Should not panic despite nil mdCache
			got := tt.render(m)
			assert.Contains(t, got, tt.want)
		})
	}
}

func TestScrollPageUpAfterPageDown(t *testing.T) {
	tests := []struct {
		name string
		view viewKind
	}{
		{"PromptView", viewKindPrompt},
		{"ReviewView", viewReview},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var lines []string
			for i := range 100 {
				lines = append(lines, fmt.Sprintf("Line %d of content", i+1))
			}
			longContent := strings.Join(lines, "\n")

			m := model{
				width:       80,
				height:      24,
				currentView: tt.view,
				mdCache:     newMarkdownCache(2),
				currentReview: &storage.Review{
					ID:  1,
					Job: &storage.ReviewJob{GitRef: "abc"},
				},
			}

			var maxScroll int
			if tt.view == viewKindPrompt {
				m.currentReview.Prompt = longContent
				m.renderPromptView()
				maxScroll = m.mdCache.lastPromptMaxScroll
			} else {
				m.currentReview.Output = longContent
				m.renderReviewView()
				maxScroll = m.mdCache.lastReviewMaxScroll
			}

			assert.NotZero(t, maxScroll, "Expected non-zero max scroll")

			// Page down past end
			for range 20 {
				m, _ = pressSpecial(m, tea.KeyPgDown)
			}

			getScroll := func(m model) int {
				if tt.view == viewKindPrompt {
					return m.promptScroll
				}
				return m.reviewScroll
			}

			s := getScroll(m)
			assert.LessOrEqual(t, s, maxScroll)

			// Page up
			before := getScroll(m)
			m, _ = pressSpecial(m, tea.KeyPgUp)
			assert.Less(t, getScroll(m), before)
		})
	}
}

func TestTruncateLongLinesOnlyTruncatesCodeBlocks(t *testing.T) {
	longLine := "a very long line that exceeds the width by a lot and should be truncated down to size"
	input := "short\n```\n" + longLine + "\n```\n" + longLine
	out := truncateLongLines(input, 20, 2)
	lines := strings.Split(out, "\n")

	assert.Len(t, lines, 5)
	assert.Equal(t, "short", lines[0])
	assert.LessOrEqual(t, len(lines[2]), 20)
	// Prose line outside code block should be preserved intact
	assert.Equal(t, lines[4], longLine)
}

func TestTruncateLongLinesFenceEdgeCases(t *testing.T) {
	longLine := strings.Repeat("x", 50)
	tests := []struct {
		name      string
		input     string
		wantTrunc bool // whether longLine inside the fence should be truncated
	}{
		{
			name:      "tilde fence",
			input:     "~~~\n" + longLine + "\n~~~",
			wantTrunc: true,
		},
		{
			name:      "indented fence (2 spaces)",
			input:     "  ```\n" + longLine + "\n  ```",
			wantTrunc: true,
		},
		{
			name:      "4-backtick fence",
			input:     "````\n" + longLine + "\n````",
			wantTrunc: true,
		},
		{
			name:      "4-backtick fence not closed by 3",
			input:     "````\n" + longLine + "\n```",
			wantTrunc: true, // still inside — 3 backticks can't close a 4-backtick fence
		},
		{
			name:      "backtick fence with info string",
			input:     "```diff\n" + longLine + "\n```",
			wantTrunc: true,
		},
		{
			name:      "prose with triple backtick in text not a fence",
			input:     longLine, // no fence at all
			wantTrunc: false,
		},
		{
			name:      "closing backtick fence with info string does not close",
			input:     "```\n```lang\n" + longLine,
			wantTrunc: true, // long line after invalid closer is still inside fence
		},
		{
			name:      "closing tilde fence with text does not close",
			input:     "~~~\n~~~text\n" + longLine,
			wantTrunc: true, // long line after invalid closer is still inside fence
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := truncateLongLines(tt.input, 20, 2)
			lines := strings.SplitSeq(out, "\n")
			// Find the longLine (or its truncation) in the output
			found := false
			for line := range lines {
				if strings.Contains(line, "xxxxxxxxxx") { // 10 x's is enough to identify the line
					truncated := len(line) <= 20
					require.Equal(t, tt.wantTrunc, truncated)
					found = true
					break
				}
			}
			require.True(t, found, "Could not find long line in output")
		})
	}
}

func TestTruncateLongLinesPreservesNewlines(t *testing.T) {
	// Ensure blank lines and structure are preserved
	input := "line1\n\n\nline4"
	out := truncateLongLines(input, 80, 2)
	assert.Equal(t, out, input)
}

func TestRenderMarkdownLinesPreservesLongProse(t *testing.T) {
	// Long prose lines should be word-wrapped by glamour, not truncated.
	// All words must appear in the rendered output.
	longProse := "This is a very long prose line with important content that should be word-wrapped by glamour rather than truncated so that no information is lost from the rendered output"
	lines := renderMarkdownLines(longProse, 60, 80, styles.DarkStyleConfig, 2, termenv.TrueColor)

	var combined strings.Builder
	for _, line := range lines {
		combined.WriteString(stripTestANSI(line))
		combined.WriteByte(' ')
	}
	for _, word := range []string{"important", "word-wrapped", "truncated", "information", "rendered"} {
		assert.Contains(t, combined.String(), word)
	}
}

func TestSanitizeEscapes(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "SGR preserved",
			input: "\x1b[31mred\x1b[0m",
			want:  "\x1b[31mred\x1b[0m",
		},
		{
			name:  "OSC stripped",
			input: "hello\x1b]0;evil title\x07world",
			want:  "helloworld",
		},
		{
			name:  "DCS stripped",
			input: "hello\x1bPevil\x1b\\world",
			want:  "helloworld",
		},
		{
			name:  "CSI non-SGR stripped",
			input: "hello\x1b[2Jworld", // ED (erase display)
			want:  "helloworld",
		},
		{
			name:  "bare ESC stripped",
			input: "hello\x1bcworld", // RIS (reset)
			want:  "helloworld",
		},
		{
			name:  "mixed: SGR kept, OSC stripped",
			input: "\x1b[1mbold\x1b]0;evil\x07\x1b[0m",
			want:  "\x1b[1mbold\x1b[0m",
		},
		{
			name:  "private-mode CSI stripped (hide cursor)",
			input: "hello\x1b[?25lworld",
			want:  "helloworld",
		},
		{
			name:  "private-mode CSI stripped (DA2)",
			input: "hello\x1b[>0cworld",
			want:  "helloworld",
		},
		{
			name:  "CSI with intermediate byte stripped (cursor style)",
			input: "hello\x1b[1 qworld",
			want:  "helloworld",
		},
		{
			// Incomplete CSI without a final byte is preserved: each line
			// is sanitized independently so there is no cross-line completion,
			// and stripping partial CSI would also catch legitimate trailing SGR.
			name:  "unterminated CSI at end of string preserved",
			input: "hello\x1b[31",
			want:  "hello\x1b[31",
		},
		{
			name:  "unterminated OSC stripped (no terminator)",
			input: "hello\x1b]0;title",
			want:  "hello",
		},
		{
			name:  "bare CSI introducer at end of string preserved",
			input: "hello\x1b[",
			want:  "hello\x1b[",
		},
		{
			name:  "carriage return stripped (prevents line overwrite)",
			input: "fake\rreal",
			want:  "fakereal",
		},
		{
			name:  "backspace stripped (prevents overwrite spoofing)",
			input: "hello\b\b\b\b\bworld",
			want:  "helloworld",
		},
		{
			name:  "BEL stripped",
			input: "hello\aworld",
			want:  "helloworld",
		},
		{
			name:  "tab and newline preserved",
			input: "col1\tcol2\nrow2",
			want:  "col1\tcol2\nrow2",
		},
		{
			name:  "null byte stripped",
			input: "hello\x00world",
			want:  "helloworld",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeEscapes(tt.input)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestRenderMarkdownLinesNoOverflow(t *testing.T) {
	// A long diff line should be truncated by renderMarkdownLines, not wrapped
	longLine := strings.Repeat("x", 200)
	text := "Review:\n\n```\n" + longLine + "\n```\n"
	width := 76
	lines := renderMarkdownLines(text, width, width, styles.DarkStyleConfig, 2, termenv.TrueColor)

	for i, line := range lines {
		stripped := stripTestANSI(line)
		if len(stripped) > width+10 { // small tolerance for trailing spaces
			assert.LessOrEqual(t, len(stripped), width+10, "line %d exceeds width %d: len=%d %q", i, width, len(stripped), stripped)
		}
	}
}

func TestRenderMarkdownLinesNoColor(t *testing.T) {
	// When colorProfile is Ascii, stripTrailingPadding removes all SGR
	// sequences (colors, bold, underline, reset) so no formatting can
	// bleed across lines.
	text := "# Heading\n\nSome **bold** text and `code`."
	lines := renderMarkdownLines(text, 80, 80, styles.DarkStyleConfig, 2, termenv.Ascii)

	combined := strings.Join(lines, "\n")
	allSGR := regexp.MustCompile(`\x1b\[[0-9;]*m`)
	matches := allSGR.FindAllString(combined, -1)
	assert.Empty(t, matches, "expected no SGR sequences with Ascii profile, got: %v", matches)
}

func TestHelpOutputParity(t *testing.T) {
	// Expectations came from the local renderer at 5c372165 before migration.
	tests := []struct {
		name        string
		rows        [][]helplayout.HelpItem
		width       int
		wantLight   string
		wantDark    string
		wantVisible string
	}{
		{
			name: "ragged_unicode_key_only",
			rows: [][]helplayout.HelpItem{
				{{Key: "a", Description: "one"}, {Key: "b", Description: "two"}, {Key: "esc", Description: ""}},
				{{Key: "界", Description: "字"}, {Key: "q", Description: ""}},
				{{Key: "z", Description: ""}},
			},
			width:       80,
			wantLight:   "\x1b[38;2;108;108;108ma\x1b[m \x1b[38;2;168;168;168mone\x1b[m\x1b[38;2;168;168;168m▕\x1b[m \x1b[38;2;108;108;108mb\x1b[m \x1b[38;2;168;168;168mtwo\x1b[m\x1b[38;2;168;168;168m▕\x1b[m \x1b[38;2;108;108;108mesc\x1b[m\n\x1b[38;2;108;108;108m界\x1b[m \x1b[38;2;168;168;168m字\x1b[m\x1b[38;2;168;168;168m▕\x1b[m \x1b[38;2;108;108;108mq\x1b[m         \n\x1b[38;2;108;108;108mz\x1b[m                ",
			wantDark:    "\x1b[38;2;148;148;148ma\x1b[m \x1b[38;2;88;88;88mone\x1b[m\x1b[38;2;108;108;108m▕\x1b[m \x1b[38;2;148;148;148mb\x1b[m \x1b[38;2;88;88;88mtwo\x1b[m\x1b[38;2;108;108;108m▕\x1b[m \x1b[38;2;148;148;148mesc\x1b[m\n\x1b[38;2;148;148;148m界\x1b[m \x1b[38;2;88;88;88m字\x1b[m\x1b[38;2;108;108;108m▕\x1b[m \x1b[38;2;148;148;148mq\x1b[m         \n\x1b[38;2;148;148;148mz\x1b[m                ",
			wantVisible: "a one▕ b two▕ esc\n界 字▕ q         \nz                ",
		},
		{
			name:        "exact_fit",
			rows:        [][]helplayout.HelpItem{{{Key: "a", Description: "one"}, {Key: "b", Description: "two"}}},
			width:       12,
			wantLight:   "\x1b[38;2;108;108;108ma\x1b[m \x1b[38;2;168;168;168mone\x1b[m\x1b[38;2;168;168;168m▕\x1b[m \x1b[38;2;108;108;108mb\x1b[m \x1b[38;2;168;168;168mtwo\x1b[m",
			wantDark:    "\x1b[38;2;148;148;148ma\x1b[m \x1b[38;2;88;88;88mone\x1b[m\x1b[38;2;108;108;108m▕\x1b[m \x1b[38;2;148;148;148mb\x1b[m \x1b[38;2;88;88;88mtwo\x1b[m",
			wantVisible: "a one▕ b two",
		},
		{
			name:        "below_fit",
			rows:        [][]helplayout.HelpItem{{{Key: "a", Description: "one"}, {Key: "b", Description: "two"}}},
			width:       11,
			wantLight:   "\x1b[38;2;108;108;108ma\x1b[m \x1b[38;2;168;168;168mone\x1b[m\n\x1b[38;2;108;108;108mb\x1b[m \x1b[38;2;168;168;168mtwo\x1b[m",
			wantDark:    "\x1b[38;2;148;148;148ma\x1b[m \x1b[38;2;88;88;88mone\x1b[m\n\x1b[38;2;148;148;148mb\x1b[m \x1b[38;2;88;88;88mtwo\x1b[m",
			wantVisible: "a one\nb two",
		},
		{
			name:        "overwide",
			rows:        [][]helplayout.HelpItem{{{Key: "long", Description: "description"}, {Key: "q", Description: ""}}},
			width:       5,
			wantLight:   "\x1b[38;2;108;108;108mlong\x1b[m \x1b[38;2;168;168;168mdescription\x1b[m\n\x1b[38;2;108;108;108mq\x1b[m               ",
			wantDark:    "\x1b[38;2;148;148;148mlong\x1b[m \x1b[38;2;88;88;88mdescription\x1b[m\n\x1b[38;2;148;148;148mq\x1b[m               ",
			wantVisible: "long description\nq               ",
		},
		{
			name:        "present_empty",
			rows:        [][]helplayout.HelpItem{nil, {{Key: "a", Description: "one"}, {Key: "", Description: ""}}, {}, {{Key: "b", Description: "two"}}},
			width:       80,
			wantLight:   "\x1b[38;2;108;108;108ma\x1b[m \x1b[38;2;168;168;168mone\x1b[m\x1b[38;2;168;168;168m▕\x1b[m \x1b[38;2;108;108;108m\x1b[m\n\x1b[38;2;108;108;108mb\x1b[m \x1b[38;2;168;168;168mtwo\x1b[m  ",
			wantDark:    "\x1b[38;2;148;148;148ma\x1b[m \x1b[38;2;88;88;88mone\x1b[m\x1b[38;2;108;108;108m▕\x1b[m \x1b[38;2;148;148;148m\x1b[m\n\x1b[38;2;148;148;148mb\x1b[m \x1b[38;2;88;88;88mtwo\x1b[m  ",
			wantVisible: "a one▕ \nb two  ",
		},
		{
			name:        "zero_width",
			rows:        [][]helplayout.HelpItem{nil, {{Key: "a", Description: "one"}, {Key: "b", Description: "two"}}, {}},
			width:       0,
			wantLight:   "            \n\x1b[38;2;108;108;108ma\x1b[m \x1b[38;2;168;168;168mone\x1b[m\x1b[38;2;168;168;168m▕\x1b[m \x1b[38;2;108;108;108mb\x1b[m \x1b[38;2;168;168;168mtwo\x1b[m\n            ",
			wantDark:    "            \n\x1b[38;2;148;148;148ma\x1b[m \x1b[38;2;88;88;88mone\x1b[m\x1b[38;2;108;108;108m▕\x1b[m \x1b[38;2;148;148;148mb\x1b[m \x1b[38;2;88;88;88mtwo\x1b[m\n            ",
			wantVisible: "            \na one▕ b two\n            ",
		},
		{
			name:        "negative_width",
			rows:        [][]helplayout.HelpItem{nil, {{Key: "a", Description: "one"}, {Key: "b", Description: "two"}}, {}},
			width:       -1,
			wantLight:   "            \n\x1b[38;2;108;108;108ma\x1b[m \x1b[38;2;168;168;168mone\x1b[m\x1b[38;2;168;168;168m▕\x1b[m \x1b[38;2;108;108;108mb\x1b[m \x1b[38;2;168;168;168mtwo\x1b[m\n            ",
			wantDark:    "            \n\x1b[38;2;148;148;148ma\x1b[m \x1b[38;2;88;88;88mone\x1b[m\x1b[38;2;108;108;108m▕\x1b[m \x1b[38;2;148;148;148mb\x1b[m \x1b[38;2;88;88;88mtwo\x1b[m\n            ",
			wantVisible: "            \na one▕ b two\n            ",
		},
		{
			name:        "all_empty_positive",
			rows:        [][]helplayout.HelpItem{nil, {}},
			width:       80,
			wantLight:   "",
			wantDark:    "",
			wantVisible: "",
		},
		{
			name:        "all_empty_zero",
			rows:        [][]helplayout.HelpItem{nil, {}},
			width:       0,
			wantLight:   "\n",
			wantDark:    "\n",
			wantVisible: "\n",
		},
		{
			name:        "all_empty_negative",
			rows:        [][]helplayout.HelpItem{nil, {}},
			width:       -1,
			wantLight:   "\n",
			wantDark:    "\n",
			wantVisible: "\n",
		},
	}
	for _, mode := range []string{"light", "dark"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("NO_COLOR", "")
			t.Setenv("ROBOREV_COLOR_MODE", mode)
			for _, tc := range tests {
				t.Run(tc.name, func(t *testing.T) {
					want := tc.wantLight
					if mode == "dark" {
						want = tc.wantDark
					}
					got := helprender.RenderHelpTable(convertAndReflowHelpRows(tc.rows, tc.width), helpTableStyles)
					assert.Equal(t, want, got)
					assert.Equal(t, tc.wantVisible, stripTestANSI(got))
				})
			}
		})
	}
}

func TestReflowHelpRows(t *testing.T) {
	a := helplayout.HelpItem{Key: "a", Description: "one"}
	b := helplayout.HelpItem{Key: "b", Description: "two"}
	c := helplayout.HelpItem{Key: "c", Description: "three"}
	d := helplayout.HelpItem{Key: "d", Description: "four"}
	wide := helplayout.HelpItem{Key: "界", Description: "字"}
	overwide := helplayout.HelpItem{Key: "very-long-item-label", Description: "description"}
	keyOnly := helplayout.HelpItem{Key: "q"}
	tests := []struct {
		name   string
		rows   [][]helplayout.HelpItem
		widths []int
		want   [][]helplayout.HelpItem
	}{
		{
			name:   "exact fit and unchanged grouping",
			rows:   [][]helplayout.HelpItem{{a, b}},
			widths: []int{-1, 0, 12, 80},
			want:   [][]helplayout.HelpItem{{a, b}},
		},
		{
			name:   "one below exact fit",
			rows:   [][]helplayout.HelpItem{{a, b}},
			widths: []int{11},
			want:   [][]helplayout.HelpItem{{a}, {b}},
		},
		{
			name:   "split into two rows",
			rows:   [][]helplayout.HelpItem{{a, b, c, d}},
			widths: []int{28},
			want:   [][]helplayout.HelpItem{{a, b, c}, {d}},
		},
		{
			name:   "wide Unicode exact fit",
			rows:   [][]helplayout.HelpItem{{wide, keyOnly}},
			widths: []int{8},
			want:   [][]helplayout.HelpItem{{wide, keyOnly}},
		},
		{
			name:   "wide Unicode below fit",
			rows:   [][]helplayout.HelpItem{{wide, keyOnly}},
			widths: []int{7},
			want:   [][]helplayout.HelpItem{{wide}, {keyOnly}},
		},
		{
			name:   "overwide item forces single column",
			rows:   [][]helplayout.HelpItem{{overwide, b}, {c, d}},
			widths: []int{-1, 0, 1, 20},
			want:   [][]helplayout.HelpItem{{overwide}, {b}, {c}, {d}},
		},
		{
			name:   "nil rows",
			widths: []int{-1, 0, 1, 80},
		},
		{
			name:   "empty rows",
			rows:   [][]helplayout.HelpItem{},
			widths: []int{-1, 0, 1, 80},
		},
		{
			name:   "all empty source rows",
			rows:   [][]helplayout.HelpItem{nil, {}, nil},
			widths: []int{-1, 0, 1, 80},
		},
		{
			name:   "mixed source rows preserve present empty item",
			rows:   [][]helplayout.HelpItem{nil, {a}, {}, {{Key: "", Description: ""}}, {b}, nil},
			widths: []int{-1, 0, 1, 80},
			want:   [][]helplayout.HelpItem{{a}, {{Key: "", Description: ""}}, {b}},
		},
		{
			name:   "present empty item alone",
			rows:   [][]helplayout.HelpItem{{{Key: "", Description: ""}}},
			widths: []int{-1, 0, 1, 80},
			want:   [][]helplayout.HelpItem{{{Key: "", Description: ""}}},
		},
	}
	for _, tc := range tests {
		for _, width := range tc.widths {
			t.Run(fmt.Sprintf("%s/width=%d", tc.name, width), func(t *testing.T) {
				before := slices.Clone(tc.rows)
				for i, row := range tc.rows {
					before[i] = slices.Clone(row)
				}
				want := tc.want
				if width <= 0 {
					want = tc.rows
				}
				got := convertAndReflowHelpRows(tc.rows, width)
				assert.Equal(t, want, got)
				assert.Equal(t, before, tc.rows, "must not mutate caller rows")
			})
		}
	}
}

func TestRenderHelpTableLinesWithinWidth(t *testing.T) {
	// Real help row sets used by the TUI views.
	helpSets := map[string][][]helplayout.HelpItem{
		"queue": {
			{{Key: "x", Description: "cancel"}, {Key: "r", Description: "rerun"}, {Key: "l", Description: "log"}, {Key: "p", Description: "prompt"}, {Key: "c", Description: "comment"}, {Key: "y", Description: "copy"}, {Key: "m", Description: "commit"}, {Key: "F", Description: "fix"}},
			{{Key: "↑/↓", Description: "nav"}, {Key: "enter", Description: "review"}, {Key: "a", Description: "closed"}, {Key: "f", Description: "filter"}, {Key: "h", Description: "hide"}, {Key: "s", Description: "show classify"}, {Key: "T", Description: "tasks"}, {Key: "?", Description: "help"}, {Key: "q", Description: "quit"}},
		},
		"review": {
			{{Key: "p", Description: "prompt"}, {Key: "c", Description: "comment"}, {Key: "m", Description: "commit"}, {Key: "a", Description: "closed"}, {Key: "y", Description: "copy"}, {Key: "F", Description: "fix"}},
			{{Key: "↑/↓", Description: "scroll"}, {Key: "←/→", Description: "prev/next"}, {Key: "?", Description: "commands"}, {Key: "esc", Description: "back"}},
		},
		"filter": {
			{{Key: "↑/↓", Description: "nav"}, {Key: "→/←", Description: "expand/collapse"}, {Key: "↵", Description: "select"}, {Key: "esc", Description: "cancel"}, {Key: "type to search", Description: ""}},
		},
		"tasks": {
			{{Key: "enter", Description: "view"}, {Key: "P", Description: "parent"}, {Key: "p", Description: "patch"}, {Key: "A", Description: "apply"}, {Key: "l", Description: "log"}, {Key: "x", Description: "cancel"}, {Key: "?", Description: "help"}, {Key: "T/esc", Description: "back"}},
		},
	}

	widths := []int{50, 80, 100, 120}

	for name, rows := range helpSets {
		for _, width := range widths {
			t.Run(fmt.Sprintf("%s/width=%d", name, width), func(t *testing.T) {
				rendered := helprender.RenderHelpTable(convertAndReflowHelpRows(rows, width), helpTableStyles)
				reflowed := convertAndReflowHelpRows(rows, width)

				// Rendered line count must match reflowed row count.
				lines := strings.Split(strings.TrimRight(rendered, "\n"), "\n")
				assert.Len(t, reflowed, len(lines))

				// No rendered line should exceed the target width.
				for _, line := range lines {
					visible := stripTestANSI(line)
					visW := runewidth.StringWidth(visible)
					assert.LessOrEqual(t, visW, width)
				}
			})
		}
	}
}

func TestQueueHelpRowsTasksWorkflowToggle(t *testing.T) {
	disabled := newModel(testEndpoint, withExternalIODisabled()).queueHelpRows()
	assert.NotEmpty(t, disabled, "expected queue help rows")
	for _, row := range disabled {
		for _, item := range row {
			assert.False(t, item.Key == "F" || item.Key == "T")
		}
	}

	m := newModel(testEndpoint, withExternalIODisabled())
	m.tasksEnabled = true
	enabled := m.queueHelpRows()
	foundF := false
	foundT := false
	for _, row := range enabled {
		for _, item := range row {
			if item.Key == "F" {
				foundF = true
			}
			if item.Key == "T" {
				foundT = true
			}
		}
	}
	assert.False(t, !foundF || !foundT)
}

func TestQueueHelpRowsDistinguishesRerunActions(t *testing.T) {
	rows := newModel(testEndpoint, withExternalIODisabled()).queueHelpRows()
	labels := make(map[string]string)
	for _, row := range rows {
		for _, item := range row {
			labels[item.Key] = item.Description
		}
	}

	assert.Equal(t, "rerun", labels["r"])
	assert.Equal(t, "rerun new agent", labels["R"])
}

func TestHelpLinesShowDisabledTasksShortcuts(t *testing.T) {
	disabled := strings.Join(helpLines(false, false), "\n")
	assert.Contains(t, stripTestANSI(disabled), "Trigger fix for selected review (disabled)")
	assert.Contains(t, stripTestANSI(disabled), "Trigger fix (opens inline panel) (disabled)")
	assert.Contains(t, stripTestANSI(disabled), "Open Tasks view (disabled)")
	assert.Contains(t, stripTestANSI(disabled), "advanced.tasks_enabled = false")

	enabled := strings.Join(helpLines(true, false), "\n")
	assert.NotContains(t, stripTestANSI(enabled), "(disabled)")
}

func TestSanitizeForDisplay(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "plain text unchanged",
			input:    "Hello, world!",
			expected: "Hello, world!",
		},
		{
			name:     "preserves newlines and tabs",
			input:    "Line1\n\tIndented",
			expected: "Line1\n\tIndented",
		},
		{
			name:     "strips ANSI color codes",
			input:    "\x1b[31mred text\x1b[0m",
			expected: "red text",
		},
		{
			name:     "strips cursor movement",
			input:    "\x1b[2Jhello\x1b[H",
			expected: "hello",
		},
		{
			name:     "strips OSC sequences (title set with BEL)",
			input:    "\x1b]0;Evil Title\x07normal text",
			expected: "normal text",
		},
		{
			name:     "strips OSC sequences (title set with ST)",
			input:    "\x1b]0;Evil Title\x1b\\normal text",
			expected: "normal text",
		},
		{
			name:     "strips control characters",
			input:    "hello\x00world\x07\x08test",
			expected: "helloworldtest",
		},
		{
			name:     "handles complex escape sequence",
			input:    "\x1b[1;32mBold Green\x1b[0m and \x1b[4munderline\x1b[24m",
			expected: "Bold Green and underline",
		},
		{
			name:     "empty string unchanged",
			input:    "",
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeForDisplay(tt.input)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestPatchFiles(t *testing.T) {
	tests := []struct {
		name  string
		patch string
		want  []string
	}{
		{
			name: "simple add",
			patch: `diff --git a/main.go b/main.go
--- a/main.go
+++ b/main.go
@@ -1 +1,2 @@
 package main
+// new line
`,
			want: []string{"main.go"},
		},
		{
			name: "file in b/ directory not double-stripped",
			patch: `diff --git a/b/main.go b/b/main.go
--- a/b/main.go
+++ b/b/main.go
@@ -1 +1,2 @@
 package main
+// new line
`,
			want: []string{"b/main.go"},
		},
		{
			name: "file in a/ directory not double-stripped",
			patch: `diff --git a/a/utils.go b/a/utils.go
--- a/a/utils.go
+++ b/a/utils.go
@@ -1 +1,2 @@
 package a
+// new line
`,
			want: []string{"a/utils.go"},
		},
		{
			name: "new file with /dev/null",
			patch: `diff --git a/new.go b/new.go
--- /dev/null
+++ b/new.go
@@ -0,0 +1 @@
+package main
`,
			want: []string{"new.go"},
		},
		{
			name: "deleted file with /dev/null",
			patch: `diff --git a/old.go b/old.go
--- a/old.go
+++ /dev/null
@@ -1 +0,0 @@
-package main
`,
			want: []string{"old.go"},
		},
		{
			name: "rename",
			patch: `diff --git a/old.go b/renamed.go
--- a/old.go
+++ b/renamed.go
@@ -1 +1 @@
-package old
+package renamed
`,
			want: []string{"old.go", "renamed.go"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := patchFiles(tt.patch)
			require.NoError(t, err, "patchFiles returned error: %v")

			wantSet := map[string]bool{}
			for _, f := range tt.want {
				wantSet[f] = true
			}
			assert.Len(t, tt.want, len(got))
			gotSet := map[string]bool{}
			for _, f := range got {
				assert.False(t, gotSet[f])
				gotSet[f] = true
			}
			for f := range wantSet {
				assert.True(t, gotSet[f])
			}
			for f := range gotSet {
				assert.True(t, wantSet[f])
			}
		})
	}
}

func TestShortRef(t *testing.T) {
	tests := []struct {
		name string
		ref  string
		want string
	}{
		{
			name: "full SHA",
			ref:  "abc1234567890def1234567890abcdef12345678",
			want: "abc1234",
		},
		{
			name: "already short",
			ref:  "abc12",
			want: "abc12",
		},
		{
			name: "exactly 7 chars",
			ref:  "abc1234",
			want: "abc1234",
		},
		{
			name: "range of full SHAs",
			ref:  "abc1234567890def1234567890abcdef12345678..fed9876543210abc9876543210fedcba98765432",
			want: "abc1234..fed9876",
		},
		{
			name: "range of short SHAs",
			ref:  "abc..def",
			want: "abc..def",
		},
		{
			name: "range with one long side",
			ref:  "abc1234567890..def",
			want: "abc1234..def",
		},
		{
			name: "range with branch endpoint",
			ref:  "abc1234567890..feature/long-name",
			want: "abc1234..feature/long-name",
		},
		{
			name: "range with revision suffix",
			ref:  "abc1234567890^..feature/long-name",
			want: "abc1234^..feature/long-name",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shortRef(tt.ref)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestShortJobRef(t *testing.T) {
	fullSHA1 := "abc1234567890def1234567890abcdef12345678"
	fullSHA2 := "fed9876543210abc9876543210fedcba98765432"
	commitID := int64(1)
	diffContent := "diff"

	tests := []struct {
		name string
		job  storage.ReviewJob
		want string
	}{
		{
			name: "single commit",
			job:  storage.ReviewJob{GitRef: fullSHA1, CommitID: &commitID},
			want: "abc1234",
		},
		{
			name: "range with nil CommitID",
			job:  storage.ReviewJob{GitRef: fullSHA1 + ".." + fullSHA2},
			want: "abc1234..fed9876",
		},
		{
			name: "prompt job",
			job:  storage.ReviewJob{GitRef: "prompt"},
			want: "run",
		},
		{
			name: "task ref without commit",
			job:  storage.ReviewJob{GitRef: "analyze"},
			want: "analyze",
		},
		{
			name: "dirty review",
			job: storage.ReviewJob{
				GitRef:      fullSHA1,
				DiffContent: &diffContent,
			},
			want: "abc1234",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shortJobRef(tt.job)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestDirtyPatchFilesError(t *testing.T) {
	// dirtyPatchFiles should return an error when git diff fails
	// (e.g., invalid repo path), not silently return nil.
	missingPath := filepath.Join(t.TempDir(), "missing")
	_, err := dirtyPatchFiles(missingPath, []string{"file.go"})
	require.Error(t, err)

	assert.Contains(t, err.Error(), "git diff")
}

func TestWrapLine(t *testing.T) {
	tests := []struct {
		name  string
		line  string
		width int
		want  []string
	}{
		{
			name:  "short line unchanged",
			line:  "hello world",
			width: 20,
			want:  []string{"hello world"},
		},
		{
			name:  "empty line",
			line:  "",
			width: 20,
			want:  []string{""},
		},
		{
			name:  "breaks at word boundary",
			line:  "hello world foo bar",
			width: 11,
			want:  []string{"hello", "world foo", "bar"},
		},
		{
			name:  "long word forced break",
			line:  "abcdefghijklmnop",
			width: 10,
			want:  []string{"abcdefghij", "klmnop"},
		},
		{
			name:  "wide characters",
			line:  "あいうえおかきくけこ",
			width: 10,
			want:  []string{"あいうえお", "かきくけこ"},
		},
		{
			name:  "multiple wraps",
			line:  "one two three four five six",
			width: 10,
			want:  []string{"one two", "three", "four five", "six"},
		},
		{
			name:  "preserves leading spaces in remainder",
			line:  "if err != nil {   return err }",
			width: 15,
			want:  []string{"if err != nil", "{   return err", "}"},
		},
		{
			name:  "indented continuation preserved",
			line:  "func foo()     bar baz",
			width: 12,
			want:  []string{"func foo() ", "    bar baz"},
		},
		{
			name:  "leading spaces preserved after wrap",
			line:  "aaaaaaaaaa   bbb",
			width: 10,
			want:  []string{"aaaaaaaaaa", "   bbb"},
		},
		{
			name:  "whitespace-only line preserves all spaces",
			line:  "            ",
			width: 10,
			want:  []string{"         ", "   "},
		},
		{
			name:  "indentation-only prefix preserved",
			line:  "    x",
			width: 3,
			want:  []string{"  ", "  x"},
		},
		{
			name:  "very narrow width no infinite loop",
			line:  " ab cd",
			width: 2,
			want:  []string{" a", "b", "cd"},
		},
		{
			name:  "width 1 terminates",
			line:  "abc",
			width: 1,
			want:  []string{"a", "b", "c"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := wrapLine(tt.line, tt.width)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestStripTrailingPadding(t *testing.T) {
	bold := "\x1b[1m"
	underline := "\x1b[4m"
	reset := "\x1b[0m"
	red := "\x1b[31m"

	tests := []struct {
		name    string
		line    string
		noColor bool
		want    string
	}{
		{
			name:    "color mode appends reset",
			line:    red + "hello" + reset,
			noColor: false,
			want:    red + "hello" + reset,
		},
		{
			name:    "color mode strips trailing padding",
			line:    red + "hello" + reset + "   " + reset,
			noColor: false,
			want:    red + "hello" + reset,
		},
		{
			name:    "noColor strips mid-line bold",
			line:    bold + "hello" + reset,
			noColor: true,
			want:    "hello",
		},
		{
			name:    "noColor strips mid-line underline",
			line:    underline + "text" + reset,
			noColor: true,
			want:    "text",
		},
		{
			name:    "noColor strips mixed SGR sequences",
			line:    bold + underline + "mixed" + reset + " plain",
			noColor: true,
			want:    "mixed plain",
		},
		{
			name:    "noColor preserves plain text",
			line:    "no formatting here",
			noColor: true,
			want:    "no formatting here",
		},
		{
			name:    "noColor strips trailing padding and all SGR",
			line:    bold + "word" + reset + "   ",
			noColor: true,
			want:    "word",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripTrailingPadding(tt.line, tt.noColor)
			assert.Equal(t, tt.want, got)
		})
	}
}
