package streamfmt

import (
	"bytes"
	"io"
	"strings"

	gansi "charm.land/glamour/v2/ansi"
)

// Formatter composes partial-line buffering, one explicitly selected decoder,
// and one terminal renderer. Non-TTY writes remain byte-for-byte passthrough.
type Formatter struct {
	buf      []byte
	isTTY    bool
	decoder  Decoder
	renderer *Renderer
}

// New creates a formatter using decoder for TTY stream interpretation.
func New(w io.Writer, isTTY bool, decoder Decoder) *Formatter {
	if decoder == nil {
		decoder = &literalDecoder{}
	}
	width := 0
	style := gansi.StyleConfig{}
	if isTTY {
		width = TerminalWidth(w)
		style = GlamourStyle()
	}
	return &Formatter{
		isTTY:   isTTY,
		decoder: decoder,
		renderer: newRenderer(
			w, width, style, ResolveColorProfile(),
		),
	}
}

// NewWithWidth creates a TTY formatter with an explicit width and style.
func NewWithWidth(
	w io.Writer, width int, style gansi.StyleConfig, decoder Decoder,
) *Formatter {
	if decoder == nil {
		decoder = &literalDecoder{}
	}
	return &Formatter{
		isTTY:   true,
		decoder: decoder,
		renderer: newRenderer(
			w, width, style, ResolveColorProfile(),
		),
	}
}

// Width returns the configured terminal width.
func (f *Formatter) Width() int {
	return f.renderer.Width()
}

// SetWriter redirects future output without resetting decoder state.
func (f *Formatter) SetWriter(w io.Writer) {
	f.renderer.SetWriter(w)
}

func (f *Formatter) Write(p []byte) (int, error) {
	if !f.isTTY {
		return f.renderer.writeRaw(p)
	}

	n := len(p)
	f.buf = append(f.buf, p...)
	for {
		idx := bytes.IndexByte(f.buf, '\n')
		if idx < 0 {
			break
		}
		line := string(f.buf[:idx])
		f.buf = f.buf[idx+1:]
		f.processLine(line)
	}
	return n, f.renderer.Err()
}

// Flush processes an unterminated line and finalizes decoder state.
func (f *Formatter) Flush() {
	f.flushInput()
	f.renderer.Render(f.decoder.Flush())
}

// flushInput processes buffered input without finalizing decoder state.
func (f *Formatter) flushInput() {
	if len(f.buf) == 0 {
		return
	}
	line := string(f.buf)
	f.buf = nil
	f.processLine(line)
}

func (f *Formatter) processLine(line string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}
	f.renderer.Render(f.decoder.Decode(line))
}

func (f *Formatter) renderPlain(text string) error {
	// A literal stderr line is a semantic boundary for buffered provider text.
	f.renderer.Render(f.decoder.Flush())
	f.renderer.RenderPlain(text)
	return f.renderer.Err()
}

func (f *Formatter) err() error {
	return f.renderer.Err()
}
