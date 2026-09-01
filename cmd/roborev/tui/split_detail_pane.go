package tui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
	xansi "github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-runewidth"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/tokens"
)

// reviewFixPanelPaneReserve is the number of pane rows the inline fix panel
// occupies when open (label + top border + input line + bottom border +
// help line), mirroring render_review.go's panelReserve so scroll math
// (renderReviewPaneBody / reviewPaneScrollInfo) never drifts from what's
// actually rendered.
const reviewFixPanelPaneReserve = 5

// reviewPaneHeaderLines returns the compact header lines rendered above the
// markdown body in the split detail pane: title, an optional verdict/closed/
// tokens line, and a trailing blank separator. renderReviewPaneBody and
// reviewPaneScrollInfo both call this so their header-height accounting can
// never drift apart.
func (m model) reviewPaneHeaderLines(innerW int) []string {
	review := m.currentReview
	if review == nil || review.Job == nil {
		return nil
	}
	var out []string
	title := fmt.Sprintf("Review #%d %s (%s)", review.Job.ID,
		m.getDisplayName(review.Job.RepoPath, review.Job.RepoName),
		formatAgentLabel(review.Agent, review.Job.Model))
	out = append(out, xansi.Truncate(titleStyle.Render(title), innerW, ""))

	verdictParts := []string{
		statusStyle.Render("Review type: " + displayReviewType(review.Job.ReviewType, review.Job.PanelRole)),
	}
	if review.Job.Verdict != nil && *review.Job.Verdict != "" && !review.Job.IsFixJob() {
		if *review.Job.Verdict == "P" {
			verdictParts = append(verdictParts, passStyle.Render("Verdict: Pass"))
		} else {
			verdictParts = append(verdictParts, failStyle.Render("Verdict: Fail"))
		}
	}
	if review.Closed {
		verdictParts = append(verdictParts, closedStyle.Render("[CLOSED]"))
	}
	if tu := tokens.ParseJSON(review.Job.TokenUsage); tu != nil {
		verdictParts = append(verdictParts, statusStyle.Render("["+tu.FormatSummary()+"]"))
	}
	if len(verdictParts) > 0 {
		out = append(out, xansi.Truncate(strings.Join(verdictParts, " "), innerW, ""))
	}
	out = append(out, "")
	return out
}

// reviewPaneBodyLines returns the markdown-rendered body lines for the
// current review, windowed to wrapWidth/maxWidth derived from innerW.
// Shared by renderReviewPaneBody and reviewPaneScrollInfo so both compute
// the exact same line set.
func (m model) reviewPaneBodyLines(innerW int) []string {
	review := m.currentReview
	maxWidth := max(20, innerW)
	wrapWidth := min(maxWidth, 100)
	if m.mdCache != nil {
		return m.mdCache.getReviewLines(m.reviewContentString(review), wrapWidth, maxWidth, review.ID)
	}
	return sanitizeLines(wrapText(m.reviewContentString(review), wrapWidth))
}

// renderReviewPaneBody renders the current review body-only for the split
// detail pane: exactly innerH clean lines. When m.reviewFixPanelOpen, the
// inline fix panel (adapted from render_review.go's full-screen block) is
// reserved out of the body budget and appended after it, so the pane never
// silently swallows keystrokes without showing where they're going -- see
// handleKeyMsg's reviewFixPanelFocused capture at the top of the key
// dispatch chain.
func (m model) renderReviewPaneBody(innerW, innerH int) []string {
	panelReserve := 0
	if m.reviewFixPanelOpen {
		panelReserve = reviewFixPanelPaneReserve
	}
	bodyH := max(innerH-panelReserve, 1)

	out := append([]string{}, m.reviewPaneHeaderLines(innerW)...)

	// Markdown body, windowed by m.reviewScroll.
	lines := m.reviewPaneBodyLines(innerW)
	visible := max(bodyH-len(out), 1)
	maxScroll := max(len(lines)-visible, 0)
	if m.mdCache != nil {
		m.mdCache.lastReviewMaxScroll = maxScroll
	}
	start := max(min(m.reviewScroll, maxScroll), 0)
	for i := start; i < min(start+visible, len(lines)); i++ {
		out = append(out, xansi.Truncate(lines[i], innerW, ""))
	}
	for len(out) < bodyH {
		out = append(out, "")
	}
	out = out[:bodyH]

	if m.reviewFixPanelOpen {
		out = append(out, m.renderReviewFixPanelPaneLines(innerW)...)
	}
	for len(out) < innerH {
		out = append(out, "")
	}
	return out[:innerH]
}

