package streamfmt

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"image/color"
	"io"
	"os"
	"strings"
	"unicode"

	gansi "charm.land/glamour/v2/ansi"
	"charm.land/glamour/v2/styles"
	"charm.land/lipgloss/v2"
	"github.com/muesli/termenv"
	"golang.org/x/term"

	"go.kenn.io/roborev/internal/termstyle"
)

// Styles for TTY-mode stream output.
var (
	sfToolStyle = lipgloss.NewStyle().
			Foreground(adaptiveColor("30", "51")) // Cyan — matches tuiClosedStyle
	sfArgStyle = lipgloss.NewStyle().
			Foreground(adaptiveColor("242", "246")) // Gray — de-emphasize detail
	sfGutterStyle = lipgloss.NewStyle().
			Foreground(adaptiveColor("242", "240")) // Dim — subtle visual grouping
	sfReasoningStyle = lipgloss.NewStyle().
				Foreground(adaptiveColor("242", "243")).Italic(true) // Dim italic — thinking indicator
)

func adaptiveColor(light, dark string) color.Color {
	return termstyle.AdaptiveColor(light, dark)
}

// Formatter wraps an io.Writer to transform raw NDJSON stream output
// from Claude into compact, human-readable progress lines.
//
// In TTY mode, tool calls are shown as one-line summaries:
//
//	Read  internal/gmail/ratelimit_test.go
//	Edit  internal/gmail/ratelimit_test.go
//	Bash  go test ./internal/gmail/ -run TestRateLimiter
//
// In non-TTY mode (piped output), raw JSON is passed through unchanged.
type Formatter struct {
	w     io.Writer
	buf   []byte
	isTTY bool
	width int // terminal width; 0 = no wrapping

	glamourStyle gansi.StyleConfig // detected once at init
	colorProfile termenv.Profile   // color profile for glamour (Ascii when NO_COLOR)

	writeErr    error // first write error encountered during formatting
	lastWasTool bool  // tracks tool vs text transitions for spacing
	hasOutput   bool  // whether any output has been written

	// Tracks opencode tool call IDs that have already been rendered.
	opencodeRenderedToolIDs map[string]struct{}
	piRenderedToolIDs       map[string]struct{}
	piLastAssistantText     string
	codexCommands           codexCommandTracker
}

// New creates a Formatter that writes to w. When isTTY is true,
// NDJSON lines are rendered as compact progress lines; otherwise
// raw JSON is passed through unchanged.
func New(w io.Writer, isTTY bool) *Formatter {
	f := &Formatter{w: w, isTTY: isTTY}
	if isTTY {
		f.glamourStyle = GlamourStyle()
		f.colorProfile = ResolveColorProfile()
		f.width = TerminalWidth(w)
	}
	return f
}

// NewWithWidth creates a Formatter with an explicit width and
// pre-computed glamour style. Used when rendering into a buffer
// (e.g. the TUI log view) where terminal queries aren't possible.
func NewWithWidth(
	w io.Writer, width int, style gansi.StyleConfig,
) *Formatter {
	return &Formatter{
		w: w, isTTY: true, width: width,
		glamourStyle: style, colorProfile: ResolveColorProfile(),
	}
}

// TerminalWidth returns the terminal width for the given writer,
// defaulting to 100 if detection fails.
func TerminalWidth(w io.Writer) int {
	if f, ok := w.(interface{ Fd() uintptr }); ok {
		if w, _, err := term.GetSize(int(f.Fd())); err == nil && w > 0 {
			return w
		}
	}
	return 100
}

