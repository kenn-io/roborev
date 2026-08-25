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
		return "", true, fmt.Errorf(
			"custom review type %q is not configured", reviewType,
		)
	}
	remaining := limit
	read := func(filePath string) (string, error) {
		data, readErr := b.readCustomReviewFile(filePath, remaining)
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
	limit int,
) ([]byte, error) {
	filePath = strings.TrimSpace(filePath)
	if limit <= 0 {
		return nil, fmt.Errorf("custom review files exceed prompt limit")
	}
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
			return b.readCustomReviewRefFile(clean, limit)
		}
		resolvedPath = filepath.Join(b.repoPath, filePath)
	} else if !filepath.IsAbs(filePath) {
		resolvedPath = filepath.Join(b.repoPath, filePath)
	}
	return readCustomReviewFilesystemFile(resolvedPath, limit)
}

func (b *Builder) readCustomReviewRefFile(
	filePath string,
	limit int,
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
				return enforceCustomFileLimit(candidateEntry.Data, limit)
			}
			entry = candidateEntry
			linkPath = candidate
			remaining = path.Join(parts[i+1:]...)
			break
		}

		linkTarget := string(entry.Data)
		if path.IsAbs(filepath.ToSlash(linkTarget)) || filepath.IsAbs(linkTarget) {
			return readCustomReviewFilesystemFile(
				filepath.Join(linkTarget, filepath.FromSlash(remaining)),
				limit,
			)
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
			return readCustomReviewFilesystemFile(filesystemTarget, limit)
		}
		filePath = filepath.ToSlash(relativeTarget)
	}
}

func readCustomReviewFilesystemFile(filePath string, limit int) ([]byte, error) {
	file, err := os.Open(filePath)
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
