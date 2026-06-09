package tui

import (
	"fmt"
	"path/filepath"
	"strings"

	"charm.land/lipgloss/v2"
	xansi "github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-runewidth"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/tokens"
)

// panelReviewHeader summarizes a panel run for the review-detail header. With
// members loaded: "3 reviewers: default P, security F, design P". Without them
// (parent opened before its members were fetched): a PanelSummary fallback,
// "3 reviewers: 2 ok · 1 failed", so the header is never dropped.
func panelReviewHeader(job storage.ReviewJob, members []storage.ReviewJob) string {
	if len(members) > 0 {
		parts := make([]string, 0, len(members))
		for _, mem := range members {
			v := "·"
			if mem.Verdict != nil && *mem.Verdict != "" {
				v = *mem.Verdict
			}
			parts = append(parts, fmt.Sprintf("%s %s", mem.PanelMemberName, v))
		}
		return fmt.Sprintf("%d reviewers: %s", len(members), strings.Join(parts, ", "))
	}
	s := job.PanelSummary
	if s == nil {
		return ""
	}
	return fmt.Sprintf("%d reviewers: %s", s.MembersTotal, panelOutcomeSplit(s))
}

func (m model) renderReviewView() string {
	var b strings.Builder

	review := m.currentReview

	// Build title string and compute its length for line calculation
	var title string
	var titleLen int
	var locationLineLen int
	if review.Job != nil {
		ref := shortJobRef(*review.Job)
		idStr := fmt.Sprintf("#%d ", review.Job.ID)
		// Use cached display name, falling back to RepoName, then basename of RepoPath
		defaultName := review.Job.RepoName
		if defaultName == "" && review.Job.RepoPath != "" {
			defaultName = filepath.Base(review.Job.RepoPath)
		}
		repoStr := m.getDisplayName(review.Job.RepoPath, defaultName)

		agentStr := formatAgentLabel(review.Agent, review.Job.Model)

		title = fmt.Sprintf("Review %s%s (%s)", idStr, repoStr, agentStr)
		titleLen = runewidth.StringWidth(title)

		b.WriteString(titleStyle.Render(title))
		b.WriteString("\x1b[K") // Clear to end of line

		// Show location line: repo path (or identity/name), git ref, and branch
		b.WriteString("\n")
		locationLine := review.Job.RepoPath
		if locationLine == "" {
			// No local path - use repo name/identity as fallback
			locationLine = review.Job.RepoName
		}
		if locationLine != "" {
			locationLine += " " + ref
		} else {
			locationLine = ref
		}
		if m.currentBranch != "" {
			locationLine += " on " + m.currentBranch
		}
		locationLineLen = runewidth.StringWidth(locationLine)
		b.WriteString(statusStyle.Render(locationLine))
		b.WriteString("\x1b[K") // Clear to end of line

		// Show verdict, closed status, and token usage on next line (skip verdict for fix jobs)
		hasVerdict := review.Job.Verdict != nil && *review.Job.Verdict != "" && !review.Job.IsFixJob()
		tokenSummary := ""
		if tu := tokens.ParseJSON(review.Job.TokenUsage); tu != nil {
			tokenSummary = tu.FormatSummary()
		}
		if hasVerdict || review.Closed || tokenSummary != "" {
			b.WriteString("\n")
			if hasVerdict {
				v := *review.Job.Verdict
				if v == "P" {
					b.WriteString(passStyle.Render("Verdict: Pass"))
				} else {
					b.WriteString(failStyle.Render("Verdict: Fail"))
				}
			}
			// Show [CLOSED] with distinct color (after verdict if present)
			if review.Closed {
				if hasVerdict {
					b.WriteString(" ")
				}
				b.WriteString(closedStyle.Render("[CLOSED]"))
			}
			if tokenSummary != "" {
				if hasVerdict || review.Closed {
					b.WriteString(" ")
				}
				b.WriteString(statusStyle.Render("[" + tokenSummary + "]"))
			}
			b.WriteString("\x1b[K") // Clear to end of line
		}
		b.WriteString("\n")
	} else {
		title = "Review"
		titleLen = len(title)
		b.WriteString(titleStyle.Render(title))
		b.WriteString("\x1b[K\n") // Clear to end of line
	}

	// Build content: panel header (synthesis parent only) + review output + responses
	var content strings.Builder
	if review.Job != nil && review.Job.IsSynthesisJob() {
		if header := panelReviewHeader(*review.Job, m.panelMembers[review.Job.PanelRunUUID]); header != "" {
			content.WriteString(sanitizeForDisplay(header))
			content.WriteString("\n\n")
		}
	}
	content.WriteString(review.Output)

	// Append responses if any
	if len(m.currentResponses) > 0 {
		content.WriteString("\n\n--- Comments ---\n")
		for _, r := range m.currentResponses {
			timestamp := r.CreatedAt.Format("Jan 02 15:04")
			fmt.Fprintf(&content, "\n[%s] %s:\n", timestamp, r.Responder)
			content.WriteString(r.Response)
			content.WriteString("\n")
		}
	}

	// Render markdown content with glamour (cached), falling back to plain text wrapping.
	// wrapWidth caps at 100 for readability; maxWidth uses actual terminal width for truncation.
	maxWidth := max(20, m.width-4)
	wrapWidth := min(maxWidth, 100)
	contentStr := content.String()
	var lines []string
	if m.mdCache != nil {
		lines = m.mdCache.getReviewLines(contentStr, wrapWidth, maxWidth, review.ID)
	} else {
		lines = sanitizeLines(wrapText(contentStr, wrapWidth))
	}

	// Compute title line count based on actual title length
	titleLines := 1
	if m.width > 0 && titleLen > m.width {
		titleLines = (titleLen + m.width - 1) / m.width
	}

	// Help table rows
	reviewHelpRows := [][]helpItem{
		{{"p", "prompt"}, {"c", "comment"}, {"m", "commit"}, {"a", "close"}, {"y", "copy"}},
		{{"↑/↓", "scroll"}, {"←/→", "prev/next"}, {"?", "commands"}, {"esc", "back"}},
	}
	if m.tasksWorkflowEnabled() {
		reviewHelpRows[0] = append(reviewHelpRows[0], helpItem{"F", "fix"})
	}
	helpLines := len(reflowHelpRows(reviewHelpRows, m.width))

	// Compute location line count (repo path + ref + branch can wrap)
	locationLines := 0
	if review.Job != nil {
		locationLines = 1
		if m.width > 0 && locationLineLen > m.width {
			locationLines = (locationLineLen + m.width - 1) / m.width
		}
	}

	// Reserve title, location, footer status, help, and optional verdict.
	headerHeight := titleLines + locationLines + 1 + helpLines
	hasVerdict := review.Job != nil && review.Job.Verdict != nil && *review.Job.Verdict != "" && !review.Job.IsFixJob()
	hasTokens := review.Job != nil && tokens.ParseJSON(review.Job.TokenUsage) != nil
	if hasVerdict || review.Closed || hasTokens {
		headerHeight++ // Add 1 for verdict/closed/tokens line
	}
	panelReserve := 0
	if m.reviewFixPanelOpen {
		panelReserve = 5 // label + top border + input line + bottom border + help line
	}
	visibleLines := max(m.height-headerHeight-panelReserve, 1)

	// Clamp scroll position to valid range
	maxScroll := max(len(lines)-visibleLines, 0)
	if m.mdCache != nil {
		m.mdCache.lastReviewMaxScroll = maxScroll
	}
	start := max(min(m.reviewScroll, maxScroll), 0)
	end := min(start+visibleLines, len(lines))

	linesWritten := 0
	for i := start; i < end; i++ {
		line := lines[i]
		if m.width > 0 {
			line = xansi.Truncate(line, m.width, "")
		}
		b.WriteString(line)
		b.WriteString("\x1b[K\n") // Clear to end of line before newline
		linesWritten++
	}

	// Pad with clear-to-end-of-line sequences to prevent ghost text
	for linesWritten < visibleLines {
		b.WriteString("\x1b[K\n")
		linesWritten++
	}

	// Render inline fix panel when open
	if m.reviewFixPanelOpen {
		innerWidth := max(m.width-4, 18) // box inner width; total visual width = innerWidth+2 (borders)

		if m.reviewFixPanelFocused {
			// Label line
			label := "Fix: enter instructions (or leave blank for default)"
			if runewidth.StringWidth(label) > m.width-1 {
				label = runewidth.Truncate(label, m.width-1, "")
			}
			b.WriteString(label)
			b.WriteString("\x1b[K\n")

			// Input content — show tail so cursor always visible
			inputDisplay := m.fixPromptText
			maxInputLen := innerWidth - 3 // " > " (3) + "_" (1) = 4 overhead, but Width handles right padding
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
				Width(innerWidth)
			for line := range strings.SplitSeq(strings.TrimRight(boxStyle.Render(content), "\n"), "\n") {
				b.WriteString(line)
				b.WriteString("\x1b[K\n")
			}
			b.WriteString(helpStyle.Render("tab: scroll review | enter: submit | esc: cancel"))
			b.WriteString("\x1b[K\n")
		} else {
			// Label line (dimmed)
			b.WriteString(statusStyle.Render("Fix (Tab to focus)"))
			b.WriteString("\x1b[K\n")

			inputDisplay := m.fixPromptText
			if inputDisplay == "" {
				inputDisplay = "(blank = default)"
			}
			if runewidth.StringWidth(inputDisplay) > innerWidth-2 {
				inputDisplay = runewidth.Truncate(inputDisplay, innerWidth-2, "")
			}
			content := " " + inputDisplay
			boxStyle := lipgloss.NewStyle().
				Border(lipgloss.NormalBorder()).
				BorderForeground(adaptiveColor("242", "246")). // gray (inactive)
				Foreground(adaptiveColor("242", "246")).
				Width(innerWidth)
			for line := range strings.SplitSeq(strings.TrimRight(boxStyle.Render(content), "\n"), "\n") {
				b.WriteString(statusStyle.Render(line))
				b.WriteString("\x1b[K\n")
			}
			b.WriteString(helpStyle.Render("F: fix | tab: focus fix panel"))
			b.WriteString("\x1b[K\n")
		}
	}

	// Status line: flash message takes priority, then scroll indicator.
	if flash := m.renderFlash(viewReview); flash != "" {
		b.WriteString(flash)
	} else if len(lines) > visibleLines {
		scrollInfo := fmt.Sprintf("[%d-%d of %d lines]", start+1, end, len(lines))
		b.WriteString(statusStyle.Render(scrollInfo))
	}
	b.WriteString("\x1b[K\n") // Clear status line

	b.WriteString(renderHelpTable(reviewHelpRows, m.width))
	b.WriteString("\x1b[K")
	b.WriteString("\x1b[J") // Clear to end of screen to prevent artifacts

	return b.String()
}

