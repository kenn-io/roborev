package review

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"go/build"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

// Embedding obeys the Go overlay used by the compiler. This binds the actual
// compiled helpers, rather than trusting unrelated files at HARNESS_DIR.
//
//go:embed quality_eval*_test.go
var qualityCompiledSources embed.FS

func qualityCompiledHarnessHash() (string, error) {
	names, err := fs.Glob(qualityCompiledSources, "quality_eval*_test.go")
	if err != nil {
		return "", err
	}
	var content []byte
	for _, name := range names {
		data, err := qualityCompiledSources.ReadFile(name)
		if err != nil {
			return "", err
		}
		content = append(content, []byte(name+"\x00")...)
		content = append(content, data...)
	}
	return qualityHash(content), nil
}

func qualityCheckHarness(root string) error {
	names, err := filepath.Glob(filepath.Join(root, "internal", "review", "quality_eval*_test.go"))
	if err != nil {
		return err
	}
	var content []byte
	for _, name := range names {
		data, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		content = append(content, []byte(filepath.Base(name)+"\x00")...)
		content = append(content, data...)
	}
	compiled, err := qualityCompiledHarnessHash()
	if err != nil {
		return err
	}
	if qualityHash(content) != compiled {
		return fmt.Errorf("declared harness bytes differ from the helpers compiled into this test binary")
	}
	return nil
}

type qualityModuleInput struct {
	Path, Version, GoMod, Dir, Sum string
	Main                           bool
	Replace                        *qualityModuleInput
}
type qualityPackageInput struct {
	EmbedPatterns                                                                                                       []string
	Dir, ImportPath, Name                                                                                               string
	Standard                                                                                                            bool
	Module                                                                                                              *qualityModuleInput
	GoFiles, CgoFiles, CFiles, CXXFiles, MFiles, HFiles, FFiles, SFiles, SwigFiles, SwigCXXFiles, SysoFiles, EmbedFiles []string
}
type qualitySourceInputs struct {
	Hashes    map[string]string
	Modules   map[string]string
	GoVersion string
	// Files and SnapshotData retain selected bytes for validation and snapshot
	// creation. CompilerPaths contains local compiler inputs that Go overlays can
	// replace; module cache inputs are copied into a private GOMODCACHE snapshot.
	ModuleCacheRoot string
	ModuleCacheDirs map[string]string
	Files           map[string]string
	SnapshotData    map[string][]byte
	CompilerPaths   map[string]string
	LocalFiles      map[string][]string
	PackagePatterns map[string]map[string][]string
	NativePackages  map[string]map[string]bool
}

func qualityIsWithin(root, name string) bool {
	_, err := qualityRelativePath(root, name)
	return err == nil
}

func qualityResolvedPath(name string) (string, error) {
	absolute, err := filepath.Abs(name)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err == nil {
		return resolved, nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(absolute))
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, filepath.Base(absolute)), nil
}

func qualityRelativePath(root, name string) (string, error) {
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	name, err = qualityResolvedPath(name)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, name)
	if err != nil {
		return "", err
	}
	if !filepath.IsLocal(rel) {
		return "", errors.New("path is outside source root")
	}
	return rel, nil
}

func qualityReadOverlay(root, harness, filename string) (map[string]string, error) {
	replacements := map[string]string{}
	if filename == "" {
		return replacements, nil
	}
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, err
	}
	var overlay struct{ Replace map[string]string }
	if err := json.Unmarshal(data, &overlay); err != nil {
		return nil, err
	}
	for target, source := range overlay.Replace {
		rel, err := filepath.Rel(filepath.Join(root, "internal", "review"), target)
		if err != nil || filepath.Dir(rel) != "." || !strings.HasPrefix(rel, "quality_eval") || !strings.HasSuffix(rel, "_test.go") || source == "" || !qualityIsWithin(harness, source) {
			return nil, fmt.Errorf("only the explicit virtual quality test harness may be overlaid: %q", target)
		}
		replacements[filepath.Clean(target)] = source
	}
	return replacements, nil
}

