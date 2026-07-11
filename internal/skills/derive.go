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
}

var derivedClaudeSkills = []string{
	"roborev-fix",
	"roborev-refine",
	"roborev-respond",
}

func skillDerivations() []skillDerivation {
	derivations := make([]skillDerivation, 0, len(derivedDroidSkills)+len(derivedClaudeSkills))
	for _, skillName := range derivedDroidSkills {
		derivations = append(derivations, skillDerivation{
			TargetAgent: AgentDroid,
			SkillName:   skillName,
			Replacements: []stringReplacement{
				{
					Old: ", plugin\n`$roborev:" + skillName + "`, or structured Codex skill selection",
					New: ", or structured\nFactory skill selection",
				},
				{Old: "$roborev", New: "/roborev"},
				{Old: "CLAUDE.md", New: "AGENTS.md"},
			},
		})
	}
	for _, skillName := range derivedClaudeSkills {
		replacements := []stringReplacement{
			{
				Old: ", plugin\n`$roborev:" + skillName + "`, or structured Codex skill selection",
				New: ", or structured\nClaude Code skill selection",
			},
			{Old: "$roborev", New: "/roborev"},
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
		if err := os.WriteFile(dest, content, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", relPath, err)
		}
	}
	return nil
}
