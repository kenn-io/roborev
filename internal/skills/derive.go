package skills

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

type skillDerivation struct {
	TargetAgent  Agent
	SkillName    string
	Replacements []stringReplacement
}

type stringReplacement struct {
	Old string
	New string
}

var derivedDroidSkills = []string{
	"roborev-design-review",
	"roborev-design-review-branch",
	"roborev-fix",
	"roborev-lookahead-review",
	"roborev-lookahead-review-branch",
	"roborev-refine",
	"roborev-respond",
	"roborev-review",
	"roborev-review-branch",
	"roborev-snooze",
}

var derivedClaudeSkills = []string{
	"roborev-fix",
	"roborev-refine",
	"roborev-respond",
	"roborev-snooze",
}

// Grok Build uses slash-style skill invocation like Claude Code and discovers
// skills under ~/.grok/skills (see Grok Build user guide).
//
// Capability set matches Droid's full derived surface (Claude-peer plus
// review/design/lookahead skills referenced by those documents), not the
// smaller Claude install set, so cross-links resolve without dangling
// /roborev-* references.
var derivedGrokSkills = []string{
	"roborev-design-review",
	"roborev-design-review-branch",
	"roborev-fix",
	"roborev-lookahead-review",
	"roborev-lookahead-review-branch",
	"roborev-refine",
	"roborev-respond",
	"roborev-review",
	"roborev-review-branch",
	"roborev-snooze",
}

func skillDerivations() []skillDerivation {
	derivations := make([]skillDerivation, 0, len(derivedDroidSkills)+len(derivedClaudeSkills)+len(derivedGrokSkills))
	for _, skillName := range derivedDroidSkills {
		replacements := []stringReplacement{
			{
				Old: ", plugin\n`$roborev:" + skillName + "`, or structured Codex skill selection",
				New: ", or structured\nFactory skill selection",
			},
			{Old: "$roborev", New: "/roborev"},
			{Old: "CLAUDE.md", New: "AGENTS.md"},
			{
				Old: "`sandbox_permissions: \"require_escalated\"`",
				New: "the runtime's supported sandbox escalation mechanism",
			},
		}
		if skillName == "roborev-snooze" {
			replacements = append([]stringReplacement{{
				Old: "invokes $" + skillName + "\n---",
				New: "invokes /" + skillName + "\ndisable-model-invocation: true\n---",
			}}, replacements...)
		}
		derivations = append(derivations, skillDerivation{
			TargetAgent:  AgentDroid,
			SkillName:    skillName,
			Replacements: replacements,
		})
	}
	for _, skillName := range derivedClaudeSkills {
		replacements := []stringReplacement{
			{
				Old: ", plugin\n`$roborev:" + skillName + "`, or structured Codex skill selection",
				New: ", or structured\nClaude Code skill selection",
			},
			{Old: "$roborev", New: "/roborev"},
			{Old: "Retry the same command with", New: "Retry the same Bash command with"},
			{
				Old: "`sandbox_permissions: \"require_escalated\"`",
				New: "`dangerouslyDisableSandbox: true`",
			},
		}
		// roborev-fix must stay model-invocable: the agent-hook Stop hook
		// instructs the Claude Code model to invoke it, and
		// disable-model-invocation would block that path. Its explicit-only
		// description and body section remain the guard against implicit
		// selection.
		if skillName != "roborev-fix" {
			replacements = append([]stringReplacement{{
				Old: "invokes $" + skillName + "\n---",
				New: "invokes /" + skillName + "\ndisable-model-invocation: true\n---",
			}}, replacements...)
		}
		derivations = append(derivations, skillDerivation{
			TargetAgent:  AgentClaude,
			SkillName:    skillName,
			Replacements: replacements,
		})
	}
	for _, skillName := range derivedGrokSkills {
		replacements := []stringReplacement{
			{
				Old: ", plugin\n`$roborev:" + skillName + "`, or structured Codex skill selection",
				New: ", or structured\nGrok Build skill selection",
			},
			{Old: "$roborev", New: "/roborev"},
			// Grok project rules use AGENTS.md (CLAUDE.md is a compatibility alias).
			{Old: "CLAUDE.md", New: "AGENTS.md"},
			{
				Old: "`sandbox_permissions: \"require_escalated\"`",
				New: "the runtime's supported sandbox escalation mechanism",
			},
		}
		// Same model-invocation rules as Claude: roborev-fix stays invocable
		// for agent-hook Stop; other skills are explicit-only.
		if skillName != "roborev-fix" {
			replacements = append([]stringReplacement{{
				Old: "invokes $" + skillName + "\n---",
				New: "invokes /" + skillName + "\ndisable-model-invocation: true\n---",
			}}, replacements...)
		}
		derivations = append(derivations, skillDerivation{
			TargetAgent:  AgentGrok,
			SkillName:    skillName,
			Replacements: replacements,
		})
	}
	return derivations
}

func renderDerivedSkills(fsys fs.FS) (map[string][]byte, error) {
	out := make(map[string][]byte)
	for _, derivation := range skillDerivations() {
		if err := validateSkillDerivation(derivation); err != nil {
			return nil, err
		}

		content, err := fs.ReadFile(fsys, path.Join("codex", derivation.SkillName, "SKILL.md"))
		if err != nil {
			return nil, fmt.Errorf("read source skill %s: %w", derivation.SkillName, err)
		}

		rendered := string(content)
		for _, replacement := range derivation.Replacements {
			rendered = strings.ReplaceAll(rendered, replacement.Old, replacement.New)
		}

		out[path.Join(string(derivation.TargetAgent), derivation.SkillName, "SKILL.md")] = []byte(rendered)
	}
	return out, nil
}

func validateSkillDerivation(derivation skillDerivation) error {
	if _, ok := lookupAgent(derivation.TargetAgent); !ok {
		return fmt.Errorf("unknown target agent %q", derivation.TargetAgent)
	}

	switch derivation.TargetAgent {
	case AgentDroid:
		if !slices.Contains(derivedDroidSkills, derivation.SkillName) {
			return fmt.Errorf("unknown droid derived skill %q", derivation.SkillName)
		}
	case AgentClaude:
		if !slices.Contains(derivedClaudeSkills, derivation.SkillName) {
			return fmt.Errorf("unknown claude derived skill %q", derivation.SkillName)
		}
	case AgentGrok:
		if !slices.Contains(derivedGrokSkills, derivation.SkillName) {
			return fmt.Errorf("unknown grok derived skill %q", derivation.SkillName)
		}
	default:
		return fmt.Errorf("unsupported derived target agent %q", derivation.TargetAgent)
	}

	return nil
}

// WriteDerivedSkillFiles rewrites checked-in derived skill files under skillRoot.
func WriteDerivedSkillFiles(skillRoot string) error {
	derived, err := renderDerivedSkills(os.DirFS(skillRoot))
	if err != nil {
		return err
	}

	for relPath, content := range derived {
		dest := filepath.Join(skillRoot, filepath.FromSlash(relPath))
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", filepath.Dir(dest), err)
		}
		if err := os.WriteFile(dest, content, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", relPath, err)
		}
	}
	return nil
}
