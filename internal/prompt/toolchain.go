package prompt

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/mod/modfile"

	"go.kenn.io/roborev/internal/git"
)

// buildToolchainSection states the Go versions of modules containing changed Go
// files. An empty ref reads dirty manifests from the working tree.
func buildToolchainSection(repoPath, ref string, changedFiles []string, neutral bool) string {
	return buildToolchainSectionCtx(context.Background(), repoPath, ref, changedFiles, neutral)
}

func buildToolchainSectionCtx(ctx context.Context, repoPath, ref string, changedFiles []string, neutral bool) string {
	if _, end, ok := git.ParseRange(ref); ok {
		ref = strings.TrimPrefix(end, ".") // Three-dot ranges also use the right endpoint.
		if ref == "" {
			ref = "HEAD"
		}
	}
	type moduleVersion struct {
		found   bool
		version string
	}
	cache := make(map[string]moduleVersion)
	versions := make(map[string]string)
	for _, file := range changedFiles {
		file = filepath.ToSlash(file)
		if path.Base(file) != "go.mod" && !strings.HasSuffix(file, ".go") {
			continue
		}
		for dir := path.Dir(file); ; dir = path.Dir(dir) {
			manifest := path.Join(dir, "go.mod")
			module, cached := cache[manifest]
			if !cached {
				var data []byte
				var err error
				if ref == "" {
					data, err = os.ReadFile(filepath.Join(repoPath, filepath.FromSlash(manifest)))
					module.found = !errors.Is(err, os.ErrNotExist)
				} else {
					data, err = git.ReadFileCtx(ctx, repoPath, ref, manifest)
					module.found = err == nil
				}
				if err == nil {
					if parsed, err := modfile.Parse(manifest, data, nil); err == nil && parsed.Go != nil {
						module.version = parsed.Go.Version
					}
				}
				cache[manifest] = module
			}
			if module.found {
				if module.version != "" {
					versions[manifest] = module.version
				}
				// A module without a usable Go directive still owns its files.
				break
			}
			if dir == "." {
				break
			}
		}
	}
	if len(versions) == 0 {
		return ""
	}
	var section strings.Builder
	if neutral {
		section.WriteString("## Format and Version Context\n\nDeclared language versions in the reviewed material (use where relevant to the rubric):\n\n")
	} else {
		section.WriteString("## Toolchain\n\nJudge version-gated language and standard-library features against the owning module's `go` directive:\n\n")
	}
	for _, manifest := range slices.Sorted(maps.Keys(versions)) {
		fmt.Fprintf(&section, "- `%s`: Go %s\n", manifest, versions[manifest])
	}
	section.WriteString("\n")
	return section.String()
}
