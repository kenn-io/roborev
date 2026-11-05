package daemon

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
	"go.kenn.io/kit/atomicfile"
	gitcmd "go.kenn.io/kit/git/cmd"
)

// Package installers see only committed metadata and private writable state.
// Their populated environments are first executed by the sandbox.
func prepareCICheckPackages(ctx context.Context, repo, root, toolPath string, gitEnv []string, out io.Writer) string {
	files, err := gitcmd.New().Command(ctx, repo, "ls-files", "-z").Output()
	if err != nil {
		fmt.Fprintf(out, "CI checker package discovery failed: %v\n", err)
		return ""
	}
	private := filepath.Join(root, "packages")
	for _, name := range []string{"home", "tmp"} {
		if err := os.MkdirAll(filepath.Join(private, name), 0o700); err != nil {
			fmt.Fprintf(out, "CI checker package staging failed: %v\n", err)
			return ""
		}
	}
	env := ciCheckPackageEnv(private, toolPath, gitEnv)
	projects := map[string]map[string][]byte{}
	for name := range strings.SplitSeq(string(files), "\x00") {
		base := filepath.Base(name)
		if !slices.Contains([]string{"package.json", "package-lock.json", "npm-shrinkwrap.json", "bun.lock", "pyproject.toml"}, base) {
			continue
		}
		path := filepath.Join(repo, name)
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		dir := filepath.Dir(name)
		if projects[dir] == nil {
			projects[dir] = map[string][]byte{}
		}
		projects[dir][base] = data
	}
	var pythonBin string
	for dir, metadata := range projects {
		stage, err := os.MkdirTemp(private, "project-*")
		if err != nil {
			fmt.Fprintf(out, "CI checker package staging failed: %v\n", err)
			continue
		}
		project := filepath.Join(repo, dir)
		if data := metadata["package.json"]; data != nil {
			prepareCICheckJS(ctx, project, stage, metadata, env, out)
		}
		if data := metadata["pyproject.toml"]; data != nil {
			if prepareCICheckPython(ctx, project, stage, data, env, out) && dir == "." {
				pythonBin = filepath.Join(project, ".venv", "bin")
			}
		}
	}
	return pythonBin
}

func ciCheckPackageEnv(private, toolPath string, gitEnv []string) []string {
	env := []string{"PATH=" + toolPath, "HOME=" + filepath.Join(private, "home"), "TMPDIR=" + filepath.Join(private, "tmp"), "LANG=C.UTF-8", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_TERMINAL_PROMPT=0", "GIT_ALLOW_PROTOCOL=https", "npm_config_ignore_scripts=true"}
	for _, entry := range gitEnv {
		if strings.HasPrefix(entry, "GIT_CONFIG_COUNT=") || strings.HasPrefix(entry, "GIT_CONFIG_KEY_") || strings.HasPrefix(entry, "GIT_CONFIG_VALUE_") {
			env = append(env, entry)
		}
	}
	return env
}

func runCICheckInstaller(ctx context.Context, command, dir string, env, args []string, out io.Writer) bool {
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir, cmd.Env = dir, env
	// Package-manager output can contain authenticated URLs. Report the failing step only.
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(out, "CI checker package preparation failed (%s %s): %v\n", filepath.Base(command), args[0], err)
		return false
	}
	return true
}

var (
	ciCheckLocalJS     = regexp.MustCompile(`(?i)^(file:|link:|workspace:|\.?\.?/|~/|[A-Z]:[\\/])`)
	ciCheckLocalJSLock = regexp.MustCompile(`(?i)(file:|link:|workspace:|"link"\s*:\s*true)`)
)

func ciCheckLocalJSDependency(value any) bool {
	switch value := value.(type) {
	case string:
		return ciCheckLocalJS.MatchString(value)
	case map[string]any:
		for _, dependency := range value {
			if ciCheckLocalJSDependency(dependency) {
				return true
			}
		}
	}
	return false
}

