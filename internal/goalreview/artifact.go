// Package goalreview reviews frozen Superpowers artifacts and Kata intent.
package goalreview

import (
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

type Selection struct {
	SpecFile string
	PlanFile string
}

type Artifact struct {
	Kind    string `json:"kind"`
	Path    string `json:"path"`
	Content string `json:"content"`
}

type Artifacts struct {
	Spec Artifact
	Plan *Artifact
}

// ResolveArtifacts reads the current checkout, including ignored and uncommitted
// artifacts. Historical documents are never selected by date or modification time.
func ResolveArtifacts(root string, selection Selection) (Artifacts, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return Artifacts{}, err
	}
	var result Artifacts
	if selection.PlanFile != "" {
		plan, err := readArtifact(root, selection.PlanFile, "plan")
		if err != nil {
			return result, err
		}
		result.Plan = &plan
		ref, err := specReference(plan.Content)
		if err != nil {
			return result, fmt.Errorf("%s: %w", plan.Path, err)
		}
		if selection.SpecFile == "" {
			if ref == "" {
				return result, fmt.Errorf("plan has no Spec reference; supply --spec")
			}
			path, err := canonicalPath(root, ref)
			if err != nil {
				return result, err
			}
			if !isDesignatedSpecPath(path) {
				return result, fmt.Errorf("custom plan Spec reference requires --spec")
			}
			selection.SpecFile = path
		} else if ref != "" {
			want, err := canonicalPath(root, selection.SpecFile)
			if err != nil {
				return result, err
			}
			got, err := canonicalPath(root, ref)
			if err != nil {
				return result, err
			}
			if got != want {
				return result, fmt.Errorf("plan Spec reference disagrees with --spec")
			}
		}
	}
	if selection.SpecFile == "" {
		paths, err := filepath.Glob(filepath.Join(root, "docs/superpowers/specs/*-design.md"))
		if err != nil {
			return result, err
		}
		if len(paths) != 1 {
			return result, fmt.Errorf("found %d Superpowers specs; supply --spec", len(paths))
		}
		selection.SpecFile = paths[0]
	}
	result.Spec, err = readArtifact(root, selection.SpecFile, "spec")
	if err != nil {
		return result, err
	}
	if result.Plan != nil {
		if result.Spec.Path == result.Plan.Path {
			return result, fmt.Errorf("spec and plan must be different files")
		}
		return result, nil
	}
	paths, err := filepath.Glob(filepath.Join(root, "docs/superpowers/plans/*.md"))
	if err != nil {
		return result, err
	}
	for _, path := range paths {
		plan, err := readArtifact(root, path, "plan")
		if err != nil {
			return result, err
		}
		ref, err := specReference(plan.Content)
		if err != nil {
			return result, fmt.Errorf("%s: %w", plan.Path, err)
		}
		if ref == "" {
			continue
		}
		// Unrelated historical plans may point at retired specs. They are not
		// candidates, but an invalid escaping reference is still rejected.
		candidate := ref
		if !filepath.IsAbs(candidate) {
			candidate = filepath.Join(root, candidate)
		}
		rel, err := filepath.Rel(root, candidate)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return result, fmt.Errorf("plan Spec reference escapes checkout")
		}
		canonical, err := canonicalPath(root, ref)
		if err != nil {
			return result, err
		}
		if canonical != result.Spec.Path {
			continue
		}
		if result.Plan != nil {
			return result, fmt.Errorf("multiple plans reference spec; supply --plan")
		}
		result.Plan = &plan
	}
	return result, nil
}

func canonicalPath(root, name string) (string, error) {
	if !utf8.ValidString(name) {
		return "", fmt.Errorf("artifact path must be UTF-8")
	}
	path := name
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	path, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("artifact path escapes checkout: %s", name)
	}
	if !utf8.ValidString(rel) {
		return "", fmt.Errorf("artifact path must be UTF-8")
	}
	return filepath.ToSlash(rel), nil
}

func isDesignatedSpecPath(rel string) bool {
	const prefix = "docs/superpowers/specs/"
	if !strings.HasPrefix(rel, prefix) {
		return false
	}
	name := strings.TrimPrefix(rel, prefix)
	return name != "" && !strings.Contains(name, "/") && strings.HasSuffix(name, "-design.md")
}

