package prompt

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"text/template"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/git"
)

const customReviewOutputInstruction = `

Roborev will constrain the final response with a JSON Schema. Return a concise
summary and every actionable finding. Each finding must include its severity,
problem, and recommended fix; include a location when one is known. Use only
these severity values: critical, high, medium, or low. Do not omit a real
finding because of the configured severity threshold; Roborev applies that
threshold after receiving the structured result.`

type customReviewTemplateData struct {
	ReviewType string
	Includes   map[string]string
}

func (b *Builder) resolveSystemPrompt(
	agentName, reviewType, promptType string,
	limit int,
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
		return GetSystemPrompt(agentName, promptType), false, nil
	}

	remaining := limit
	read := func(filePath string) (string, error) {
		data, readErr := b.readCustomReviewFile(
			filePath, resolved.RepoDefined, remaining,
		)
		if readErr != nil {
			return "", readErr
		}
		remaining -= len(data)
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
		customReviewOutputInstruction
	if len(result)+1 > limit {
		return "", true, fmt.Errorf(
			"review type %q rendered prompt is %d bytes but prompt limit is %d bytes",
			reviewType, len(result)+1, limit,
		)
	}
	return result, true, nil
}

func (b *Builder) resolveRepoConfig() (*config.RepoConfig, error) {
	if b.repoCfg != nil {
		return b.repoCfg, nil
	}
	if b.repoCfgRef != "" {
		return config.LoadRepoConfigFromRef(b.repoPath, b.repoCfgRef)
	}
	return config.LoadRepoConfig(b.repoPath)
}

func (b *Builder) readCustomReviewFile(
	filePath string,
	repoDefined bool,
	limit int,
) ([]byte, error) {
	filePath = strings.TrimSpace(filePath)
	if limit <= 0 {
		return nil, fmt.Errorf("custom review files exceed prompt limit")
	}
	if repoDefined {
		if filepath.IsAbs(filePath) || filePath == "~" ||
			strings.HasPrefix(filePath, "~/") ||
			strings.HasPrefix(filePath, `~\`) {
			return nil, fmt.Errorf(
				"repository-defined path %q must be relative to the repository root",
				filePath,
			)
		}
		clean := path.Clean(filepath.ToSlash(filePath))
		if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
			return nil, fmt.Errorf(
				"repository-defined path %q escapes the repository root",
				filePath,
			)
		}
		if b.repoCfgRef != "" {
			data, err := git.ReadFile(b.repoPath, b.repoCfgRef, clean)
			if err != nil {
				return nil, err
			}
			return enforceCustomFileLimit(data, limit)
		}
		root, err := os.OpenRoot(b.repoPath)
		if err != nil {
			return nil, err
		}
		defer root.Close()
		file, err := root.Open(filepath.FromSlash(clean))
		if err != nil {
			return nil, err
		}
		defer file.Close()
		return readCustomFile(file, limit)
	}

	resolvedPath := filePath
	if filePath == "~" || strings.HasPrefix(filePath, "~/") ||
		strings.HasPrefix(filePath, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		resolvedPath = filepath.Join(home, strings.TrimLeft(filePath[1:], `/\`))
	} else if !filepath.IsAbs(filePath) {
		resolvedPath = filepath.Join(b.repoPath, filePath)
	}
	file, err := os.Open(resolvedPath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return readCustomFile(file, limit)
}

func readCustomFile(r io.Reader, limit int) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	return enforceCustomFileLimit(data, limit)
}

func enforceCustomFileLimit(data []byte, limit int) ([]byte, error) {
	if len(data) > limit {
		return nil, fmt.Errorf(
			"file is %d bytes but only %d bytes remain in the prompt limit",
			len(data), limit,
		)
	}
	return data, nil
}