// GlamourStyle returns a glamour style config with zero margins,
// matching the TUI's rendering. Detects dark/light background once.
// Respects ROBOREV_COLOR_MODE env var and NO_COLOR convention.
func GlamourStyle() gansi.StyleConfig {
	mode := strings.ToLower(os.Getenv("ROBOREV_COLOR_MODE"))
	var style gansi.StyleConfig
	isDark := true
	switch {
	case mode == "none" || termenv.EnvNoColor():
		// Use dark style as base; colors will be stripped by Ascii profile.
		style = styles.DarkStyleConfig
	case mode == "dark":
		style = styles.DarkStyleConfig
	case mode == "light":
		style = styles.LightStyleConfig
		isDark = false
	default: // "auto" or ""
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

// ResolveColorProfile returns the termenv color profile based on
// the ROBOREV_COLOR_MODE env var and the NO_COLOR convention.
// Returns termenv.Ascii when colors should be suppressed.
func ResolveColorProfile() termenv.Profile {
	mode := strings.ToLower(os.Getenv("ROBOREV_COLOR_MODE"))
	if mode == "none" || termenv.EnvNoColor() {
		return termenv.Ascii
	}
	return termenv.EnvColorProfile()
}

// Width returns the configured terminal width.
func (f *Formatter) Width() int {
	return f.width
}

// SetWriter replaces the underlying writer. Used to redirect a
// persistent formatter's output to a fresh buffer for incremental
// rendering.
func (f *Formatter) SetWriter(w io.Writer) {
	f.w = w
}

func (f *Formatter) Write(p []byte) (int, error) {
	if !f.isTTY {
		return f.w.Write(p)
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
	if f.writeErr != nil {
		return n, f.writeErr
	}
	return n, nil
}

// Flush writes any remaining buffered content.
func (f *Formatter) Flush() {
	if len(f.buf) > 0 {
		line := string(f.buf)
		f.buf = nil
		f.processLine(line)
	}
}

// streamEvent is a unified representation of stream-json events from
// Claude Code, Gemini CLI, and Codex CLI.
//
// Claude:  {"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{...}}]}}
// Gemini:  {"type":"tool_use","tool_name":"read_file","parameters":{"file_path":"..."}}
//
//	{"type":"message","role":"assistant","content":"...","delta":true}
//
// Codex:   {"type":"item.completed","item":{"type":"agent_message","text":"..."}}
//
//	{"type":"item.started","item":{"type":"command_execution","command":"bash -lc ls"}}
type streamEvent struct {
	Type string `json:"type"`
	// Claude: nested message with content blocks
	Message *struct {
		Role    string          `json:"role,omitempty"`
		Content json.RawMessage `json:"content,omitempty"`
	} `json:"message,omitempty"`
	// Gemini: top-level fields
	Role       string          `json:"role,omitempty"`
	Content    json.RawMessage `json:"content,omitempty"`
	ToolName   string          `json:"tool_name,omitempty"`
	Parameters json.RawMessage `json:"parameters,omitempty"`
	// Codex: item events
	Item *codexItem `json:"item,omitempty"`
	// OpenCode: nested part payload
	Part json.RawMessage `json:"part,omitempty"`
	// Pi: message/tool execution events
	AssistantMessageEvent *piAssistantMessageEvent `json:"assistantMessageEvent,omitempty"`
	ToolCallID            string                   `json:"toolCallId,omitempty"`
	PiToolName            string                   `json:"toolName,omitempty"`
	Args                  json.RawMessage          `json:"args,omitempty"`
}

// codexItem represents the item field in codex JSONL events.
type codexItem struct {
	ID      string `json:"id,omitempty"`
	Type    string `json:"type,omitempty"`
	Text    string `json:"text,omitempty"`
	Command string `json:"command,omitempty"`
}

// opencodeToolPart represents the part payload for opencode tool events.
type opencodeToolPart struct {
	Type  string `json:"type"`
	Tool  string `json:"tool"`
	ID    string `json:"id,omitempty"`
	State struct {
		Status string                     `json:"status,omitempty"`
		Input  map[string]json.RawMessage `json:"input,omitempty"`
	} `json:"state"`
}

type piAssistantMessageEvent struct {
	Type    string `json:"type"`
	Content string `json:"content,omitempty"`
}

type contentBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

// toolAliases maps case- and underscore-normalized tool names to a
// canonical display name. Covers the variants emitted by Claude Code,
// Gemini CLI, opencode, and similar agents so the renderer does not
// need per-agent special cases for cosmetic naming differences.
var toolAliases = map[string]string{
	"read":            "Read",
	"readfile":        "Read",
	"edit":            "Edit",
	"multiedit":       "Edit",
	"replace":         "Edit",
	"write":           "Write",
	"writefile":       "Write",
	"bash":            "Bash",
	"runshellcommand": "Bash",
	"shell":           "Bash",
	"grep":            "Grep",
	"search":          "Grep",
	"glob":            "Glob",
	"list":            "List",
	"listdir":         "List",
	"ls":              "List",
	"webfetch":        "WebFetch",
	"fetch":           "WebFetch",
}

// normalizeToolKey lowercases s and strips underscores/dashes so
// "file_path", "filePath", "FILE-PATH", and "FilePath" all collapse
// to the same lookup key.
func normalizeToolKey(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r == '_' || r == '-' {
			continue
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

// canonicalToolName returns the display name for a tool identifier
// (e.g. "read_file" -> "Read") or the empty string if no alias is
// registered.
func canonicalToolName(name string) string {
	return toolAliases[normalizeToolKey(name)]
}

// normalizeFields returns a copy of fields with each key normalized
// via normalizeToolKey, enabling case- and separator-insensitive
// lookups while preserving the raw JSON values.
func normalizeFields(
	fields map[string]json.RawMessage,
) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(fields))
	for k, v := range fields {
		out[normalizeToolKey(k)] = v
	}
	return out
}

func (f *Formatter) processLine(line string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}

	var ev streamEvent
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		f.writeText(SanitizeControlKeepNewlines(line))
		return
	}

	switch ev.Type {
	case "assistant":
		// Claude format
		if ev.Message != nil {
			f.processAssistantContent(ev.Message.Content)
		}
	case "message":
		// Gemini format: assistant text
		if ev.Role == "assistant" {
			var text string
			if json.Unmarshal(ev.Content, &text) == nil {
				f.writeText(text)
			}
		}
	case "item.started", "item.completed", "item.updated":
		// Codex format: item events
		f.processCodexItem(ev.Type, ev.Item)
	case "message_update":
		// Pi format: render only completed assistant text.
		f.processPiMessageUpdate(ev.AssistantMessageEvent)
	case "message_end":
		// Pi format: some streams only include the completed
		// assistant content on message_end.
		if ev.Message != nil {
			f.processPiMessageEnd(ev.Message.Role, ev.Message.Content)
		}
	case "tool_execution_start":
		// Pi format: render each tool call once, at start, so
		// running logs show progress without replaying result text.
		f.processPiToolExecution(ev.ToolCallID, ev.PiToolName, ev.Args)
	case "text", "reasoning", "tool", "tool_use":
		// Both Gemini and opencode use "tool_use", and opencode
		// 1.4+ emits "tool_use" where earlier versions used
		// "tool". Route by payload shape: an opencode event
		// carries a nested "part" object; a Gemini tool_use
		// carries top-level tool_name/parameters.
		switch {
		case ev.Part != nil:
			f.processOpenCodePart(ev.Type, ev.Part)
		case ev.Type == "tool_use" && ev.ToolName != "":
			f.formatToolUse(ev.ToolName, ev.Parameters)
		}
	case "step_start", "step_finish":
		// OpenCode lifecycle events — suppress
	case "result", "tool_result", "init",
		"thread.started", "turn.started", "turn.completed",
		"session", "agent_start", "turn_start", "turn_end",
		"agent_end", "message_start",
		"tool_execution_update", "tool_execution_end":
		// Suppress lifecycle events
	default:
		// Suppress system, user, and other events
	}
}

func (f *Formatter) processCodexItem(eventType string, item *codexItem) {
	if item == nil {
		return
	}
	switch item.Type {
	case "reasoning":
		if eventType != "item.completed" {
			return
		}
		text := strings.TrimSpace(sanitizeControl(item.Text))
		if text != "" {
			f.writeReasoning(text)
		}
	case "agent_message":
		if eventType != "item.completed" {
			return
		}
		f.writeText(SanitizeControlKeepNewlines(item.Text))
	case "command_execution":
		cmd := strings.TrimSpace(sanitizeControl(item.Command))
		cmd, render := f.codexCommands.Observe(eventType, codexItem{
			ID:      item.ID,
			Command: cmd,
		})
		if !render {
			return
		}
		if len(cmd) > 80 {
			cmd = cmd[:77] + "..."
		}
		f.writeTool("Bash", cmd)
	case "file_change":
		if eventType != "item.completed" {
			return
		}
		f.writeTool("Edit", "")
	}
}

func (f *Formatter) processOpenCodePart(
	eventType string, raw json.RawMessage,
) {
	switch eventType {
	case "text":
		var part struct{ Text string }
		if json.Unmarshal(raw, &part) == nil && part.Text != "" {
			f.writeText(SanitizeControlKeepNewlines(part.Text))
		}
	case "reasoning":
		var part struct{ Text string }
		if json.Unmarshal(raw, &part) == nil {
			text := strings.TrimSpace(sanitizeControl(part.Text))
			if text != "" {
				f.writeReasoning(text)
			}
		}
	case "tool", "tool_use":
		var tp opencodeToolPart
		if json.Unmarshal(raw, &tp) != nil || tp.Tool == "" {
			return
		}
		// Only render on "running" or "completed" status to
		// skip the initial "pending" event that has no details.
		status := tp.State.Status
		if status != "running" && status != "completed" {
			return
		}
		// Deduplicate by tool call ID.
		if tp.ID != "" {
			if f.opencodeRenderedToolIDs == nil {
				f.opencodeRenderedToolIDs = make(
					map[string]struct{},
				)
			}
			if _, seen := f.opencodeRenderedToolIDs[tp.ID]; seen {
				return
			}
			f.opencodeRenderedToolIDs[tp.ID] = struct{}{}
		}
		f.formatToolUse(tp.Tool, f.opencodeToolInput(tp))
	}
}

func (f *Formatter) processPiMessageUpdate(
	ev *piAssistantMessageEvent,
) {
	if ev == nil || ev.Type != "text_end" {
		return
	}
	f.writePiAssistantText(ev.Content)
}

func (f *Formatter) processPiMessageEnd(
	role string, raw json.RawMessage,
) {
	if role != "assistant" || raw == nil {
		return
	}

	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var parts []string
		for _, block := range blocks {
			if block.Type == "text" && block.Text != "" {
				parts = append(parts, block.Text)
			}
		}
		if len(parts) > 0 {
			f.writePiAssistantText(strings.Join(parts, "\n"))
		}
		return
	}

	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		f.writePiAssistantText(text)
	}
}

