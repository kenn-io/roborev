package tui

import (
	"regexp"
	"strings"
	"time"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"charm.land/glamour/v2"
	gansi "charm.land/glamour/v2/ansi"
	xansi "github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-runewidth"
	"github.com/muesli/termenv"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/streamfmt"
)

// Filter type constants used in filterStack and popFilter/pushFilter.
const (
	filterTypeRepo   = "repo"
	filterTypeBranch = "branch"
)

// branchNone is the sentinel value for jobs with no branch information.
const branchNone = "(none)"

// Some embedded terminals forward Enter as LF (ctrl+j) instead of CR.
// Only use this in non-text-entry flows where ctrl+j is not meaningful input.
func isSubmitKey(msg tea.KeyMsg) bool {
	key := msg.Key()
	switch key.Code {
	case tea.KeyEnter:
		return true
	}
	if key.Mod.Contains(tea.ModCtrl) && key.Code == 'j' {
		return true
	}
	return key.Text == "\r" || key.Text == "\n"
}

func keyRunes(msg tea.KeyMsg) []rune {
	key := msg.Key()
	if key.Text != "" {
		return []rune(key.Text)
	}
	if key.Code == tea.KeySpace {
		return []rune{' '}
	}
	if key.Mod != 0 {
		return nil
	}
	if key.Code >= 0 && key.Code <= unicode.MaxRune && unicode.IsPrint(key.Code) {
		return []rune{key.Code}
	}
	return nil
}

// setFlash sets a flash message with the given duration and view.
func (m *model) setFlash(msg string, d time.Duration, v viewKind) {
	m.flashMessage = msg
	m.flashExpiresAt = time.Now().Add(d)
	m.flashView = v
	m.flashWarning = false
}

func (m *model) setWarningFlash(msg string, d time.Duration, v viewKind) {
	m.flashMessage = msg
	m.flashExpiresAt = time.Now().Add(d)
	m.flashView = v
	m.flashWarning = true
}

func (m model) renderDaemonStatus() string {
	daemonVersion := m.daemonVersion
	if daemonVersion == "" {
		daemonVersion = "?"
	}

	line := statusStyle.Render("Daemon: " + daemonVersion)
	if m.versionMismatch {
		line += " " + errorStyle.Render("[MISMATCH]")
	}
	return line
}

func (m model) renderFlash(view viewKind) string {
	if m.flashMessage == "" || !time.Now().Before(m.flashExpiresAt) || m.flashView != view {
		return ""
	}
	if m.flashWarning {
		return warningFlashStyle.Render(m.flashMessage)
	}
	return flashStyle.Render(m.flashMessage)
}

// selectedJob returns the currently selected job (a panel parent / standalone
// job in m.jobs, or a side-fetched panel member), resolved by selectedJobID.
// selectedJobID is the single selection identity; selectedIdx is a render hint
// only. Returns nil,false when nothing is selected or the id is not loaded.
func (m *model) selectedJob() (*storage.ReviewJob, bool) {
	if m.selectedJobID == 0 {
		return nil, false
	}
	for i := range m.jobs {
		if m.jobs[i].ID == m.selectedJobID {
			return &m.jobs[i], true
		}
	}
	for _, members := range m.panelMembers {
		for i := range members {
			if members[i].ID == m.selectedJobID {
				return &members[i], true
			}
		}
	}
	return nil, false
}

// selectedIsMember reports whether selectedJobID names a side-fetched panel
// member (present in panelMembers, absent from m.jobs). Used to keep a member
// selected across a parents-only refresh, since the member is not in m.jobs.
func (m *model) selectedIsMember() bool {
	if m.selectedJobID == 0 {
		return false
	}
	for i := range m.jobs {
		if m.jobs[i].ID == m.selectedJobID {
			return false
		}
	}
	for _, members := range m.panelMembers {
		for i := range members {
			if members[i].ID == m.selectedJobID {
				return true
			}
		}
	}
	return false
}

// selectJobByID restores selection to a specific job if it is still loaded.
func (m *model) selectJobByID(jobID int64) bool {
	for i := range m.jobs {
		if m.jobs[i].ID == jobID {
			m.selectedIdx = i
			m.selectedJobID = jobID
			return true
		}
	}
	return false
}

// mutateJob finds a job by ID and applies the mutation function.
// Returns true if the job was found and mutated.
func (m *model) mutateJob(id int64, fn func(*storage.ReviewJob)) bool {
	for i := range m.jobs {
		if m.jobs[i].ID == id {
			fn(&m.jobs[i])
			return true
		}
	}
	return false
}

