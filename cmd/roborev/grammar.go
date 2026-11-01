package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

const (
	legacyPathAnnotation  = "roborev.legacy-path"
	replacementAnnotation = "roborev.replacement"
)

// Constructors give both spellings independent flag values, completion
// callbacks, and execution state, without copying Cobra's private state.
var commandMoves = []struct {
	legacy, canonical string
	newCommand        func() *cobra.Command
}{
	{"list", "job list", listCmd},
	{"show", "job show", showCmd},
	{"wait", "job wait", waitCmd},
	{"search", "job search", searchCmd},
	{"comment", "job comment", commentCmd},
	{"close", "job close", closeCmd},
	{"cancel", "job cancel", cancelCmd},
	{"stream", "job stream", streamCmd},
	{"log", "job log", logCmd},
	{"compact", "job compact", compactCmd},
	{"status", "daemon status", statusCmd},
	{"pause", "daemon pause", pauseCmd},
	{"unpause", "daemon resume", unpauseCmd},
	{"install-hook", "hook install", installHookCmd},
	{"uninstall-hook", "hook uninstall", uninstallHookCmd},
	{"check-agents", "agent check", checkAgentsCmd},
	{"skills", "agent skills", skillsCmd},
	{"agent-hook", "agent hook", agentHookCmd},
	{"snooze", "agent hook snooze", snoozeCmd},
	{"summary", "report summary", summaryCmd},
	{"cost", "report cost", costCmd},
	{"insights", "report insights", insightsCmd},
	{"backfill-tokens", "storage backfill-tokens", backfillTokensCmd},
	{"backfill-verdicts", "storage backfill-verdicts", backfillVerdictsCmd},
	{"legacy-reviews", "storage legacy-reviews", legacyReviewsCmd},
	{"signing-init", "signing init", signingInitCmd},
}

func applyCommandGrammar(root *cobra.Command) {
	for _, group := range []struct{ name, short, long, example string }{
		{"job", "Find, read and manage review jobs", "Inspect review jobs and their findings, wait for completion, and record how findings were resolved. Job IDs belong to the selected daemon.", "  roborev job list\n  roborev job show --job 42"},
		{"hook", "Manage automatic review Git hooks", "Install or remove Git hooks that enqueue reviews after commits. Coding-agent harness callbacks live under agent hook.", "  roborev hook install\n  roborev hook install repair"},
		{"agent", "Check agents and manage their integrations", "Check available AI providers and manage bundled skills and coding-agent harness callbacks. Git hooks live under hook.", "  roborev agent check\n  roborev agent hook status"},
		{"report", "Summarize review activity, costs and insights", "Read aggregate review activity and cost reports, or ask an agent for insights grounded in review history.", "  roborev report summary\n  roborev report cost"},
		{"storage", "Repair and migrate stored review data", "Backfill missing review metadata or convert historical Markdown reviews through the daemon. Inspect each command's help before changing stored data.", "  roborev storage backfill-tokens --dry-run\n  roborev storage legacy-reviews export"},
		{"signing", "Set up remote reader signing keys", "Create private signing keys for remote readers. Configure server grants separately before enabling remote access.", "  roborev signing init --secret-file ./reader.key"},
	} {
		root.AddCommand(&cobra.Command{Use: group.name, Short: group.short, Long: group.long, Example: group.example})
	}
	for _, move := range commandMoves {
		legacy := directCommand(root, move.legacy)
		if legacy == nil {
			panic("missing legacy command: " + move.legacy)
		}
		legacy.Hidden = true
		if legacy.Annotations == nil {
			legacy.Annotations = map[string]string{}
		}
		legacy.Annotations[replacementAnnotation] = move.canonical

		parts := strings.Fields(move.canonical)
		parent := root
		for _, name := range parts[:len(parts)-1] {
			parent = directCommand(parent, name)
			if parent == nil {
				panic("missing command group: " + move.canonical)
			}
		}
		name := parts[len(parts)-1]
		canonical := directCommand(parent, name)
		if canonical == nil {
			canonical = move.newCommand()
			_, synopsis, _ := strings.Cut(canonical.Use, " ")
			canonical.Use = name
			if synopsis != "" {
				canonical.Use += " " + synopsis
			}
			parent.AddCommand(canonical)
		} else if move.canonical != "daemon status" {
			panic("duplicate command: " + move.canonical)
		}
		if canonical.Annotations == nil {
			canonical.Annotations = map[string]string{}
		}
		canonical.Annotations[legacyPathAnnotation] = "roborev " + move.legacy
	}
	// Keep the existing respond alias alongside the moved comment command.
	respond := directCommand(root, "respond")
	respond.Hidden = true
	respond.Annotations = map[string]string{replacementAnnotation: "job comment"}
}

func directCommand(parent *cobra.Command, name string) *cobra.Command {
	for _, child := range parent.Commands() {
		if child.Name() == name {
			return child
		}
	}
	return nil
}

// Stable identities keep telemetry and hook behavior independent of CLI layout.
func stableCommandPath(cmd *cobra.Command) string {
	var suffix []string
	for current := cmd; current != nil; current = current.Parent() {
		if path := current.Annotations[legacyPathAnnotation]; path != "" {
			return strings.Join(append([]string{path}, suffix...), " ")
		}
		suffix = append([]string{current.Name()}, suffix...)
	}
	return cmd.CommandPath()
}

func legacyCommandWarning(cmd *cobra.Command, args []string) {
	if isCLIHook(cmd) || cliBoolFlag(cmd, args, "quiet") {
		return
	}
	suffix := ""
	for current := cmd; current != nil; current = current.Parent() {
		if replacement := current.Annotations[replacementAnnotation]; replacement != "" {
			// Cobra's Deprecated field writes to stdout, corrupting JSON and
			// streamed output. Emit our migration notice on stderr after success.
			fmt.Fprintf(cmd.ErrOrStderr(), "Command %q is deprecated, use 'roborev %s%s' instead\n", strings.TrimPrefix(cmd.CommandPath(), "roborev "), replacement, suffix)
			return
		}
		suffix = " " + current.Name() + suffix
	}
}

// Rewrite command references in help, including commands embedded in examples.
// Canonical paths begin with domain nouns, so replacements do not cascade.
func canonicalCommandText(text string) string {
	for _, move := range commandMoves {
		text = strings.ReplaceAll(text, "roborev "+move.legacy, "roborev "+move.canonical)
	}
	return text
}
