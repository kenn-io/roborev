# Prompt Command Wrapping Implementation Plan

> **For agentic workers:** REQUIRED: Use superpowers:subagent-driven-development (if subagents available) or superpowers:executing-plans to implement this plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make Prompt show the full command wrapped by default while preserving an independent collapsed-by-default Log command header.

**Architecture:** Replace the shared command-expansion boolean with one boolean per view. Keep command wrapping and truncation in the existing shared renderer, but pass the desired view state explicitly so rendering and visible-height calculations use the same policy.

**Tech Stack:** Go, Bubble Tea v2, Lip Gloss/terminal-width helpers, Testify

---

## File Map

- `cmd/roborev/tui/tui.go`: own Prompt and Log expansion state and initialize Prompt to expanded.
- `cmd/roborev/tui/render_log.go`: render command headers from explicit expansion state and use Log state.
- `cmd/roborev/tui/render_review.go`: use Prompt state and update the compact help label.
- `cmd/roborev/tui/nav.go`: calculate Log content height from Log expansion state.
- `cmd/roborev/tui/handlers.go`: toggle only Prompt expansion state.
- `cmd/roborev/tui/handlers_modal.go`: toggle only Log expansion state.
- `cmd/roborev/tui/command_header_test.go`: pin Prompt defaults, independent toggles, help text, wrapping, truncation, and height accounting.

### Task 1: Separate Prompt and Log command-header state

**Files:**

- Modify: `cmd/roborev/tui/command_header_test.go`
- Modify: `cmd/roborev/tui/tui.go:404-407,620-660`
- Modify: `cmd/roborev/tui/render_log.go:12-40,64-70`
- Modify: `cmd/roborev/tui/render_review.go:312-336`
- Modify: `cmd/roborev/tui/nav.go:170-181`
- Modify: `cmd/roborev/tui/handlers.go:131-136`
- Modify: `cmd/roborev/tui/handlers_modal.go:322-324`

- [ ] **Step 1: Add failing tests for the requested behavior**

Follow @superpowers:test-driven-development. Update existing direct helper calls to the desired explicit-state API, rename the shared-state toggle assertions, and add focused tests equivalent to:

```go
func TestPromptCommandWrapsByDefault(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.width = 12
	job := makeJob(1, withAgent("test"))

	lines := m.commandHeaderLines(&job, m.promptCmdExpanded)

	assert.Greater(t, len(lines), 1)
	assert.NotContains(t, strings.Join(lines, "\n"), "…")
}

func TestCommandExpansionStateIsIndependentByView(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.height = 30
	m.width = 12
	job := makeJob(1, withAgent("test"))
	m.jobs = []storage.ReviewJob{job}
	m.currentReview = &storage.Review{
		ID:     1,
		JobID:  1,
		Agent:  "test",
		Prompt: "hello",
		Job:    &job,
	}

	assert.True(t, m.promptCmdExpanded)
	assert.False(t, m.logCmdExpanded)

	m.currentView = viewKindPrompt
	promptCollapsed, _ := pressKey(m, 'i')
	assert.False(t, promptCollapsed.promptCmdExpanded)
	assert.False(t, promptCollapsed.logCmdExpanded)

	promptCollapsed.currentView = viewLog
	promptCollapsed.logJobID = 1
	logExpanded, _ := pressKey(promptCollapsed, 'i')
	assert.False(t, logExpanded.promptCmdExpanded)
	assert.True(t, logExpanded.logCmdExpanded)
}

func TestPromptViewLabelsCommandToggle(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.currentView = viewKindPrompt
	m.height = 30
	m.width = 160
	job := makeJob(1, withAgent("test"))
	m.currentReview = &storage.Review{
		ID:     1,
		JobID:  1,
		Agent:  "test",
		Prompt: "hello",
		Job:    &job,
	}

	view := m.View().Content
	assert.Contains(t, view, "toggle cmd")
	assert.NotContains(t, view, "expand cmd")
}
```

Update the existing tests as follows:

- `commandHeaderLines(&job)` becomes `commandHeaderLines(&job, false)` for collapsed cases and `commandHeaderLines(&job, true)` for expanded cases.
- `cmdExpanded` in Log tests becomes `logCmdExpanded`.
- `TestPromptViewTogglesCommandExpand` starts by asserting `promptCmdExpanded` is true, then asserts the first `i` press makes it false and the second makes it true.
- Keep `TestLogViewTogglesCommandExpand` asserting the Log default is false, then true, then false.
- Update `TestQueueViewIgnoresCommandExpandKey` to assert that pressing `i`
  changes neither `promptCmdExpanded` nor `logCmdExpanded`.

- [ ] **Step 2: Run the focused tests and verify RED**

Run:

```bash
ROBOREV_DATA_DIR="$(mktemp -d)" go test ./cmd/roborev/tui -run 'Test(CommandHeader|PromptCommand|CommandExpansion|PromptView|LogView|LogVisible)' -count=1
```

Expected: FAIL to compile because `promptCmdExpanded`, `logCmdExpanded`, and the explicit expansion argument do not exist yet. This is the expected missing-behavior failure; there must be no unrelated fixture or environment failure.

- [ ] **Step 3: Implement the minimal per-view state**

In `cmd/roborev/tui/tui.go`, replace the shared field and initialize Prompt explicitly:

```go
// Command-line header expansion is independent by view. Prompt defaults to
// expanded so long invocations remain readable; Log retains its compact
// collapsed default.
promptCmdExpanded bool
logCmdExpanded    bool
```

Add this entry to the `model{...}` literal in `newModel`:

```go
promptCmdExpanded: true,
```

In `cmd/roborev/tui/render_log.go`, make the helper's policy explicit:

```go
func (m model) commandHeaderLines(
	job *storage.ReviewJob, expanded bool,
) []string {
	cmdLine := commandLineForJob(job)
	if cmdLine == "" {
		return nil
	}
	cmdText := "Command: " + cmdLine
	if m.width <= 0 {
		return []string{statusStyle.Render(cmdText)}
	}
	if expanded {
		wrapped := wrapLine(cmdText, m.width)
		lines := make([]string, len(wrapped))
		for i, ln := range wrapped {
			lines[i] = statusStyle.Render(ln)
		}
		return lines
	}
	if runewidth.StringWidth(cmdText) > m.width {
		cmdText = runewidth.Truncate(cmdText, m.width, "…")
	}
	return []string{statusStyle.Render(cmdText)}
}
```

Call it with the matching state at every production call site:

```go
// renderLogView and logVisibleLines
m.commandHeaderLines(job, m.logCmdExpanded)

// renderPromptView
m.commandHeaderLines(review.Job, m.promptCmdExpanded)
```

Toggle only the active view's state:

```go
// handlers.go, Prompt case
m.promptCmdExpanded = !m.promptCmdExpanded

// handlers_modal.go, Log case
m.logCmdExpanded = !m.logCmdExpanded
```

Change Prompt's compact help item to:

```go
{"i", "toggle cmd"}
```

Update nearby comments so they describe Prompt's expanded default and Log's collapsed default. Do not reset either field during navigation; each choice should persist while the TUI remains open.

- [ ] **Step 4: Format and verify GREEN with focused tests**

Run:

```bash
gofmt -w cmd/roborev/tui/tui.go cmd/roborev/tui/render_log.go cmd/roborev/tui/render_review.go cmd/roborev/tui/nav.go cmd/roborev/tui/handlers.go cmd/roborev/tui/handlers_modal.go cmd/roborev/tui/command_header_test.go
ROBOREV_DATA_DIR="$(mktemp -d)" go test ./cmd/roborev/tui -run 'Test(CommandHeader|PromptCommand|CommandExpansion|PromptView|LogView|LogVisible)' -count=1
```

Expected: PASS with no warnings or ambient-daemon access.

- [ ] **Step 5: Run package and repository quality gates**

Run in isolated state:

```bash
ROBOREV_DATA_DIR="$(mktemp -d)" go test ./cmd/roborev/tui -count=1
ROBOREV_DATA_DIR="$(mktemp -d)" go test ./... -count=1
go build ./...
make lint-ci
```

Expected: every command exits 0. Do not run `make install`, `go install`, or a branch binary against the live daemon/data directory.

- [ ] **Step 6: Review and commit the implementation**

Use @kenn:verify-before-handoff, @superpowers:verification-before-completion, @kenn:scrub-private-data, and the mandatory @kenn:commit workflow. Review `git diff`, `git diff --stat`, and `git status`; stage all six related production files and the test file, then create a rationale-focused commit without amending either design commit.

```bash
git add cmd/roborev/tui/tui.go \
  cmd/roborev/tui/render_log.go \
  cmd/roborev/tui/render_review.go \
  cmd/roborev/tui/nav.go \
  cmd/roborev/tui/handlers.go \
  cmd/roborev/tui/handlers_modal.go \
  cmd/roborev/tui/command_header_test.go
git commit -m "Wrap prompt commands by default"
```

The commit body should explain that narrow Prompt views hid long invocations and that independent state preserves Log's compact default.

- [ ] **Step 7: Publish and verify the branch**

Follow the repository's session-completion instructions. Confirm there is no remaining work requiring an issue, run the private-data scan over the complete unpushed range, then:

```bash
git pull --rebase
git push -u origin glacier-couch
git status --short --branch
git stash list
git remote prune origin --dry-run
```

Expected: push succeeds, status reports `glacier-couch` is up to date with its upstream, there are no task-created stashes, and the prune dry run does not mutate repository state.
