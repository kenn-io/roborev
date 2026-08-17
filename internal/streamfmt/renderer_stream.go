package streamfmt

import (
	"fmt"
	"image/color"
	"io"
	"strings"

	gansi "charm.land/glamour/v2/ansi"
	"charm.land/lipgloss/v2"
	"github.com/muesli/termenv"

	"go.kenn.io/roborev/internal/termstyle"
)

var (
	sfToolStyle = lipgloss.NewStyle().
			Foreground(adaptiveColor("30", "51"))
	sfArgStyle = lipgloss.NewStyle().
			Foreground(adaptiveColor("242", "246"))
	sfGutterStyle = lipgloss.NewStyle().
			Foreground(adaptiveColor("242", "240"))
	sfReasoningStyle = lipgloss.NewStyle().
				Foreground(adaptiveColor("242", "243")).Italic(true)
)

func adaptiveColor(light, dark string) color.Color {
	return termstyle.AdaptiveColor(light, dark)
}

// Renderer owns terminal styling, wrapping, spacing, and sticky write errors.
// It consumes only provider-neutral events.
type Renderer struct {
	w            io.Writer
	width        int
	glamourStyle gansi.StyleConfig
	colorProfile termenv.Profile
	writeErr     error
	lastWasTool  bool
	hasOutput    bool
}

func newRenderer(
	w io.Writer,
	width int,
	style gansi.StyleConfig,
	profile termenv.Profile,
) *Renderer {
	return &Renderer{
		w: w, width: width, glamourStyle: style, colorProfile: profile,
	}
}

// Render writes neutral events using the configured terminal presentation.
func (r *Renderer) Render(events []Event) {
	for _, event := range events {
		switch event.Kind {
		case EventText:
			r.writeText(event.Text)
		case EventReasoning:
			r.writeReasoning(event.Text)
		case EventTool:
			r.writeTool(event.Name, event.Arg)
		case EventLiteral:
			r.RenderPlain(event.Text)
		case EventBoundary:
			// Boundaries carry decoder semantics but add no terminal output.
		}
	}
}

// RenderPlain sanitizes and wraps literal stderr without parsing JSON or
// interpreting Markdown.
func (r *Renderer) RenderPlain(text string) {
	text = SanitizeControlKeepNewlines(text)
	if r.width <= 0 {
		r.writef("%s\n", text)
		return
	}
	for _, line := range WrapText(text, r.width) {
		r.writef("%s\n", line)
	}
}

// SetWriter redirects future output while retaining spacing and error state.
func (r *Renderer) SetWriter(w io.Writer) {
	r.w = w
}

// Width returns the configured terminal width.
func (r *Renderer) Width() int {
	return r.width
}

// Err returns the first write error observed by the renderer.
func (r *Renderer) Err() error {
	return r.writeErr
}

func (r *Renderer) writeRaw(p []byte) (int, error) {
	if r.writeErr != nil {
		return 0, r.writeErr
	}
	if r.w == nil {
		return len(p), nil
	}
	n, err := r.w.Write(p)
	if err != nil {
		r.writeErr = err
	}
	return n, err
}

func (r *Renderer) writef(format string, args ...any) {
	if r.writeErr != nil || r.w == nil {
		return
	}
	_, r.writeErr = fmt.Fprintf(r.w, format, args...)
}

func (r *Renderer) writeText(text string) {
	text = strings.TrimSpace(SanitizeControlKeepNewlines(text))
	if text == "" {
		return
	}
	if r.lastWasTool && r.hasOutput {
		r.writef("\n")
	}
	r.lastWasTool = false
	r.hasOutput = true
	if r.width <= 0 {
		r.writef("%s\n", text)
		return
	}
	for _, line := range RenderMarkdownLines(
		text,
		r.width,
		r.width,
		r.glamourStyle,
		2,
		r.colorProfile,
	) {
		r.writef("%s\n", line)
	}
}

func (r *Renderer) writeReasoning(text string) {
	text = strings.TrimSpace(sanitizeControl(text))
	if text == "" {
		return
	}
	if r.lastWasTool && r.hasOutput {
		r.writef("\n")
	}
	r.lastWasTool = false
	r.hasOutput = true
	r.writef("%s\n", sfReasoningStyle.Render(text))
}

func (r *Renderer) writeTool(name, arg string) {
	name = sanitizeControl(name)
	arg = sanitizeControl(arg)
	if !r.lastWasTool && r.hasOutput {
		r.writef("\n")
	}
	r.lastWasTool = true
	r.hasOutput = true
	styled := fmt.Sprintf(
		"%s %s %s",
		sfGutterStyle.Render(" │"),
		sfToolStyle.Render(fmt.Sprintf("%-6s", name)),
		sfArgStyle.Render(arg),
	)
	r.writef("%s\n", styled)
}