// renderReviewFixPanelPaneLines renders the inline fix panel adapted for
// the split detail pane at innerW, producing exactly
// reviewFixPanelPaneReserve lines. Mirrors render_review.go's full-screen
// fix panel block (focused: label + bordered input + help; unfocused:
// dimmed label + gray box + hint) so the two stay visually consistent; kept
// separate rather than shared because the two write to different targets
// (a line slice here vs. a strings.Builder with "\x1b[K" trailers there).
func (m model) renderReviewFixPanelPaneLines(innerW int) []string {
	boxW := max(innerW-2, 10) // box inner width; total visual width = boxW+2 (borders)
	trunc := func(s string) string { return xansi.Truncate(s, innerW, "") }

	var out []string
	if m.reviewFixPanelFocused {
		label := "Fix: enter instructions (or leave blank for default)"
		if runewidth.StringWidth(label) > innerW {
			label = runewidth.Truncate(label, innerW, "")
		}
		out = append(out, label)

		inputDisplay := m.fixPromptText
		// lipgloss v2's Width is border-box: content wider than boxW-2
		// wraps the box past its 3-line budget, and the 5-line pane cap
		// then silently drops the help line. " > " (3) + "_" (1) = 4
		// overhead, so the input itself may use boxW-6.
		maxInputLen := boxW - 6
		if runewidth.StringWidth(inputDisplay) > maxInputLen {
			runes := []rune(inputDisplay)
			for runewidth.StringWidth(string(runes)) > maxInputLen {
				runes = runes[1:]
			}
			inputDisplay = string(runes)
		}
		content := fmt.Sprintf(" > %s_", inputDisplay)
		boxStyle := lipgloss.NewStyle().
			Border(lipgloss.NormalBorder()).
			BorderForeground(adaptiveColor("125", "205")). // magenta/pink (active)
			Width(boxW)
		for line := range strings.SplitSeq(strings.TrimRight(boxStyle.Render(content), "\n"), "\n") {
			out = append(out, trunc(line))
		}
		out = append(out, trunc(helpStyle.Render("tab: scroll review | enter: submit | esc: cancel")))
	} else {
		out = append(out, trunc(statusStyle.Render("Fix (Tab to focus)")))

		inputDisplay := m.fixPromptText
		if inputDisplay == "" {
			inputDisplay = "(blank = default)"
		}
		// Border-box again: " " (1) + display must fit in boxW-2.
		if runewidth.StringWidth(inputDisplay) > boxW-3 {
			inputDisplay = runewidth.Truncate(inputDisplay, boxW-3, "")
		}
		content := " " + inputDisplay
		boxStyle := lipgloss.NewStyle().
			Border(lipgloss.NormalBorder()).
			BorderForeground(adaptiveColor("242", "246")). // gray (inactive)
			Foreground(adaptiveColor("242", "246")).
			Width(boxW)
		for line := range strings.SplitSeq(strings.TrimRight(boxStyle.Render(content), "\n"), "\n") {
			out = append(out, trunc(statusStyle.Render(line)))
		}
		out = append(out, trunc(helpStyle.Render("F: fix | tab: focus fix panel")))
	}

	for len(out) < reviewFixPanelPaneReserve {
		out = append(out, "")
	}
	return out[:reviewFixPanelPaneReserve]
}

// reviewPaneScrollInfo reports the visible body window for the info line.
func (m model) reviewPaneScrollInfo(innerW, innerH int) (start, end, total int) {
	review := m.currentReview
	if review == nil {
		return 0, 0, 0
	}
	headerLines := len(m.reviewPaneHeaderLines(innerW))
	panelReserve := 0
	if m.reviewFixPanelOpen {
		panelReserve = reviewFixPanelPaneReserve
	}
	lines := m.reviewPaneBodyLines(innerW)
	visible := max(innerH-headerLines-panelReserve, 1)
	total = len(lines)
	start = max(min(m.reviewScroll, max(total-visible, 0)), 0)
	end = min(start+visible, total)
	return start, end, total
}