// applyStatsDelta adjusts jobStats for a closed state change.
// closed=true means marking as closed (+Closed, -Open).
func (m *model) applyStatsDelta(closed bool) {
	if closed {
		m.jobStats.Closed++
		m.jobStats.Open--
	} else {
		m.jobStats.Closed--
		m.jobStats.Open++
	}
}

// setJobClosed updates the closed state for a job by ID.
// Handles nil pointer by allocating if necessary.
func (m *model) setJobClosed(jobID int64, state bool) {
	m.mutateJob(jobID, func(job *storage.ReviewJob) {
		if job.Closed == nil {
			job.Closed = new(bool)
		}
		*job.Closed = state
	})
}

// setJobStatus updates the status for a job by ID.
func (m *model) setJobStatus(jobID int64, status storage.JobStatus) {
	m.mutateJob(jobID, func(job *storage.ReviewJob) {
		job.Status = status
	})
}

// setJobFinishedAt updates the FinishedAt for a job by ID.
func (m *model) setJobFinishedAt(jobID int64, finishedAt *time.Time) {
	m.mutateJob(jobID, func(job *storage.ReviewJob) {
		job.FinishedAt = finishedAt
	})
}

// setJobStartedAt updates the StartedAt for a job by ID.
func (m *model) setJobStartedAt(jobID int64, startedAt *time.Time) {
	m.mutateJob(jobID, func(job *storage.ReviewJob) {
		job.StartedAt = startedAt
	})
}

// setJobError updates the Error for a job by ID.
func (m *model) setJobError(jobID int64, errMsg string) {
	m.mutateJob(jobID, func(job *storage.ReviewJob) {
		job.Error = errMsg
	})
}

// wrapText wraps text to the specified width, preserving existing line breaks
// and breaking at word boundaries when possible. Uses runewidth for correct
// display width calculation with Unicode and wide characters.
func wrapText(text string, width int) []string {
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

			// Walk runes up to the width limit
			for i, r := range runes {
				rw := runewidth.RuneWidth(r)
				if currentWidth+rw > width {
					break
				}
				currentWidth += rw
				breakPoint = i + 1
			}

			// Ensure forward progress: if the first rune is wider than width,
			// take at least one rune to avoid an infinite loop.
			if breakPoint == 0 {
				breakPoint = 1
			}

			// Try to find a space to break at (look back from breakPoint)
			bestBreak := breakPoint
			scanWidth := 0
			for i := breakPoint - 1; i >= 0; i-- {
				if runes[i] == ' ' {
					bestBreak = i
					break
				}
				scanWidth += runewidth.RuneWidth(runes[i])
				if scanWidth > width/2 {
					break // Don't look back too far
				}
			}

			result = append(result, string(runes[:bestBreak]))
			line = strings.TrimLeft(string(runes[bestBreak:]), " ")
		}
		if len(line) > 0 {
			result = append(result, line)
		}
	}

	return result
}

// wrapLine wraps a single line (no embedded newlines) to the given
// display width, breaking at word boundaries when possible. Returns
// at least one element even for empty input.
func wrapLine(line string, width int) []string {
	if width <= 0 {
		width = 100
	}
	if runewidth.StringWidth(line) <= width {
		return []string{line}
	}
	var result []string
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
		if breakPoint == 0 {
			breakPoint = 1
		}
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
		// Guarantee forward progress: if the backward scan landed
		// at index 0 (e.g. space at position 0, very narrow width),
		// fall back to breakPoint which is always >= 1.
		if bestBreak == 0 {
			bestBreak = breakPoint
		}
		result = append(result, string(runes[:bestBreak]))
		// Skip the break-point space only when it is a true
		// word separator (non-space on both sides). This preserves
		// whitespace-only lines and indentation runs verbatim.
		isWordSep := bestBreak < len(runes) &&
			runes[bestBreak] == ' ' &&
			bestBreak > 0 && runes[bestBreak-1] != ' ' &&
			bestBreak+1 < len(runes) && runes[bestBreak+1] != ' '
		if isWordSep {
			line = string(runes[bestBreak+1:])
		} else {
			line = string(runes[bestBreak:])
		}
	}
	if len(line) > 0 {
		result = append(result, line)
	}
	if len(result) == 0 {
		return []string{""}
	}
	return result
}