func (f *Formatter) writePiAssistantText(text string) {
	text = strings.TrimSpace(SanitizeControlKeepNewlines(text))
	if text == "" || text == f.piLastAssistantText {
		return
	}
	f.piLastAssistantText = text
	f.writeText(text)
}

func (f *Formatter) processPiToolExecution(
	callID, toolName string, args json.RawMessage,
) {
	if callID != "" {
		if f.piRenderedToolIDs == nil {
			f.piRenderedToolIDs = make(map[string]struct{})
		}
		if _, seen := f.piRenderedToolIDs[callID]; seen {
			return
		}
		f.piRenderedToolIDs[callID] = struct{}{}
	}
	f.formatToolUse(toolName, args)
}

// opencodeToolInput returns the raw JSON input map from an opencode
// tool part, suitable for passing to formatToolUse.
func (f *Formatter) opencodeToolInput(
	tp opencodeToolPart,
) json.RawMessage {
	if len(tp.State.Input) == 0 {
		return nil
	}
	b, err := json.Marshal(tp.State.Input)
	if err != nil {
		return nil
	}
	return b
}

func (f *Formatter) processAssistantContent(raw json.RawMessage) {
	if raw == nil {
		return
	}

	// Try as array of content blocks
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		// Try as plain string (legacy format)
		var text string
		if err := json.Unmarshal(raw, &text); err == nil {
			f.writeText(text)
		}
		return
	}

	for _, b := range blocks {
		switch b.Type {
		case "text":
			f.writeText(b.Text)
		case "tool_use":
			f.formatToolUse(b.Name, b.Input)
		}
	}
}