func readArtifact(root, path, kind string) (Artifact, error) {
	rel, err := canonicalPath(root, path)
	if err != nil {
		return Artifact{}, fmt.Errorf("%s: %w", kind, err)
	}
	dir, name, err := openArtifactParent(root, rel)
	if err != nil {
		return Artifact{}, err
	}
	defer dir.Close()
	info, err := dir.Lstat(name)
	if err != nil {
		return Artifact{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return Artifact{}, fmt.Errorf("%s must not be a symbolic link", rel)
	}
	if !info.Mode().IsRegular() {
		return Artifact{}, fmt.Errorf("%s must be a regular file", rel)
	}
	// A replacement FIFO between Lstat and Open must not block capture.
	file, err := dir.OpenFile(name, os.O_RDONLY|artifactOpenFlags, 0)
	if err != nil {
		return Artifact{}, err
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil {
		return Artifact{}, err
	}
	if !before.Mode().IsRegular() || !os.SameFile(info, before) {
		return Artifact{}, fmt.Errorf("%s must be a regular file", rel)
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return Artifact{}, err
	}
	after, err := file.Stat()
	if err != nil {
		return Artifact{}, err
	}
	current, err := dir.Lstat(name)
	if err != nil {
		return Artifact{}, err
	}
	if current.Mode()&os.ModeSymlink != 0 || !os.SameFile(before, current) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return Artifact{}, fmt.Errorf("artifact changed during read: %s", rel)
	}
	if !utf8.Valid(data) || strings.TrimSpace(string(data)) == "" {
		return Artifact{}, fmt.Errorf("%s must contain nonempty UTF-8 text", rel)
	}
	return Artifact{Kind: kind, Path: rel, Content: string(data)}, nil
}

// openArtifactParent walks each directory through an anchored Root and checks
// its identity after opening. This rejects symlinked path components even if a
// directory entry changes between the check and the open.
func openArtifactParent(root, rel string) (*os.Root, string, error) {
	components := strings.Split(rel, "/")
	if len(components) == 0 {
		return nil, "", fmt.Errorf("invalid artifact path")
	}
	dir, err := os.OpenRoot(root)
	if err != nil {
		return nil, "", err
	}
	for _, component := range components[:len(components)-1] {
		info, err := dir.Lstat(component)
		if err != nil {
			dir.Close()
			return nil, "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			dir.Close()
			return nil, "", fmt.Errorf("artifact path must not contain symbolic links: %s", rel)
		}
		if !info.IsDir() {
			dir.Close()
			return nil, "", fmt.Errorf("artifact parent must be a directory: %s", rel)
		}
		next, err := dir.OpenRoot(component)
		if err != nil {
			dir.Close()
			return nil, "", err
		}
		opened, err := next.Stat(".")
		if err != nil || !os.SameFile(info, opened) {
			next.Close()
			dir.Close()
			if err != nil {
				return nil, "", err
			}
			return nil, "", fmt.Errorf("artifact parent changed during open: %s", rel)
		}
		if err := dir.Close(); err != nil {
			next.Close()
			return nil, "", err
		}
		dir = next
	}
	return dir, components[len(components)-1], nil
}

var (
	taskHeading  = regexp.MustCompile(`^#{1,6}\s+Task\s+(\d+)\b`)
	specHeader   = regexp.MustCompile(`^(?:\*\*Spec:\*\*|Spec:)\s*(.*?)\s*$`)
	markdownLink = regexp.MustCompile(`^\[[^\]]*\]\(([^\s)]+)\)$`)
)

// proseLines excludes fenced examples, which cannot declare metadata or tasks.
func proseLines(content string, visit func(int, string) bool) {
	var fence byte
	var width int
	for index, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			char := trimmed[0]
			count := len(trimmed) - len(strings.TrimLeft(trimmed, string(char)))
			if fence == 0 {
				fence, width = char, count
			} else if char == fence && count >= width && strings.TrimSpace(trimmed[count:]) == "" {
				fence = 0
			}
			continue
		}
		if fence == 0 && !visit(index+1, trimmed) {
			return
		}
	}
}

func specReference(content string) (string, error) {
	var refs []string
	proseLines(content, func(_ int, line string) bool {
		if taskHeading.MatchString(line) {
			return false
		}
		if match := specHeader.FindStringSubmatch(line); match != nil {
			refs = append(refs, match[1])
		}
		return true
	})
	if len(refs) == 0 {
		return "", nil
	}
	if len(refs) != 1 {
		return "", fmt.Errorf("plan must have a single Spec reference")
	}
	ref := refs[0]
	if match := markdownLink.FindStringSubmatch(ref); match != nil {
		ref = match[1]
	} else if strings.HasPrefix(ref, "`") && strings.HasSuffix(ref, "`") {
		ref = strings.Trim(ref, "`")
	}
	parsed, err := url.Parse(ref)
	if ref == "" || err != nil || strings.ContainsAny(ref, "\r\n`[]") || strings.Contains(ref, "://") || (parsed != nil && parsed.Scheme != "" && !filepath.IsAbs(ref)) {
		return "", fmt.Errorf("spec reference must be a local file path")
	}
	return ref, nil
}