// markdownCache caches glamour-rendered lines for review and prompt views.
// Stored as a pointer in model so that View() (value receiver) can update
// the cache and have it persist across bubbletea's model copies.
//
// glamourStyle is detected once at creation time (before bubbletea takes over
// the terminal) to avoid calling termenv.HasDarkBackground() on every render,
// which blocks for seconds inside bubbletea's raw-mode input loop.
type markdownCache struct {
	glamourStyle gansi.StyleConfig // custom style derived from dark/light, detected once at init
	colorProfile termenv.Profile   // color profile for glamour rendering (Ascii when NO_COLOR)
	tabWidth     int               // tab expansion width (default 2)

	reviewLines []string
	reviewID    int64
	reviewWidth int
	reviewText  string // raw input text used to produce reviewLines

	promptLines []string
	promptID    int64
	promptWidth int
	promptText  string // raw input text used to produce promptLines

	// Max scroll positions computed during the last render.
	// Stored here (in the shared pointer) so key handlers can clamp
	// scroll values even though View() uses a value receiver.
	lastReviewMaxScroll int
	lastPromptMaxScroll int
}

// newMarkdownCache creates a markdownCache, detecting terminal background
// color now (before bubbletea enters raw mode and takes over stdin).
// Delegates style and color profile resolution to the streamfmt package,
// which respects ROBOREV_COLOR_MODE env var and NO_COLOR convention.
func newMarkdownCache(tabWidth int) *markdownCache {
	style := streamfmt.GlamourStyle()
	profile := streamfmt.ResolveColorProfile()
	if tabWidth <= 0 {
		tabWidth = 2
	} else if tabWidth > 16 {
		tabWidth = 16
	}
	return &markdownCache{glamourStyle: style, colorProfile: profile, tabWidth: tabWidth}
}

// truncateLongLines normalizes tabs and truncates lines inside fenced code
// blocks that exceed maxWidth, so glamour won't word-wrap them. Prose lines
// outside code blocks are left intact for glamour to word-wrap naturally.
//
// Fence detection follows CommonMark rules: 0-3 spaces of indentation
// followed by 3+ backticks or 3+ tildes. A closing fence must use the
// same character and be at least as long as the opening fence.
func truncateLongLines(text string, maxWidth int, tabWidth int) string {
	if maxWidth <= 0 {
		return text
	}
	if tabWidth <= 0 {
		tabWidth = 2
	} else if tabWidth > 16 {
		tabWidth = 16
	}
	// Expand tabs to spaces. runewidth counts tabs as width 0 but
	// terminals expand them to up to 8 columns, causing width mismatch.
	text = strings.ReplaceAll(text, "\t", strings.Repeat(" ", tabWidth))
	lines := strings.Split(text, "\n")
	var fenceChar byte // '`' or '~' for the opening fence
	var fenceLen int   // length of the opening fence run
	for i, line := range lines {
		if fenceLen == 0 {
			// Not inside a code block — check for opening fence.
			if ch, n, _ := parseFence(line); n > 0 {
				fenceChar = ch
				fenceLen = n
			}
			continue
		}
		// Inside a code block — check for closing fence.
		// Closing fences must have whitespace-only trailing content.
		if ch, n, wsOnly := parseFence(line); n >= fenceLen && ch == fenceChar && wsOnly {
			fenceLen = 0
			continue
		}
		if runewidth.StringWidth(line) > maxWidth {
			lines[i] = runewidth.Truncate(line, maxWidth, "")
		}
	}
	return strings.Join(lines, "\n")
}

