# Prompt Command Wrapping Design

## Context

The TUI uses one `cmdExpanded` flag for the command header in both the Prompt
and Log views. Its zero value is collapsed, so both views initially truncate a
command that exceeds the terminal width and append an ellipsis. Although `i`
expands and wraps the command, the Prompt view should show the full invocation
by default, especially on narrow terminals.

## Requirements

- The Prompt view opens with the full command visible and wrapped to the
  terminal width when necessary.
- Pressing `i` in Prompt toggles between wrapped and single-line collapsed
  display.
- The Log view retains its current collapsed default and `i` toggle.
- Prompt and Log expansion choices are independent; visiting or toggling one
  view must not change the other view's presentation.
- Wrapped header height remains included in visible-content calculations so
  content, status, and help rows stay within the terminal height.

## Design

Replace the shared expansion flag with per-view state:

- `promptCmdExpanded` defaults to `true` when the model is constructed.
- `logCmdExpanded` keeps the Go zero value of `false`.

Make `commandHeaderLines` accept the desired expansion state explicitly. The
Prompt renderer passes `promptCmdExpanded`; the Log renderer and
`logVisibleLines` pass `logCmdExpanded`. This keeps the shared wrapping and
truncation implementation while making its caller's policy visible.

The Prompt and Log key handlers toggle only their corresponding state. The
preference persists while navigating between jobs in the same view, but it
does not leak across views. No navigation path needs to reset shared state.

## Error Handling and Edge Cases

- Jobs without a representative command continue to omit the header.
- Unknown or nonpositive terminal widths continue to render the command on one
  unmodified line because no safe wrapping width is available.
- Commands that already fit remain one line even when expansion is enabled.
- Existing Unicode display-width handling and ANSI styling remain unchanged.

## Testing

Update the command-header tests test-first to prove:

- A newly constructed Prompt model renders a narrow command across multiple
  lines without an ellipsis.
- Pressing `i` collapses Prompt and pressing it again restores wrapping.
- A newly constructed Log model remains collapsed.
- Toggling Prompt does not alter Log state, and vice versa.
- Existing header-height and command-content assertions continue to pass.

Run the focused TUI tests, then the repository-wide Go test suite and lint
checks before handoff.
