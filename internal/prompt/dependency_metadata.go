package prompt

import (
	"path/filepath"
	"slices"
	"strings"
)

var dependencyMetadataNames = map[string]struct{}{
	"package.json":       {},
	"package-lock.json":  {},
	"yarn.lock":          {},
	"pnpm-lock.yaml":     {},
	"bun.lockb":          {},
	"bun.lock":           {},
	"go.mod":             {},
	"go.sum":             {},
	"pyproject.toml":     {},
	"requirements.txt":   {},
	"uv.lock":            {},
	"poetry.lock":        {},
	"Pipfile.lock":       {},
	"pdm.lock":           {},
	"Cargo.toml":         {},
	"Cargo.lock":         {},
	"cargo.lock":         {},
	"Gemfile":            {},
	"Gemfile.lock":       {},
	"composer.json":      {},
	"composer.lock":      {},
	"packages.lock.json": {},
	"pubspec.yaml":       {},
	"pubspec.lock":       {},
	"mix.exs":            {},
	"mix.lock":           {},
	"Package.swift":      {},
	"Package.resolved":   {},
	"Podfile":            {},
	"Podfile.lock":       {},
	"flake.nix":          {},
	"flake.lock":         {},
	"pom.xml":            {},
	"build.gradle":       {},
	"build.gradle.kts":   {},
	"gradle.lockfile":    {},
	"requirements.in":    {},
	"setup.py":           {},
	"setup.cfg":          {},
	"environment.yml":    {},
	"environment.yaml":   {},
	"conda-lock.yml":     {},
	"conda-lock.yaml":    {},
}

func buildDependencyMetadataSection(files []string) string {
	metadata := changedDependencyMetadataFiles(files)
	if len(metadata) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("## Dependency Metadata\n\n")
	b.WriteString("Lockfile and checksum bodies may be omitted from the main diff to keep reviews focused. Use this file list as dependency-consistency evidence; do not infer that a companion file is missing just because its body is omitted.\n\n")
	b.WriteString("Changed dependency metadata:\n")
	for _, file := range metadata {
		b.WriteString("- ")
		b.WriteString(dependencyMetadataLine(file, metadata))
		b.WriteString("\n")
	}
	b.WriteString("\n")
	return b.String()
}

// HasDependencyMetadataFiles reports whether files contains dependency metadata
// that buildDependencyMetadataSection will summarize.
func HasDependencyMetadataFiles(files []string) bool {
	return len(changedDependencyMetadataFiles(files)) > 0
}

func changedDependencyMetadataFiles(files []string) []string {
	seen := make(map[string]struct{}, len(files))
	for _, file := range files {
		file = strings.TrimSpace(filepath.ToSlash(file))
		if file == "" {
			continue
		}
		if _, ok := dependencyMetadataNames[filepath.Base(file)]; ok {
			seen[file] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for file := range seen {
		out = append(out, file)
	}
	slices.Sort(out)
	return out
}

func dependencyMetadataLine(file string, metadata []string) string {
	switch filepath.Base(file) {
	case "package.json":
		if !anyFileChanged(metadata, "package-lock.json", "yarn.lock", "pnpm-lock.yaml", "bun.lockb", "bun.lock") {
			return file + " changed; no JavaScript lockfile change detected"
		}
	case "go.mod":
		if !anyCompanionChanged(file, metadata, "go.sum") {
			return file + " changed; no go.sum change detected"
		}
	}
	return file + " changed"
}

func anyFileChanged(metadata []string, names ...string) bool {
	for _, candidate := range metadata {
		if slices.Contains(names, filepath.Base(candidate)) {
			return true
		}
	}
	return false
}

func anyCompanionChanged(file string, metadata []string, names ...string) bool {
	dir := filepath.Dir(file)
	for _, candidate := range metadata {
		if filepath.Dir(candidate) != dir {
			continue
		}
		if slices.Contains(names, filepath.Base(candidate)) {
			return true
		}
	}
	return false
}