// qualityCollectSourceInputs uses Go's actual dependency selection, including
// test dependencies and resolved embed inputs. It never walks unrelated packages
// or Hegel/cache files. Module-cache dependencies bind by version and content;
// main/local-replacement Git inputs must also be tracked and clean.
func qualityCollectSourceInputs(ctx context.Context, root, overlayFile, modfile, harness string) (qualitySourceInputs, error) {
	got := qualitySourceInputs{Hashes: map[string]string{}, Modules: map[string]string{}, GoVersion: runtime.Version(), Files: map[string]string{}, SnapshotData: map[string][]byte{}, CompilerPaths: map[string]string{}, ModuleCacheDirs: map[string]string{}, LocalFiles: map[string][]string{}, PackagePatterns: map[string]map[string][]string{}, NativePackages: map[string]map[string]bool{}}
	moduleCache, err := qualityCommand(ctx, root, "go", "env", "GOMODCACHE")
	if err != nil {
		return got, fmt.Errorf("resolve Go module cache: %w", err)
	}
	got.ModuleCacheRoot, err = filepath.Abs(moduleCache)
	if err != nil {
		return got, err
	}
	overlay, err := qualityReadOverlay(root, harness, overlayFile)
	if err != nil {
		return got, err
	}
	args := []string{"list", "-deps", "-test", "-json", "-tags=qualityeval", "-mod=readonly"}
	if overlayFile != "" {
		args = append(args, "-overlay="+overlayFile)
	}
	if modfile != "" {
		args = append(args, "-modfile="+modfile)
	}
	args = append(args, "./internal/review")
	settings, err := qualityCurrentBuildSettings()
	if err != nil {
		return got, err
	}
	goEnv := append(qualityOfflineEnv(os.Environ()), "GOTOOLCHAIN=local", "GOPROXY=off", "GOSUMDB=off")
	goEnv = append(goEnv, qualitySelectionEnv(settings)...)
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = root
	cmd.Env = goEnv
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		return got, fmt.Errorf("list linked inputs: %w: %s", err, stderr.String())
	}
	decoder := json.NewDecoder(bytes.NewReader(output))
	localRoots := map[string]string{}
	localGitRoot := func(module *qualityModuleInput) (string, error) {
		if cached, ok := localRoots[module.Dir]; ok {
			return cached, nil
		}
		cached, err := qualityCommand(ctx, module.Dir, "git", "rev-parse", "--show-toplevel")
		if err != nil {
			return "", fmt.Errorf("resolve Git root for local replacement %q: %w", module.Path, err)
		}
		if cached == "" {
			return "", fmt.Errorf("resolve Git root for local replacement %q: empty repository root", module.Path)
		}
		localRoots[module.Dir] = cached
		return cached, nil
	}
	add := func(key, physical, gitRoot string, compilerInput bool) error {
		compilerPath := filepath.Clean(physical)
		if replacement, ok := overlay[compilerPath]; ok {
			physical = replacement
			gitRoot = harness
		}
		if previous, ok := got.Files[key]; ok && previous == physical {
			if compilerInput && gitRoot != "" {
				got.CompilerPaths[key] = compilerPath
			}
			return nil
		}
		data, err := os.ReadFile(physical)
		if err != nil {
			return err
		}
		got.Hashes[key] = qualityHash(data)
		got.Files[key] = physical
		got.SnapshotData[key] = data
		if compilerInput && gitRoot != "" {
			got.CompilerPaths[key] = compilerPath
		}
		if gitRoot != "" {
			rel, err := qualityRelativePath(gitRoot, physical)
			if err != nil {
				return fmt.Errorf("linked input outside declared source: %s", key)
			}
			got.LocalFiles[gitRoot] = append(got.LocalFiles[gitRoot], filepath.ToSlash(rel))
		}
		return nil
	}
	for {
		var pkg qualityPackageInput
		if err := decoder.Decode(&pkg); err == io.EOF {
			break
		} else if err != nil {
			return got, err
		}
		// Go emits the generated test main as a synthetic package in the build
		// cache. It is not an editable module input.
		if pkg.Name == "main" && strings.HasSuffix(pkg.ImportPath, ".test") {
			continue
		}
		if pkg.Standard || pkg.Module == nil {
			continue
		}
		module := pkg.Module
		selected := module
		if module.Replace != nil {
			selected = module.Replace
		}
		moduleKey := "module:" + module.Path + "@" + module.Version
		identity := module.Version
		if module.Sum != "" {
			identity += " " + module.Sum
		}
		if module.Replace != nil {
			identity += " => " + selected.Path + "@" + selected.Version
		}
		got.Modules[module.Path] = identity
		if selected.Version != "" && selected.Dir != "" && qualityIsWithin(got.ModuleCacheRoot, selected.Dir) {
			got.ModuleCacheDirs[selected.Path+"@"+selected.Version] = selected.Dir
		}
		gitRoot := ""
		if module.Main {
			gitRoot = root
		} else if module.Replace != nil && selected.Version == "" {
			gitRoot, err = localGitRoot(selected)
			if err != nil {
				return got, err
			}
		}
		if module.Main {
			moduleKey = ""
		}
		if gitRoot != "" {
			rel, err := qualityRelativePath(gitRoot, pkg.Dir)
			if err != nil {
				return got, fmt.Errorf("package directory outside declared source for %q", module.Path)
			}
			if got.PackagePatterns[gitRoot] == nil {
				got.PackagePatterns[gitRoot] = map[string][]string{}
			}
			got.PackagePatterns[gitRoot][filepath.ToSlash(rel)] = append(got.PackagePatterns[gitRoot][filepath.ToSlash(rel)], append([]string{"*.go"}, pkg.EmbedPatterns...)...)
			if got.NativePackages[gitRoot] == nil {
				got.NativePackages[gitRoot] = map[string]bool{}
			}
			got.NativePackages[gitRoot][filepath.ToSlash(rel)] = len(pkg.CgoFiles) > 0 || len(pkg.SwigFiles) > 0 || len(pkg.SwigCXXFiles) > 0
		}
		keyFor := func(physical string) (string, error) {
			rel, err := qualityRelativePath(selected.Dir, physical)
			if err != nil {
				return "", err
			}
			if moduleKey == "" {
				return filepath.ToSlash(rel), nil
			}
			return moduleKey + "/" + filepath.ToSlash(rel), nil
		}
		files := [][]string{pkg.GoFiles, pkg.CgoFiles, pkg.CFiles, pkg.CXXFiles, pkg.MFiles, pkg.HFiles, pkg.FFiles, pkg.SFiles, pkg.SwigFiles, pkg.SwigCXXFiles, pkg.SysoFiles, pkg.EmbedFiles}
		for _, names := range files {
			for _, name := range names {
				physical := name
				if !filepath.IsAbs(physical) {
					physical = filepath.Join(pkg.Dir, name)
				}
				if !qualityIsWithin(selected.Dir, physical) {
					return got, fmt.Errorf("selected source outside module %q", module.Path)
				}
				key, err := keyFor(physical)
				if err != nil {
					return got, fmt.Errorf("resolve selected input: %w", err)
				}
				if err := add(key, physical, gitRoot, true); err != nil {
					return got, err
				}
			}
		}
		metadataNames := []string{selected.GoMod, filepath.Join(selected.Dir, "go.sum")}
		if module.Main && modfile != "" {
			metadataNames = append(metadataNames, modfile, strings.TrimSuffix(modfile, ".mod")+".sum")
		}
		for _, metadata := range metadataNames {
			if metadata == "" {
				continue
			}
			metadataRoot := gitRoot
			if modfile != "" && (filepath.Clean(metadata) == filepath.Clean(modfile) || filepath.Clean(metadata) == filepath.Clean(strings.TrimSuffix(modfile, ".mod")+".sum")) {
				metadataRoot = harness
			}
			if metadataRoot != "" {
				rel, err := qualityRelativePath(metadataRoot, metadata)
				if err == nil {
					dir := filepath.ToSlash(filepath.Dir(rel))
					if got.PackagePatterns[metadataRoot] == nil {
						got.PackagePatterns[metadataRoot] = map[string][]string{}
					}
					got.PackagePatterns[metadataRoot][dir] = append(got.PackagePatterns[metadataRoot][dir], filepath.Base(rel))
				}
			}
			if _, err := os.Stat(metadata); os.IsNotExist(err) {
				continue
			} else if err != nil {
				return got, err
			}
			key := moduleKey + "/" + filepath.Base(metadata)
			if module.Main {
				key = filepath.Base(metadata)
			}
			if module.Main && modfile != "" && filepath.Clean(metadata) == filepath.Clean(modfile) {
				key = "selected-go.mod"
			}
			if module.Main && modfile != "" && filepath.Clean(metadata) == filepath.Clean(strings.TrimSuffix(modfile, ".mod")+".sum") {
				key = "selected-go.sum"
			}
			if err := add(key, metadata, metadataRoot, false); err != nil {
				return got, err
			}
		}
	}
	moduleArgs := []string{"list", "-m", "-json", "-mod=readonly"}
	if modfile != "" {
		moduleArgs = append(moduleArgs, "-modfile="+modfile)
	}
	moduleArgs = append(moduleArgs, "all")
	moduleCmd := exec.CommandContext(ctx, "go", moduleArgs...)
	moduleCmd.Dir = root
	moduleCmd.Env = goEnv
	var moduleStderr bytes.Buffer
	moduleCmd.Stderr = &moduleStderr
	moduleOutput, err := moduleCmd.Output()
	if err != nil {
		return got, fmt.Errorf("list selected modules: %w: %s", err, moduleStderr.String())
	}
	moduleDecoder := json.NewDecoder(bytes.NewReader(moduleOutput))
	for {
		var module qualityModuleInput
		if err := moduleDecoder.Decode(&module); err == io.EOF {
			break
		} else if err != nil {
			return got, err
		}
		selected := module
		if module.Replace != nil {
			selected = *module.Replace
		}
		identity := module.Version
		if module.Sum != "" {
			identity += " " + module.Sum
		}
		if module.Replace != nil {
			identity += " => " + selected.Path + "@" + selected.Version
		}
		got.Modules[module.Path] = identity
		if selected.Version != "" && selected.Dir != "" && qualityIsWithin(got.ModuleCacheRoot, selected.Dir) {
			got.ModuleCacheDirs[selected.Path+"@"+selected.Version] = selected.Dir
		}
		metadata := selected.GoMod
		metadataRoot := ""
		if module.Main {
			metadataRoot = root
			if modfile != "" {
				metadata, metadataRoot = modfile, harness
			}
		} else if module.Replace != nil && selected.Version == "" {
			metadataRoot, err = localGitRoot(&selected)
			if err != nil {
				return got, err
			}
		}
		if metadata == "" {
			continue
		}
		key := "module:" + module.Path + "@" + module.Version + "/go.mod"
		if module.Main && modfile == "" {
			key = "go.mod"
		} else if module.Main {
			key = "selected-go.mod"
		}
		if err := add(key, metadata, metadataRoot, false); err != nil {
			return got, err
		}
	}
	return got, nil
}

