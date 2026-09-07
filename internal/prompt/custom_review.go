package prompt

import (
	"bytes"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"text/template"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/git"
)

const structuredReviewOutputInstruction = `

Roborev will constrain the final response with a JSON Schema. Return a concise
summary, your overall verdict, and every actionable finding. Each finding must
include its severity, problem, and recommended fix; include a location when one
is known. Use only these severity values: critical, high, medium, or low. The
verdict is "pass" when the change is acceptable, "fail" when it is not, and
"unable_to_review" only when you could not assess the change at all, for
example because the diff is missing or unreadable; explain why in the summary.`

// ReconcileStructuredOutputInstruction makes a prebuilt review prompt match
// the agent that will actually run it. Prompts are built before failover,
// so a prompt written for a structured agent may reach a prose agent and
// vice versa. A prose agent must not be told its answer will be
// schema-constrained, and a structured agent should be told what the schema
// expects. The instruction is appended only when absent, so custom prompts
// that already embed it are unchanged.
func ReconcileStructuredOutputInstruction(prompt string, structured bool) string {
	present := strings.Contains(prompt, structuredReviewOutputInstruction)
	switch {
	case structured && !present:
		return prompt + structuredReviewOutputInstruction
	case !structured && present:
		return strings.ReplaceAll(prompt, structuredReviewOutputInstruction, "")
	default:
		return prompt
	}
}

type customReviewTemplateData struct {
	ReviewType string
	Includes   map[string]string
}

func (b *Builder) resolveSystemPrompt(
	agentName, reviewType, promptType string,
) (string, bool, error) {
	if config.IsBuiltInReviewType(reviewType) {
		return GetSystemPrompt(agentName, promptType), false, nil
	}
	repoCfg, err := b.resolveRepoConfig()
	if err != nil {
		return "", false, err
	}
	resolved, ok := config.ResolveCustomReviewTypeFromConfig(
		reviewType, repoCfg, b.globalCfg,
	)
	if !ok {
		return "", true, fmt.Errorf(
			"custom review type %q is not configured", reviewType,
		)
	}
	read := func(filePath string) (string, error) {
		data, readErr := b.readCustomReviewFile(filePath)
		if readErr != nil {
			return "", readErr
		}
		return string(data), nil
	}

	templateText, err := read(resolved.Spec.Template)
	if err != nil {
		return "", true, fmt.Errorf(
			"review type %q template: %w", reviewType, err,
		)
	}
	includes := make(map[string]string, len(resolved.Spec.Includes))
	for name, filePath := range resolved.Spec.Includes {
		contents, includeErr := read(filePath)
		if includeErr != nil {
			return "", true, fmt.Errorf(
				"review type %q include %q: %w",
				reviewType, name, includeErr,
			)
		}
		includes[name] = contents
	}

	tmpl, err := template.New(reviewType).
		Option("missingkey=error").
		Parse(templateText)
	if err != nil {
		return "", true, fmt.Errorf(
			"review type %q parse template: %w", reviewType, err,
		)
	}
	var rendered bytes.Buffer
	if err := tmpl.Execute(&rendered, customReviewTemplateData{
		ReviewType: reviewType,
		Includes:   includes,
	}); err != nil {
		return "", true, fmt.Errorf(
			"review type %q render template: %w", reviewType, err,
		)
	}
	result := strings.TrimSpace(rendered.String()) +
		structuredReviewOutputInstruction
	return result, true, nil
}

func (b *Builder) resolveRepoConfig() (*config.RepoConfig, error) {
	if b.repoCfgSet {
		return b.repoCfg, nil
	}
	if b.repoCfgRef != "" {
		return config.LoadRepoConfigFromRef(b.repoPath, b.repoCfgRef)
	}
	return config.LoadRepoConfig(b.repoPath)
}

func (b *Builder) readCustomReviewFile(
	filePath string,
) ([]byte, error) {
	filePath = strings.TrimSpace(filePath)
	resolvedPath := filePath
	if filePath == "~" || strings.HasPrefix(filePath, "~/") ||
		strings.HasPrefix(filePath, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		resolvedPath = filepath.Join(home, strings.TrimLeft(filePath[1:], `/\`))
	} else if !filepath.IsAbs(filePath) && b.repoCfgRef != "" {
		clean := path.Clean(filepath.ToSlash(filePath))
		if clean != "." && clean != ".." && !strings.HasPrefix(clean, "../") {
			return b.readCustomReviewRefFile(clean)
		}
		resolvedPath = filepath.Join(b.repoPath, filePath)
	} else if !filepath.IsAbs(filePath) {
		resolvedPath = filepath.Join(b.repoPath, filePath)
	}
	return os.ReadFile(resolvedPath)
}

func (b *Builder) readCustomReviewRefFile(
	filePath string,
) ([]byte, error) {
	seen := make(map[string]struct{})
	for {
		if _, ok := seen[filePath]; ok {
			return nil, fmt.Errorf("custom review file symlink cycle at %q", filePath)
		}
		seen[filePath] = struct{}{}

		parts := strings.Split(filePath, "/")
		var entry git.RefFile
		var linkPath string
		var remaining string
		for i := range parts {
			candidate := path.Join(parts[:i+1]...)
			candidateEntry, err := git.ReadRefFile(
				b.repoPath, b.repoCfgRef, candidate,
			)
			if err != nil {
				if i == len(parts)-1 {
					return nil, err
				}
				continue
			}
			if !candidateEntry.Symlink {
				if i != len(parts)-1 {
					return nil, fmt.Errorf(
						"git path %s:%s is not a directory",
						b.repoCfgRef, candidate,
					)
				}
				return candidateEntry.Data, nil
			}
			entry = candidateEntry
			linkPath = candidate
			remaining = path.Join(parts[i+1:]...)
			break
		}

		linkTarget := string(entry.Data)
		if path.IsAbs(filepath.ToSlash(linkTarget)) || filepath.IsAbs(linkTarget) {
			return os.ReadFile(filepath.Join(linkTarget, filepath.FromSlash(remaining)))
		}
		filesystemTarget := filepath.Clean(filepath.Join(
			b.repoPath,
			filepath.FromSlash(path.Dir(linkPath)),
			linkTarget,
			filepath.FromSlash(remaining),
		))
		relativeTarget, relErr := filepath.Rel(b.repoPath, filesystemTarget)
		if relErr != nil || relativeTarget == ".." ||
			strings.HasPrefix(relativeTarget, ".."+string(filepath.Separator)) {
			return os.ReadFile(filesystemTarget)
		}
		filePath = filepath.ToSlash(relativeTarget)
	}
}