func (f *Formatter) formatToolUse(name string, input json.RawMessage) {
	name = sanitizeControl(name)
	display := canonicalToolName(name)
	if display == "" {
		display = name
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(input, &raw); err != nil || len(raw) == 0 {
		f.writeTool(display, "")
		return
	}
	fields := normalizeFields(raw)

	switch display {
	case "Read", "Edit", "Write":
		path := jsonString(fields["filepath"])
		if path == "" {
			path = jsonString(fields["path"])
		}
		f.writeTool(display, path)
	case "Bash":
		cmd := jsonString(fields["command"])
		if len(cmd) > 80 {
			cmd = cmd[:77] + "..."
		}
		f.writeTool(display, cmd)
	case "Grep":
		pattern := jsonString(fields["pattern"])
		path := jsonString(fields["path"])
		if path != "" {
			f.writeTool(display, pattern+"  "+path)
		} else {
			f.writeTool(display, pattern)
		}
	case "Glob":
		f.writeTool(display, jsonString(fields["pattern"]))
	case "List":
		f.writeTool(display, jsonString(fields["path"]))
	case "WebFetch":
		f.writeTool(display, jsonString(fields["url"]))
	default:
		f.writeTool(display, "")
	}
}

// writef writes formatted output, capturing the first error.
func (f *Formatter) writef(format string, args ...any) {
	if f.writeErr != nil || f.w == nil {
		return
	}
	_, f.writeErr = fmt.Fprintf(f.w, format, args...)
}

// writeText writes agent text, rendering markdown and wrapping to
// terminal width when in TTY mode with a known width.
func (f *Formatter) writeText(text string) {
	text = SanitizeControlKeepNewlines(text)
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	if f.lastWasTool && f.hasOutput {
		f.writef("\n")
	}
	f.lastWasTool = false
	f.hasOutput = true
	if f.width <= 0 {
		f.writef("%s\n", text)
		return
	}
	lines := RenderMarkdownLines(
		text, f.width, f.width, f.glamourStyle, 2, f.colorProfile,
	)
	for _, line := range lines {
		f.writef("%s\n", line)
	}
}

// writeReasoning writes a dimmed reasoning summary line.
func (f *Formatter) writeReasoning(text string) {
	text = SanitizeControlKeepNewlines(text)
	if f.lastWasTool && f.hasOutput {
		f.writef("\n")
	}
	f.lastWasTool = false
	f.hasOutput = true
	f.writef("%s\n", sfReasoningStyle.Render(text))
}

// writeTool writes a styled tool-call line with a gutter prefix
// for visual grouping:
//
//	│ Read   internal/daemon/worker.go
//	│ Edit   internal/daemon/worker.go
func (f *Formatter) writeTool(name, arg string) {
	name = sanitizeControl(name)
	arg = sanitizeControl(arg)
	if !f.lastWasTool && f.hasOutput {
		f.writef("\n")
	}
	f.lastWasTool = true
	f.hasOutput = true
	gutter := sfGutterStyle.Render(" │")
	styled := fmt.Sprintf(
		"%s %s %s",
		gutter,
		sfToolStyle.Render(fmt.Sprintf("%-6s", name)),
		sfArgStyle.Render(arg),
	)
	f.writef("%s\n", styled)
}

// WriterIsTerminal checks if a writer is backed by a terminal.
func WriterIsTerminal(w io.Writer) bool {
	if f, ok := w.(interface{ Fd() uintptr }); ok {
		return isTerminal(f.Fd())
	}
	return false
}

// isTerminal checks if the file descriptor is a terminal.
func isTerminal(fd uintptr) bool {
	return term.IsTerminal(int(fd))
}

// PrintMarkdownOrPlain renders text as glamour-styled markdown when
// writing to a TTY, or prints it as-is otherwise.
func PrintMarkdownOrPlain(w io.Writer, text string) {
	if !WriterIsTerminal(w) {
		fmt.Fprintln(w, text)
		return
	}
	width := TerminalWidth(w)
	style := GlamourStyle()
	lines := RenderMarkdownLines(text, width, width, style, 2, ResolveColorProfile())
	for _, line := range lines {
		fmt.Fprintln(w, line)
	}
}

// sanitizeControl strips ANSI escape sequences and non-printable control
// characters from s. Newlines are replaced with spaces to produce
// single-line output (used for command text summaries).
func sanitizeControl(s string) string {
	return sanitizeControlChars(s, false)
}

// SanitizeControlKeepNewlines strips ANSI escape sequences and
// non-printable control characters but preserves newlines. Used for
// agent text content that needs to retain paragraph structure.
func SanitizeControlKeepNewlines(s string) string {
	return sanitizeControlChars(s, true)
}

func sanitizeControlChars(s string, keepNewlines bool) string {
	s = ansiEscapePattern.ReplaceAllString(s, "")
	if keepNewlines {
		// Normalize line endings but preserve them.
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

// jsonString extracts a string value from a raw JSON field.
func jsonString(raw json.RawMessage) string {
	if raw == nil {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return strings.Trim(string(raw), `"`)
	}
	return s
}

// RenderLog reads a job log file and writes human-friendly output.
// JSONL lines are processed through Formatter for compact tool/text
// rendering. Non-JSON lines are printed as-is.
func RenderLog(r io.Reader, w io.Writer, isTTY bool) error {
	return RenderLogWith(r, New(w, isTTY), w)
}

// RenderLogWith renders a job log using a pre-configured Formatter.
// plainW receives non-JSON lines directly.
func RenderLogWith(
	r io.Reader, fmtr *Formatter, plainW io.Writer,
) error {
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadString('\n')
		// ReadString returns data even on error (e.g. EOF
		// without trailing newline), so process before checking.
		line = strings.TrimRight(line, "\n\r")
		if line != "" {
			if LooksLikeJSON(line) {
				if _, werr := fmtr.Write(
					[]byte(line + "\n"),
				); werr != nil {
					return werr
				}
			} else {
				// Non-JSON lines: sanitize ANSI/control sequences
				// to prevent terminal spoofing from agent stderr,
				// then word-wrap to the formatter's width.
				line = SanitizeControlKeepNewlines(line)
				if w := fmtr.Width(); w > 0 {
					for _, wrapped := range WrapText(line, w) {
						if _, werr := fmt.Fprintln(plainW, wrapped); werr != nil {
							return werr
						}
					}
				} else {
					if _, werr := fmt.Fprintln(plainW, line); werr != nil {
						return werr
					}
				}

			}
		} else if err != io.EOF {
			// Preserve blank lines for spacing in rendered output.
			if _, werr := fmt.Fprintln(plainW); werr != nil {
				return werr
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}

	fmtr.Flush()
	return nil
}

// LooksLikeJSON returns true if line is a JSON object with a
// non-empty "type" field, matching the stream event format used
// by Claude Code, Codex, and Gemini CLI.
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
