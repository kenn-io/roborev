package streamfmt

import (
	"regexp"
	"strings"

	"charm.land/glamour/v2"
	gansi "charm.land/glamour/v2/ansi"
	xansi "github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-runewidth"
	"github.com/muesli/termenv"
)

// ansiEscapePattern matches ANSI escape sequences (colors, cursor
// movement, etc.). Handles CSI sequences (\x1b[...X) and OSC
// sequences terminated by BEL (\x07) or ST (\x1b\\).
var ansiEscapePattern = regexp.MustCompile(
	`\x1b\[[0-9;?]*[a-zA-Z]` +
		`|\x1b\]([^\x07\x1b]|\x1b[^\\])*(\x07|\x1b\\)`,
)

// StripANSI removes ANSI escape sequences from s.
func StripANSI(s string) string {
	return ansiEscapePattern.ReplaceAllString(s, "")
}

// nonCSIEscRe matches non-CSI escape sequences: OSC, DCS, and bare ESC.
var nonCSIEscRe = regexp.MustCompile(
	// OSC: ESC ] ... (terminated by BEL or ST)
	`\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)?` +
		`|` +
		// DCS: ESC P ... ST
		`\x1bP[^\x1b]*(?:\x1b\\)?` +
		`|` +
		// Bare ESC followed by single char (e.g. ESC c for RIS)
		`\x1b[^[\]P]`,
)

// csiRe matches all well-formed CSI sequences per ECMA-48:
// ESC [ <parameter bytes 0x30-0x3F>* <intermediate bytes 0x20-0x2F>*
// <final byte 0x40-0x7E>
var csiRe = regexp.MustCompile(
	`\x1b\[[\x30-\x3f]*[\x20-\x2f]*[\x40-\x7e]`,
)

// sgrRe matches SGR (Select Graphic Rendition) sequences:
// ESC [ <digits and semicolons only> m
var sgrRe = regexp.MustCompile(`^\x1b\[[0-9;]*m$`)