// parseFence checks if line is a CommonMark fenced code block delimiter.
// Returns the fence character ('`' or '~'), the run length, and whether
// trailing content is whitespace-only (required for closing fences).
// Returns (0, 0, false) if the line is not a valid fence.
// Allows 0-3 leading spaces.
func parseFence(line string) (byte, int, bool) {
	// Strip up to 3 leading spaces.
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
	// Backtick fences can't have backticks in the info string.
	if ch == '`' && strings.ContainsRune(trailing, '`') {
		return 0, 0, false
	}
	return ch, n, wsOnly
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
// ESC [ <parameter bytes 0x30-0x3F>* <intermediate bytes 0x20-0x2F>* <final byte 0x40-0x7E>
var csiRe = regexp.MustCompile(`\x1b\[[\x30-\x3f]*[\x20-\x2f]*[\x40-\x7e]`)

// sgrRe matches SGR (Select Graphic Rendition) sequences specifically:
// ESC [ <digits and semicolons only> m
var sgrRe = regexp.MustCompile(`^\x1b\[[0-9;]*m$`)

// sanitizeEscapes strips non-SGR terminal escape sequences and dangerous C0
// control characters from a line, preventing untrusted content from injecting
// OSC/CSI/DCS control codes or spoofing output via \r/\b overwrites.
// SGR sequences (colors/styles), tabs, and newlines are preserved.
func sanitizeEscapes(line string) string {
	line = nonCSIEscRe.ReplaceAllString(line, "")
	line = csiRe.ReplaceAllStringFunc(line, func(seq string) string {
		if sgrRe.MatchString(seq) {
			return seq
		}
		return ""
	})
	// Strip C0 control characters (0x00-0x1F) except tab (0x09), newline
	// (0x0A), and ESC (0x1B, already handled above). Characters like \r
	// and \b can overwrite displayed content, enabling output spoofing.
	var b strings.Builder
	for _, r := range line {
		if r < 0x20 && r != '\t' && r != '\n' && r != 0x1b {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// sanitizeLines applies sanitizeEscapes to every line in the slice.
func sanitizeLines(lines []string) []string {
	for i, line := range lines {
		lines[i] = sanitizeEscapes(line)
	}
	return lines
}

// trailingPadRe matches trailing whitespace and ANSI SGR sequences.
// Glamour pads code block lines with spaces (using background color) to fill
// the wrap width. Stripping this padding prevents overflow on narrow terminals.
var trailingPadRe = regexp.MustCompile(`(\s|\x1b\[[0-9;]*m)+$`)

// allSGRRe matches any ANSI SGR escape sequence.
var allSGRRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// stripTrailingPadding removes trailing whitespace and ANSI SGR codes from a
// glamour output line, then appends a reset to ensure clean color state.
// When noColor is true, all SGR sequences are stripped to prevent open
// attributes (bold, underline) from bleeding across lines.
func stripTrailingPadding(line string, noColor bool) string {
	line = trailingPadRe.ReplaceAllString(line, "")
	if noColor {
		return allSGRRe.ReplaceAllString(line, "")
	}
	return line + "\x1b[0m"
}

// renderMarkdownLines renders markdown text using glamour and splits into lines.
// wrapWidth controls glamour's word-wrap column (capped for readability).
// maxWidth controls line truncation (actual terminal width).
// colorProfile controls post-render color stripping.
// Falls back to wrapText if glamour rendering fails.
func renderMarkdownLines(text string, wrapWidth, maxWidth int, glamourStyle gansi.StyleConfig, tabWidth int, colorProfile termenv.Profile) []string {
	// Truncate long lines before glamour so they don't get word-wrapped.
	// Use maxWidth (terminal width) so content fills the available space.
	text = truncateLongLines(text, maxWidth, tabWidth)
	r, err := glamour.NewTermRenderer(
		glamour.WithStyles(glamourStyle),
		glamour.WithWordWrap(wrapWidth),
		glamour.WithPreservedNewLines(),
	)
	if err != nil {
		return sanitizeLines(wrapText(text, wrapWidth))
	}
	out, err := r.Render(text)
	if err != nil {
		return sanitizeLines(wrapText(text, wrapWidth))
	}
	noColor := colorProfile == termenv.Ascii
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	for i, line := range lines {
		line = stripTrailingPadding(line, noColor)
		line = sanitizeEscapes(line)
		// Truncate output lines that still exceed maxWidth (glamour can add
		// indentation for block quotes, lists, etc. beyond the wrap width).
		if xansi.StringWidth(line) > maxWidth {
			line = xansi.Truncate(line, maxWidth, "")
		}
		lines[i] = line
	}
	return lines
}

// getReviewLines returns glamour-rendered lines for a review, using the cache
// when the inputs (review ID, width, text) haven't changed.
// wrapWidth is the glamour word-wrap column; maxWidth is the truncation limit.
func (c *markdownCache) getReviewLines(text string, wrapWidth, maxWidth int, reviewID int64) []string {
	if c.reviewID == reviewID && c.reviewWidth == maxWidth && c.reviewText == text {
		return c.reviewLines
	}
	c.reviewLines = renderMarkdownLines(text, wrapWidth, maxWidth, c.glamourStyle, c.tabWidth, c.colorProfile)
	c.reviewID = reviewID
	c.reviewWidth = maxWidth
	c.reviewText = text
	return c.reviewLines
}

// getPromptLines returns glamour-rendered lines for a prompt, using the cache
// when the inputs (review ID, width, text) haven't changed.
// wrapWidth is the glamour word-wrap column; maxWidth is the truncation limit.
func (c *markdownCache) getPromptLines(text string, wrapWidth, maxWidth int, reviewID int64) []string {
	if c.promptID == reviewID && c.promptWidth == maxWidth && c.promptText == text {
		return c.promptLines
	}
	c.promptLines = renderMarkdownLines(text, wrapWidth, maxWidth, c.glamourStyle, c.tabWidth, c.colorProfile)
	c.promptID = reviewID
	c.promptWidth = maxWidth
	c.promptText = text
	return c.promptLines
}