func prepareCICheckJS(ctx context.Context, project, stage string, metadata map[string][]byte, env []string, out io.Writer) {
	var manifest map[string]jsontext.Value
	if err := json.Unmarshal(metadata["package.json"], &manifest); err != nil {
		fmt.Fprintf(out, "CI checker invalid package.json: %v\n", err)
		return
	}
	if len(manifest["workspaces"]) > 0 {
		fmt.Fprintln(out, "CI checker skips JavaScript workspaces requiring local packages")
		return
	}
	for _, field := range []string{"dependencies", "devDependencies", "optionalDependencies", "peerDependencies", "overrides", "resolutions"} {
		if data := manifest[field]; data != nil {
			var dependencies any
			if err := json.Unmarshal(data, &dependencies); err != nil {
				fmt.Fprintf(out, "CI checker invalid dependency declarations: %v\n", err)
				return
			}
			if ciCheckLocalJSDependency(dependencies) {
				fmt.Fprintln(out, "CI checker skips JavaScript local dependencies")
				return
			}
		}
	}
	manager := "npm"
	args := []string{"install", "--ignore-scripts", "--include=dev", "--no-audit", "--no-fund"}
	if metadata["bun.lock"] != nil {
		manager = "bun"
		args = []string{"install", "--ignore-scripts", "--backend=copyfile", "--frozen-lockfile"}
	} else if metadata["package-lock.json"] != nil || metadata["npm-shrinkwrap.json"] != nil {
		args[0] = "ci"
	}
	for _, name := range []string{"package.json", "package-lock.json", "npm-shrinkwrap.json", "bun.lock"} {
		if data := metadata[name]; data != nil {
			if name != "package.json" && ciCheckLocalJSLock.Match(data) {
				fmt.Fprintln(out, "CI checker skips JavaScript local dependencies")
				return
			}
			if err := os.WriteFile(filepath.Join(stage, name), data, 0o600); err != nil {
				fmt.Fprintf(out, "CI checker package staging failed: %v\n", err)
				return
			}
		}
	}
	command, err := resolveCICheckCommand(ctx, "/", manager)
	if err != nil {
		fmt.Fprintf(out, "CI checker package tool unavailable: %s\n", manager)
		return
	}
	if !runCICheckInstaller(ctx, command, stage, env, args, out) {
		return
	}
	if err := atomicfile.RenameNoReplace(filepath.Join(stage, "node_modules"), filepath.Join(project, "node_modules")); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(out, "CI checker package handoff failed: %v\n", err)
	}
}

// Accept registry requirements only. Names, extras, versions, and markers stay intact;
// direct URLs, editable inputs, and pip options could request host source execution.
var ciCheckRegistryRequirement = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*(\[[A-Za-z0-9_,. -]+\])?\s*([<>=!~(].*)?$`)

func prepareCICheckPython(ctx context.Context, project, stage string, data []byte, env []string, out io.Writer) bool {
	venv := filepath.Join(project, ".venv")
	if _, err := os.Lstat(venv); !os.IsNotExist(err) {
		fmt.Fprintln(out, "CI checker skips existing .venv")
		return false
	}

	var manifest struct {
		Project struct {
			Dependencies         []string
			OptionalDependencies map[string][]string `toml:"optional-dependencies"`
		}
		DependencyGroups map[string][]any `toml:"dependency-groups"`
	}
	if err := toml.Unmarshal(data, &manifest); err != nil {
		fmt.Fprintf(out, "CI checker invalid pyproject.toml: %v\n", err)
		return false
	}
	requirements := slices.Clone(manifest.Project.Dependencies)
	for _, name := range slices.Sorted(maps.Keys(manifest.Project.OptionalDependencies)) {
		requirements = append(requirements, manifest.Project.OptionalDependencies[name]...)
	}
	for _, name := range slices.Sorted(maps.Keys(manifest.DependencyGroups)) {
		for _, item := range manifest.DependencyGroups[name] {
			if requirement, ok := item.(string); ok {
				requirements = append(requirements, requirement)
			}
		}
	}
	requirements = slices.DeleteFunc(requirements, func(requirement string) bool {
		declaration, _, _ := strings.Cut(requirement, ";")
		return !ciCheckRegistryRequirement.MatchString(strings.TrimSpace(declaration)) || strings.ContainsAny(declaration, "@/:\\")
	})
	if len(requirements) == 0 {
		return false
	}
	python, err := resolveCICheckCommand(ctx, "/", "python3")
	if err != nil {
		fmt.Fprintln(out, "CI checker package tool unavailable: python3")
		return false
	}
	wheels := filepath.Join(stage, "wheels")
	var accepted []string
	for _, requirement := range requirements {
		args := append([]string{"-I", "-m", "pip", "--isolated", "download", "--only-binary=:all:", "--dest", wheels}, accepted...)
		if runCICheckInstaller(ctx, python, stage, env, append(args, requirement), out) {
			accepted = append(accepted, requirement)
		}
	}
	if len(accepted) == 0 {
		return false
	}
	if !runCICheckInstaller(ctx, python, stage, env, []string{"-I", "-m", "venv", venv}, out) {
		return false
	}
	// This is the only host startup of the empty venv. Installed .pth files and
	// sitecustomize must never run on the host after pip populates it.
	args := append([]string{"-I", "-m", "pip", "--isolated", "install", "--only-binary=:all:", "--no-index", "--no-compile", "--find-links", wheels}, accepted...)
	return runCICheckInstaller(ctx, filepath.Join(venv, "bin", "python"), stage, env, args, out)
}
