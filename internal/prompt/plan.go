package prompt

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/git"
	"go.kenn.io/roborev/internal/storage"
)

// FormatPlans keeps proposed work separate from records of attempted fixes.
func FormatPlans(plans []storage.Response) string {
	if len(plans) == 0 {
		return ""
	}
	var body strings.Builder
	body.WriteString("## Previous Plans\n\nThese are proposed approaches, not records of completed fixes.\n\n")
	for _, plan := range plans {
		fmt.Fprintf(&body, "--- Plan by %s", plan.Responder)
		if !plan.CreatedAt.IsZero() {
			fmt.Fprintf(&body, " at %s", plan.CreatedAt.Format("2006-01-02 15:04"))
		}
		fmt.Fprintf(&body, " ---\n%s\n\n", plan.Response)
	}
	return body.String()
}

// WithFixPlan adds strategy before the findings, leaving unplanned prompts intact.
func WithFixPlan(implementationPrompt, plan string) string {
	if plan == "" {
		return implementationPrompt
	}
	section := "## Plan\n\n" + plan + "\n\n"
	if index := strings.LastIndex(implementationPrompt, "## Review Findings to Address"); index >= 0 {
		return implementationPrompt[:index] + section + implementationPrompt[index:]
	}
	for _, heading := range []string{"## Analysis Findings", "## Review Findings", "## Review 1"} {
		if index := strings.Index(implementationPrompt, heading); index >= 0 {
			return implementationPrompt[:index] + section + implementationPrompt[index:]
		}
	}
	return section + implementationPrompt
}

// BuildFixPlanPrompt assembles read-only analysis context shared by fix flows.
func BuildFixPlanPrompt(repoPath string, cfg *config.Config, findings, minSeverity string, responses []storage.Response, instructions string) (string, error) {
	return buildFixPlanPrompt(repoPath, repoPath, cfg, findings, minSeverity, responses, instructions)
}

func buildFixPlanPrompt(repoPath, skillRoot string, cfg *config.Config, findings, minSeverity string, responses []storage.Response, instructions string) (string, error) {
	var context strings.Builder
	if guidelines := LoadGuidelinesLocal(repoPath, cfg); guidelines != "" {
		context.WriteString("## Project Guidelines\n\n" + guidelines + "\n\n")
	}
	if cfg != nil && cfg.FixGuidelines != "" {
		context.WriteString("## Fix Guidelines\n\n" + cfg.FixGuidelines + "\n\n")
	}
	attempts, comments, plans := SplitResponses(responses)
	context.WriteString(FormatPlans(plans))
	context.WriteString(FormatToolAttempts(attempts))
	context.WriteString(FormatUserComments(comments))
	if instructions != "" {
		context.WriteString("## Additional Context\n\n" + instructions + "\n\n")
	}
	context.WriteString(config.SeverityInstruction(minSeverity))
	context.WriteString("\n## Review Findings\n\n" + findings + "\n")
	body, err := templateFS.ReadFile("templates/assembled_fix_plan.md.gotmpl")
	if err != nil {
		return "", err
	}
	tmpl, err := template.New("fix_plan").Parse(string(body))
	if err != nil {
		return "", err
	}
	var result bytes.Buffer
	err = tmpl.Execute(&result, struct{ Context, Superpowers string }{
		Context: context.String(), Superpowers: superpowersPlanGuidance(skillRoot),
	})
	return result.String(), err
}

// FixPlanReviewContext associates findings with the reviewed ref and available diff.
func FixPlanReviewContext(repoPath string, review *storage.Review) string {
	var context strings.Builder
	if review.Job != nil && review.Job.GitRef != "" {
		fmt.Fprintf(&context, "Reviewed reference: %q\n\n", review.Job.GitRef)
	}
	context.WriteString(review.Output)
	if review.Job != nil && review.Job.GitRef != "" && review.Job.GitRef != "dirty" {
		if diff, err := git.GetDiff(repoPath, review.Job.GitRef); err == nil && len(diff) > 0 {
			context.WriteString("\n## Original Commit Diff (for context)\n\n```diff\n" + diff + "\n```\n")
		}
	}
	return context.String()
}

// BuildPlanPrompt also supplies the reviewed ref and original diff when available.
func (b *Builder) BuildPlanPrompt(review *storage.Review, responses []storage.Response, minSeverity string) (string, error) {
	skillRoot := b.planSkillRoot
	if skillRoot == "" {
		skillRoot = b.repoPath
	}
	return buildFixPlanPrompt(
		b.repoPath, skillRoot, b.globalCfg,
		FixPlanReviewContext(b.repoPath, review), minSeverity, responses, "",
	)
}

func superpowersPlanGuidance(repoPath string) string {
	found := make(map[string]bool)
	for _, root := range []string{".agents/skills", ".claude/skills", ".pi/skills"} {
		entries, _ := os.ReadDir(filepath.Join(repoPath, root))
		for _, entry := range entries {
			body, err := os.ReadFile(filepath.Join(repoPath, root, entry.Name(), "SKILL.md"))
			if err != nil || !strings.HasPrefix(string(body), "---\n") {
				continue
			}
			frontMatter, _, _ := strings.Cut(string(body)[4:], "\n---")
			for line := range strings.SplitSeq(frontMatter, "\n") {
				if name, ok := strings.CutPrefix(line, "name:"); ok {
					found[strings.Trim(strings.TrimSpace(name), "\"'")] = true
				}
			}
		}
	}
	if !found["brainstorming"] || !found["writing-plans"] {
		return ""
	}
	return "## Superpowers planning discipline\n\n" +
		"The repository uses Superpowers brainstorming and writing-plans. Apply their " +
		"design discipline within this read-only pass: understand the intent and constraints, " +
		"compare plausible approaches, select a concrete design, and turn it into ordered " +
		"implementation steps with file/function targets and regression tests. " +
		"Self-check that the steps cover every actionable finding. " +
		"Return the artifact inline; skip approval pauses, delegation, document writes, and commits.\n\n"
}