// renderDetailPane picks the detail pane content for the highlighted job:
// done -> review (or loading placeholder until the follow fetch lands),
// failed -> error text, running -> status card (+ live log),
// queued/canceled -> status card.
func (m model) renderDetailPane(innerW, innerH int) []string {
	pad := func(lines []string) []string {
		for len(lines) < innerH {
			lines = append(lines, "")
		}
		return lines[:innerH]
	}
	job, ok := m.selectedJob()
	if !ok {
		// selectedReviewLoaded's anchored fallback: hide-closed can prune
		// a just-closed job's row from the refresh while its review is
		// still the anchored, displayed content -- render it from the
		// loaded review rather than vanishing into "No job selected".
		if m.selectedReviewLoaded() {
			return m.renderReviewPaneBody(innerW, innerH)
		}
		return pad([]string{statusStyle.Render("No job selected")})
	}
	switch job.Status {
	case storage.JobStatusDone:
		// selectedReviewLoaded, not a bare JobID match: job IDs are reused
		// across reruns, so a matching ID can still name a PREVIOUS
		// attempt's review while reconcile refetches the current one.
		// Rendering it would display obsolete output -- indefinitely, if
		// the refetch fails, since the review body would also mask the
		// splitDetailErr card below -- and would show content whose
		// actions the staleness guards refuse (tab silently dead). Falling
		// through shows the loading/error state until the fresh review is
		// accepted, exactly like a not-yet-loaded review.
		if m.selectedReviewLoaded() {
			return m.renderReviewPaneBody(innerW, innerH)
		}
		if m.splitDetailErr != nil {
			card := m.renderJobStatusCard(*job, innerW)
			card = append(card, "",
				errorStyle.Render(xansi.Truncate("Failed to load review: "+m.splitDetailErr.Error(), innerW, "")),
				statusStyle.Render("(re-select the row to retry)"))
			return pad(card)
		}
		return pad(append(m.renderJobStatusCard(*job, innerW),
			"", statusStyle.Render("Loading review...")))
	case storage.JobStatusFailed:
		// Same attempt-freshness gate as the Done branch above. A stale
		// synthesized review is a one-refresh transient here (reconcile
		// rebuilds synchronously), and the fallback below renders the same
		// job.Error text, so the gate costs nothing and keeps the
		// display's condition identical to the action guards'.
		if m.selectedReviewLoaded() {
			return m.renderReviewPaneBody(innerW, innerH)
		}
		card := m.renderJobStatusCard(*job, innerW)
		card = append(card, "")
		// job.Error is untrusted agent/process output rendered directly
		// (unlike the done-job path above, which routes through the
		// synthesized review and its glamour/sanitizeEscapes pipeline once
		// the follow-fetch lands) -- sanitize it here per the repo's
		// sanitizeForDisplay convention for multi-line untrusted content,
		// so it can't inject terminal escapes (cursor moves, OSC clipboard
		// writes, \r/\b overwrites) while this direct render is showing.
		for _, l := range wrapText("Job failed:\n\n"+sanitizeForDisplay(job.Error), max(innerW-2, 20)) {
			card = append(card, xansi.Truncate(failStyle.Render(l), innerW, ""))
		}
		return pad(card)
	case storage.JobStatusRunning:
		card := m.renderJobStatusCard(*job, innerW)
		if m.splitDetailErr != nil {
			card = append(card, "",
				errorStyle.Render(xansi.Truncate("Failed to load log: "+m.splitDetailErr.Error(), innerW, "")))
			return pad(card)
		}
		divider := xansi.Truncate(statusStyle.Render(strings.Repeat("─", max(innerW/3, 8))+" live log"), innerW, "")
		card = append(card, "", divider)
		return pad(m.appendPaneLogTail(card, innerW, innerH))
	default:
		return pad(m.renderJobStatusCard(*job, innerW))
	}
}

// appendPaneLogTail appends the tail of the live log buffer to
// lines, filling the space remaining below the status card/divider (one row
// held back as a bottom margin), each entry truncated to innerW.
func (m model) appendPaneLogTail(lines []string, innerW, innerH int) []string {
	avail := innerH - len(lines) - 1
	if avail <= 0 {
		return lines
	}
	total := len(m.paneLogLines)
	start := max(total-avail, 0)
	for i := start; i < total; i++ {
		lines = append(lines, xansi.Truncate(m.paneLogLines[i].text, innerW, ""))
	}
	return lines
}

// renderJobStatusCard renders job metadata for non-review pane states.
func (m model) renderJobStatusCard(job storage.ReviewJob, innerW int) []string {
	trunc := func(s string) string { return xansi.Truncate(s, innerW, "") }
	title := fmt.Sprintf("Job #%d · %s", job.ID, job.Status)
	card := []string{trunc(titleStyle.Render(title)), ""}
	add := func(label, val string) {
		if val != "" {
			card = append(card, trunc(statusStyle.Render(label+": ")+val))
		}
	}
	add("Repo", m.getDisplayName(job.RepoPath, job.RepoName))
	add("Ref", shortJobRef(job))
	add("Branch", m.getBranchForJob(job))
	add("Agent", formatAgentLabel(job.Agent, job.Model))
	add("Review type", displayReviewType(job.ReviewType, job.PanelRole))
	if job.StartedAt != nil {
		add("Elapsed", m.jobElapsedCell(job))
	} else if !job.EnqueuedAt.IsZero() {
		add("Queued", job.EnqueuedAt.Local().Format("Jan 02 15:04"))
	}
	return card
}
