package streamfmt

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode"

	gansi "charm.land/glamour/v2/ansi"
	"charm.land/glamour/v2/styles"
	"github.com/muesli/termenv"
	"golang.org/x/term"

	"go.kenn.io/roborev/internal/termstyle"
)

// TerminalWidth returns the terminal width for the given writer, defaulting
// to 100 if detection fails.
func TerminalWidth(w io.Writer) int {
	if f, ok := w.(interface{ Fd() uintptr }); ok {
		if width, _, err := term.GetSize(int(f.Fd())); err == nil && width > 0 {
			return width
		}
	}
	return 100
}

// GlamourStyle returns a glamour style config with zero margins, matching the
// TUI's rendering. It respects ROBOREV_COLOR_MODE and NO_COLOR.
func GlamourStyle() gansi.StyleConfig {
	mode := strings.ToLower(os.Getenv("ROBOREV_COLOR_MODE"))
	var style gansi.StyleConfig
	isDark := true
	switch {
	case mode == "dark":
		style = styles.DarkStyleConfig
	case mode == "light":
		style = styles.LightStyleConfig
		isDark = false
	case mode == "none" || termenv.EnvNoColor():
		style = styles.DarkStyleConfig
	default:
		style = styles.LightStyleConfig
		isDark = termenv.HasDarkBackground()
		if isDark {
			style = styles.DarkStyleConfig
		}
	}
	termstyle.SetDarkBackground(isDark)
	zeroMargin := uint(0)
	style.Document.Margin = &zeroMargin
	style.CodeBlock.Margin = &zeroMargin
	style.Code.Prefix = ""
	style.Code.Suffix = ""
	return style
}

// ResolveColorProfile returns the configured terminal color profile.
func ResolveColorProfile() termenv.Profile {
	mode := strings.ToLower(os.Getenv("ROBOREV_COLOR_MODE"))
	if mode == "none" || termenv.EnvNoColor() {
		return termenv.Ascii
	}
	return termenv.EnvColorProfile()
}

// WriterIsTerminal reports whether w is backed by a terminal.
func WriterIsTerminal(w io.Writer) bool {
	if f, ok := w.(interface{ Fd() uintptr }); ok {
		return term.IsTerminal(int(f.Fd()))
	}
	return false
}

// PrintMarkdownOrPlain renders text as styled Markdown for a terminal and
// writes it unchanged otherwise.
func PrintMarkdownOrPlain(w io.Writer, text string) {
	if !WriterIsTerminal(w) {
		_, _ = fmt.Fprintln(w, text)
		return
	}
	width := TerminalWidth(w)
	for _, line := range RenderMarkdownLines(
		text, width, width, GlamourStyle(), 2, ResolveColorProfile(),
	) {
		_, _ = fmt.Fprintln(w, line)
	}
}

// sanitizeControl strips ANSI escapes and control characters. Newlines become
// spaces so the result is safe for a one-line summary.
func sanitizeControl(s string) string {
	return sanitizeControlChars(s, false)
}

// SanitizeControlKeepNewlines strips ANSI escapes and control characters but
// preserves normalized newlines.
func SanitizeControlKeepNewlines(s string) string {
	return sanitizeControlChars(s, true)
}

func sanitizeControlChars(s string, keepNewlines bool) string {
	s = ansiEscapePattern.ReplaceAllString(s, "")
	if keepNewlines {
		s = strings.ReplaceAll(s, "\r\n", "\n")
		s = strings.ReplaceAll(s, "\r", "\n")
	} else {
		s = strings.ReplaceAll(s, "\r\n", " ")
		s = strings.ReplaceAll(s, "\n", " ")
		s = strings.ReplaceAll(s, "\r", " ")
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r == '\t' || r == '\n' || !unicode.IsControl(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// RenderLog renders a log without inferring a provider. TTY output therefore
// uses the literal decoder; callers with job identity use RenderLogWith and an
// explicitly selected formatter.
func RenderLog(r io.Reader, w io.Writer, isTTY bool) error {
	return RenderLogWith(r, New(w, isTTY, DecoderForAgent("")))
}

// RenderLogWith renders a complete log with a preconfigured formatter.
func RenderLogWith(r io.Reader, fmtr *Formatter) error {
	return renderLogWith(r, fmtr, true)
}

// RenderLogChunkWith renders an incremental log chunk without finalizing
// provider state that may continue in a later chunk.
func RenderLogChunkWith(r io.Reader, fmtr *Formatter) error {
	return renderLogWith(r, fmtr, false)
}

func renderLogWith(r io.Reader, fmtr *Formatter, final bool) error {
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadString('\n')
		line = strings.TrimRight(line, "\n\r")
		if line != "" {
			if LooksLikeJSON(line) {
				if _, writeErr := fmtr.Write([]byte(line + "\n")); writeErr != nil {
					return writeErr
				}
			} else if writeErr := fmtr.renderPlain(line); writeErr != nil {
				return writeErr
			}
		} else if err != io.EOF {
			if writeErr := fmtr.renderPlain(""); writeErr != nil {
				return writeErr
			}
		}

		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}

	if final {
		fmtr.Flush()
	} else {
		fmtr.flushInput()
	}
	return fmtr.err()
}

// LooksLikeJSON reports whether line is a JSON object with a non-empty type.
// It separates provider frames from literal stderr; it does not select a
// provider decoder.
func LooksLikeJSON(line string) bool {
	for _, c := range line {
		switch c {
		case ' ', '\t':
			continue
		case '{':
			var probe struct{ Type string }
			if json.Unmarshal([]byte(line), &probe) != nil {
				return false
			}
			return probe.Type != ""
		default:
			return false
		}
	}
	return false
}