func qualityValidateSourceInputs(ctx context.Context, inputs qualitySourceInputs) error {
	for root, names := range inputs.LocalFiles {
		slices.Sort(names)
		names = slices.Compact(names)
		// A selected input must be committed. This includes ignored/untracked files
		// that Go actually selected, without rejecting nonlinked cache metadata.
		out, err := qualityCommand(ctx, root, "git", "ls-files", "-z")
		if err != nil {
			return err
		}
		tracked := map[string]bool{}
		for name := range strings.SplitSeq(out, "\x00") {
			tracked[name] = true
		}
		for _, name := range names {
			if !tracked[name] {
				return fmt.Errorf("uncommitted linked source %q", name)
			}
		}
		changed, err := qualityCommand(ctx, root, "git", "diff", "--name-only", "--no-renames", "-z", "HEAD")
		if err != nil {
			return err
		}
		selected := map[string]bool{}
		for _, name := range names {
			selected[name] = true
		}
		for key, physical := range inputs.Files {
			rel, err := qualityRelativePath(root, physical)
			if err != nil || !selected[filepath.ToSlash(rel)] {
				continue
			}
			current, err := os.ReadFile(physical)
			if err != nil {
				return fmt.Errorf("read linked input after inventory %q: %w", rel, err)
			}
			if qualityHash(current) != inputs.Hashes[key] {
				return fmt.Errorf("linked input changed after inventory: %q", rel)
			}
		}
		for name := range strings.SplitSeq(changed, "\x00") {
			if selected[name] {
				return fmt.Errorf("linked source differs from recorded HEAD: %q", name)
			}
			for dir, patterns := range inputs.PackagePatterns[root] {
				rel, err := filepath.Rel(filepath.Join(root, dir), filepath.Join(root, name))
				if err != nil || !filepath.IsLocal(rel) {
					continue
				}
				rel = filepath.ToSlash(rel)
				if filepath.Dir(rel) == "." {
					selectedNative, err := qualityTrackedNativeInput(ctx, root, name, inputs.NativePackages[root][dir])
					if err != nil {
						return err
					}
					if selectedNative {
						return fmt.Errorf("changed or removed linked native input %q", name)
					}
				}
				for _, pattern := range patterns {
					candidate := rel
					for candidate != "." {
						if match, _ := path.Match(strings.TrimPrefix(pattern, "all:"), candidate); match {
							return fmt.Errorf("changed linked source or embedded input %q", name)
						}
						if pattern == "*.go" {
							break
						}
						candidate = path.Dir(candidate)
					}
				}
			}
		}

	}
	return nil
}

