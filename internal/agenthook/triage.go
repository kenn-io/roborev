package agenthook

import "strings"

// DefaultInstruction is the built-in workflow used unless the user supplies one.
const DefaultInstruction = `Triage the review job IDs named in this reminder with the roborev-fix skill,
and only those IDs. Fix reachable defects with the smallest change that stays
inside the user's current task, decline or disprove the rest with a recorded
reason, and ask the user about anything that would grow the task. Then return
to the work this reminder interrupted.`

// These are exact defaults shipped by earlier releases. Saved copies follow
// the current workflow, just like an unset instruction.
var legacyDefaultInstructions = []string{
	"Invoke the $roborev-fix skill now.",
	"Run the roborev-fix skill to address the unresolved roborev findings, then continue.",
	"Resolve open roborev findings now. Use the roborev-fix skill if available; " +
		"otherwise run `roborev fix --open --list`, inspect each job with " +
		"`roborev show --job <id> --json`, fix and verify all findings, record each fix with " +
		"`roborev comment --commenter agent-hook --job <id> \"<summary>\"`, then run " +
		"`roborev close <id>` before continuing.",
	`Invoke the roborev-fix skill for only the review job IDs named in this reminder.
Do not discover or address any other reviews. Before editing, independently validate every finding
against the current code and the user's current task; a review finding is not proof that a problem
exists. Never expand the scope of that task. Fix and verify valid findings only when they are clearly
within scope. For invalid, stale, already-resolved, or inapplicable findings, make no code change,
record the evidence, and close the review. If a valid finding is outside the task or its scope is
unclear, leave it open and ask the user for direction. Then continue the task that this hook interrupted.`,
	`Triage the review job IDs named in this reminder with the roborev-fix skill,
and only those IDs. Fix reachable defects with the smallest change that stays
inside the user's current task, decline or disprove the rest with a recorded
reason, and ask the user about anything that would grow the task. Then return
to the work this reminder interrupted.`,
}

// IsBuiltinInstruction reports whether s is empty or a shipped default.
// Config saves and hand wrapping must not turn built-in guidance into a custom flow.
func IsBuiltinInstruction(s string) bool {
	normalized := normalizeInstruction(s)
	if normalized == "" || normalized == normalizeInstruction(DefaultInstruction) {
		return true
	}
	for _, legacy := range legacyDefaultInstructions {
		if normalized == normalizeInstruction(legacy) {
			return true
		}
	}
	return false
}

func normalizeInstruction(s string) string { return strings.Join(strings.Fields(s), " ") }
func effectiveInstruction(s string) string {
	if IsBuiltinInstruction(s) {
		return DefaultInstruction
	}
	return strings.TrimSpace(s)
}

func customInstruction(s string) string {
	if IsBuiltinInstruction(s) {
		return ""
	}
	return strings.TrimSpace(s)
}