func (m model) renderPromptView() string {
	var b strings.Builder

	review := m.currentReview
	if review.Job != nil {
		ref := shortJobRef(*review.Job)
		idStr := fmt.Sprintf("#%d ", review.Job.ID)
		agentStr := formatAgentLabel(review.Agent, review.Job.Model)
		title := fmt.Sprintf("Prompt %s%s (%s)", idStr, ref, agentStr)
		b.WriteString(titleStyle.Render(title))
	} else {
		b.WriteString(titleStyle.Render("Prompt"))
	}
	b.WriteString("\x1b[K\n") // Clear to end of line

	// Show command line (computed from job params, dimmed, below title).
	// Collapsed by default; press i to expand the full command via cmdExpanded.
	headerLines := 1
	for _, line := range m.commandHeaderLines(review.Job) {
		b.WriteString(line)
		b.WriteString("\x1b[K\n")
		headerLines++
	}

	// Render markdown content with glamour (cached), falling back to plain text wrapping.
	// wrapWidth caps at 100 for readability; maxWidth uses actual terminal width for truncation.
	maxWidth := max(20, m.width-4)
	wrapWidth := min(maxWidth, 100)
	var lines []string
	if m.mdCache != nil {
		lines = m.mdCache.getPromptLines(review.Prompt, wrapWidth, maxWidth, review.ID)
	} else {
		lines = sanitizeLines(wrapText(review.Prompt, wrapWidth))
	}

	// Reserve: title + command(N, headerLines) + scroll indicator(1) + help(N) + margin(1)
	promptHelpRows := [][]helpItem{
		{{"↑/↓", "scroll"}, {"←/→", "prev/next"}, {"i", "expand cmd"}, {"p", "toggle prompt/review"}, {"?", "commands"}, {"esc", "back"}},
	}
	promptHelpLines := len(reflowHelpRows(promptHelpRows, m.width))
	visibleLines := max(m.height-(2+promptHelpLines)-headerLines, 1)

	// Clamp scroll position to valid range
	maxScroll := max(len(lines)-visibleLines, 0)
	if m.mdCache != nil {
		m.mdCache.lastPromptMaxScroll = maxScroll
	}
	start := max(min(m.promptScroll, maxScroll), 0)
	end := min(start+visibleLines, len(lines))

	linesWritten := 0
	for i := start; i < end; i++ {
		line := lines[i]
		if m.width > 0 {
			line = xansi.Truncate(line, m.width, "")
		}
		b.WriteString(line)
		b.WriteString("\x1b[K\n") // Clear to end of line before newline
		linesWritten++
	}

	// Pad with clear-to-end-of-line sequences to prevent ghost text
	for linesWritten < visibleLines {
		b.WriteString("\x1b[K\n")
		linesWritten++
	}

	// Scroll indicator
	if len(lines) > visibleLines {
		scrollInfo := fmt.Sprintf("[%d-%d of %d lines]", start+1, end, len(lines))
		b.WriteString(statusStyle.Render(scrollInfo))
	}
	b.WriteString("\x1b[K\n") // Clear scroll indicator line

	b.WriteString(renderHelpTable(promptHelpRows, m.width))
	b.WriteString("\x1b[K") // Clear help line
	b.WriteString("\x1b[J") // Clear to end of screen to prevent artifacts

	return b.String()
}