// SanitizeEscapes strips non-SGR terminal escape sequences and
// dangerous C0 control characters from a line, preventing untrusted
// content from injecting OSC/CSI/DCS control codes or spoofing output
// via \r/\b overwrites. SGR sequences (colors/styles), tabs, and
// newlines are preserved.
func SanitizeEscapes(line string) string {
	line = nonCSIEscRe.ReplaceAllString(line, "")
	line = csiRe.ReplaceAllStringFunc(line, func(seq string) string {
		if sgrRe.MatchString(seq) {
			return seq
		}
		return ""
	})
	// Strip C0 control characters (0x00-0x1F) except tab (0x09),
	// newline (0x0A), and ESC (0x1B, already handled above).
	// Characters like \r and \b can overwrite displayed content.
	var b strings.Builder
	for _, r := range line {
		if r < 0x20 && r != '\t' && r != '\n' && r != 0x1b {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// SanitizeLines applies SanitizeEscapes to every line in the slice.
func SanitizeLines(lines []string) []string {
	for i, line := range lines {
		lines[i] = SanitizeEscapes(line)
	}
	return lines
}

// trailingPadRe matches trailing whitespace and ANSI SGR sequences.
// Glamour pads code block lines with spaces (using background color)
// to fill the wrap width. Stripping prevents overflow on narrow
// terminals.
var trailingPadRe = regexp.MustCompile(`(\s|\x1b\[[0-9;]*m)+$`)

// allSGRRe matches any ANSI SGR escape sequence.
var allSGRRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// StripTrailingPadding removes trailing whitespace and ANSI SGR codes
// from a glamour output line, then appends a reset to ensure clean
// color state. When noColor is true, all SGR sequences are stripped
// to prevent open attributes (bold, underline) from bleeding across
// lines.
func StripTrailingPadding(line string, noColor bool) string {
	line = trailingPadRe.ReplaceAllString(line, "")
	if noColor {
		return allSGRRe.ReplaceAllString(line, "")
	}
	return line + "\x1b[0m"
}

// WrapText wraps text to the specified width, preserving existing
// line breaks and breaking at word boundaries when possible. Uses
// runewidth for correct display width with Unicode and wide
// characters.
func WrapText(text string, width int) []string {
	if width <= 0 {
		width = 100
	}

	var result []string
	for line := range strings.SplitSeq(text, "\n") {
		lineWidth := runewidth.StringWidth(line)
		if lineWidth <= width {
			result = append(result, line)
			continue
		}

		// Wrap long lines using display width
		for runewidth.StringWidth(line) > width {
			runes := []rune(line)
			breakPoint := 0
			currentWidth := 0

			for i, r := range runes {
				rw := runewidth.RuneWidth(r)
				if currentWidth+rw > width {
					break
				}
				currentWidth += rw
				breakPoint = i + 1
			}

			// Ensure forward progress
			if breakPoint == 0 {
				breakPoint = 1
			}

			// Try to find a space to break at
			bestBreak := breakPoint
			scanWidth := 0
			for i := breakPoint - 1; i >= 0; i-- {
				if runes[i] == ' ' {
					bestBreak = i
					break
				}
				scanWidth += runewidth.RuneWidth(runes[i])
				if scanWidth > width/2 {
					break
				}
			}

			result = append(result, string(runes[:bestBreak]))
			line = strings.TrimLeft(
				string(runes[bestBreak:]), " ",
			)
		}
		if len(line) > 0 {
			result = append(result, line)
		}
	}

	return result
}

// TruncateLongLines normalizes tabs and truncates lines inside fenced
// code blocks that exceed maxWidth, so glamour won't word-wrap them.
// Prose lines outside code blocks are left intact for glamour to
// word-wrap naturally.
func TruncateLongLines(
	text string, maxWidth, tabWidth int,
) string {
	if maxWidth <= 0 {
		return text
	}
	if tabWidth <= 0 {
		tabWidth = 2
	} else if tabWidth > 16 {
		tabWidth = 16
	}
	text = strings.ReplaceAll(
		text, "\t", strings.Repeat(" ", tabWidth),
	)
	lines := strings.Split(text, "\n")
	var fenceChar byte
	var fenceLen int
	for i, line := range lines {
		if fenceLen == 0 {
			if ch, n, _ := ParseFence(line); n > 0 {
				fenceChar = ch
				fenceLen = n
			}
			continue
		}
		if ch, n, wsOnly := ParseFence(line); n >= fenceLen && ch == fenceChar && wsOnly {
			fenceLen = 0
			continue
		}
		if runewidth.StringWidth(line) > maxWidth {
			lines[i] = runewidth.Truncate(
				line, maxWidth, "",
			)
		}
	}
	return strings.Join(lines, "\n")
}

// ParseFence checks if line is a CommonMark fenced code block
// delimiter. Returns the fence character ('`' or '~'), the run
// length, and whether trailing content is whitespace-only (required
// for closing fences). Returns (0, 0, false) if not a valid fence.
func ParseFence(line string) (byte, int, bool) {
	indent := 0
	for indent < 3 && indent < len(line) && line[indent] == ' ' {
		indent++
	}
	rest := line[indent:]
	if len(rest) < 3 {
		return 0, 0, false
	}
	ch := rest[0]
	if ch != '`' && ch != '~' {
		return 0, 0, false
	}
	n := 0
	for n < len(rest) && rest[n] == ch {
		n++
	}
	if n < 3 {
		return 0, 0, false
	}
	trailing := rest[n:]
	wsOnly := strings.TrimSpace(trailing) == ""
	if ch == '`' && strings.ContainsRune(trailing, '`') {
		return 0, 0, false
	}
	return ch, n, wsOnly
}

// RenderMarkdownLines renders markdown text using glamour and splits
// into lines. wrapWidth controls glamour's word-wrap column.
// maxWidth controls line truncation (actual terminal width).
// colorProfile controls post-render color stripping.
// Falls back to WrapText if glamour rendering fails.
func RenderMarkdownLines(
	text string, wrapWidth, maxWidth int,
	glamourStyle gansi.StyleConfig, tabWidth int,
	colorProfile termenv.Profile,
) []string {
	text = TruncateLongLines(text, maxWidth, tabWidth)
	r, err := glamour.NewTermRenderer(
		glamour.WithStyles(glamourStyle),
		glamour.WithWordWrap(wrapWidth),
		glamour.WithPreservedNewLines(),
	)
	if err != nil {
		return SanitizeLines(WrapText(text, wrapWidth))
	}
	out, err := r.Render(text)
	if err != nil {
		return SanitizeLines(WrapText(text, wrapWidth))
	}
	noColor := colorProfile == termenv.Ascii
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	for i, line := range lines {
		line = StripTrailingPadding(line, noColor)
		line = SanitizeEscapes(line)
		if xansi.StringWidth(line) > maxWidth {
			line = xansi.Truncate(line, maxWidth, "")
		}
		lines[i] = line
	}
	return lines
}