// MatchFile implements Go's filename, OS/architecture and build-constraint
// selection against the tracked original, which is absent from current go list
// after a deletion/move. The native extension families follow go/build's
// fileListForExt; uppercase assembly belongs to cgo packages only.
func qualityTrackedNativeInput(ctx context.Context, root, name string, cgo bool) (bool, error) {
	switch filepath.Ext(name) {
	// A SWIG file itself establishes native capability; the current package
	// loses that signal when its last SWIG input is removed.
	case ".s", ".syso", ".swig", ".swigcxx":
	case ".c", ".cc", ".cpp", ".cxx", ".m", ".h", ".hh", ".hpp", ".hxx", ".f", ".F", ".for", ".f90", ".S", ".sx":
		if !cgo {
			return false, nil
		}
	default:
		return false, nil
	}
	// Only HEAD inputs can have disappeared from the current selected list.
	// A newly staged excluded file has no historical bytes; actual selected
	// additions are rejected separately against the current dependency list.
	entry, err := qualityCommand(ctx, root, "git", "--literal-pathspecs", "ls-tree", "-z", "HEAD", "--", name)
	if err != nil {
		return false, fmt.Errorf("read historical native entry: %w", err)
	}
	if entry == "" {
		return false, nil
	}
	cmd := exec.CommandContext(ctx, "git", "show", "HEAD:"+name)
	cmd.Dir = root
	data, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("read tracked native input: %w", err)
	}
	selection := build.Default
	selection.BuildTags = []string{"qualityeval"}
	selection.OpenFile = func(string) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }
	return selection.MatchFile(filepath.Join(root, filepath.Dir(name)), filepath.Base(name))
}