func (m model) renderRespondView() string {
	var b strings.Builder

	title := "Add Comment"
	if m.commentCommit != "" {
		title = fmt.Sprintf("Add Comment (%s)", m.commentCommit)
	}
	b.WriteString(titleStyle.Render(title))
	b.WriteString("\x1b[K\n\x1b[K\n") // Clear title and blank line

	b.WriteString(statusStyle.Render("Enter your comment (e.g., \"This is a known issue, can be ignored\")"))
	b.WriteString("\x1b[K\n\x1b[K\n")

	// Simple text box with border
	boxWidth := max(m.width-4, 20)

	b.WriteString("┌─")
	b.WriteString(strings.Repeat("─", boxWidth-2))
	b.WriteString("─┐\n")

	// Wrap text display to box width
	textLinesWritten := 0
	maxTextLines := max(
		// Reserve space for chrome
		m.height-10, 3)

	if m.commentText == "" {
		// Show placeholder (styled, but we pad manually to avoid ANSI issues)
		placeholder := "Type your comment..."
		padded := placeholder + strings.Repeat(" ", boxWidth-2-len(placeholder))
		b.WriteString("│ ")
		b.WriteString(statusStyle.Render(padded))
		b.WriteString(" │\x1b[K\n")
		textLinesWritten++
	} else {
		lines := strings.SplitSeq(m.commentText, "\n")
		for line := range lines {
			if textLinesWritten >= maxTextLines {
				break
			}
			// Expand tabs to spaces (4-space tabs) for consistent width calculation
			line = strings.ReplaceAll(line, "\t", "    ")
			// Word-wrap long lines to fit within the box
			wrapped := wrapLine(line, boxWidth-2)
			for _, wl := range wrapped {
				if textLinesWritten >= maxTextLines {
					break
				}
				padding := max(boxWidth-2-runewidth.StringWidth(wl), 0)
				fmt.Fprintf(&b, "│ %s%s │\x1b[K\n", wl, strings.Repeat(" ", padding))
				textLinesWritten++
			}
		}
	}

	// Pad with empty lines if needed
	for textLinesWritten < 3 {
		fmt.Fprintf(&b, "│ %-*s │\x1b[K\n", boxWidth-2, "")
		textLinesWritten++
	}

	b.WriteString("└─")
	b.WriteString(strings.Repeat("─", boxWidth-2))
	b.WriteString("─┘\x1b[K\n")

	// Pad remaining space
	linesWritten := 6 + textLinesWritten // title, blank, help, blank, top border, bottom border
	for linesWritten < m.height-1 {
		b.WriteString("\x1b[K\n")
		linesWritten++
	}

	b.WriteString(renderHelpTable([][]helpItem{
		{{"↵", "submit"}, {"esc", "cancel"}},
	}, m.width))
	b.WriteString("\x1b[K")
	b.WriteString("\x1b[J") // Clear to end of screen to prevent artifacts

	return b.String()
}

func (m model) renderCommitMsgView() string {
	var b strings.Builder

	b.WriteString(titleStyle.Render("Commit Message"))
	b.WriteString("\x1b[K\n") // Clear to end of line

	if m.commitMsgContent == "" {
		b.WriteString(statusStyle.Render("Loading commit message..."))
		b.WriteString("\x1b[K\n")
		// Pad to fill terminal
		linesWritten := 2
		for linesWritten < m.height-1 {
			b.WriteString("\x1b[K\n")
			linesWritten++
		}
		b.WriteString(helpStyle.Render("esc/q: back"))
		b.WriteString("\x1b[K")
		b.WriteString("\x1b[J")
		return b.String()
	}

	// Wrap text to terminal width minus padding
	wrapWidth := max(20, min(m.width-4, 100))
	lines := wrapText(m.commitMsgContent, wrapWidth)

	// Reserve: title(1) + scroll indicator(1) + help(1) + margin(1)
	visibleLines := max(m.height-4, 1)

	// Clamp scroll position to valid range
	maxScroll := max(len(lines)-visibleLines, 0)
	start := max(min(m.commitMsgScroll, maxScroll), 0)
	end := min(start+visibleLines, len(lines))

	linesWritten := 0
	for i := start; i < end; i++ {
		b.WriteString(lines[i])
		b.WriteString("\x1b[K\n") // Clear to end of line before newline
		linesWritten++
	}

	// Pad with clear-to-end-of-line sequences to prevent ghost text
	for linesWritten < visibleLines {
		b.WriteString("\x1b[K\n")
		linesWritten++
	}

	// Scroll indicator
	if len(lines) > visibleLines {
		scrollInfo := fmt.Sprintf("[%d-%d of %d lines]", start+1, end, len(lines))
		b.WriteString(statusStyle.Render(scrollInfo))
	}
	b.WriteString("\x1b[K\n") // Clear scroll indicator line

	b.WriteString(renderHelpTable([][]helpItem{
		{{"↑/↓", "scroll"}, {"esc/q", "back"}},
	}, m.width))
	b.WriteString("\x1b[K") // Clear help line
	b.WriteString("\x1b[J") // Clear to end of screen to prevent artifacts

	return b.String()
}
