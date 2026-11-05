package daemon

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
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
			ancestors := map[string]any{}
			for parent := dir; parent != "."; {
				parent = filepath.Dir(parent)
				var manifest map[string]any
				if json.Unmarshal(projects[parent]["package.json"], &manifest) == nil && manifest["workspaces"] != nil {
					relative, _ := filepath.Rel(parent, dir)
					ancestors[filepath.ToSlash(relative)] = manifest["workspaces"]
				}
			}
			prepareCICheckJS(ctx, project, stage, metadata, ancestors, env, out)
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
	env := []string{"PATH=" + toolPath, "HOME=" + filepath.Join(private, "home"), "TMPDIR=" + filepath.Join(private, "tmp"), "LANG=C.UTF-8", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_TERMINAL_PROMPT=0", "GIT_ALLOW_PROTOCOL=https", "npm_config_ignore_scripts=true", "PIP_CONFIG_FILE=" + os.DevNull}
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
		fmt.Fprintf(out, "CI checker package preparation failed (%s %s): %v\n", filepath.Base(command), strings.Join(args[:min(5, len(args))], " "), err)
		return false
	}
	return true
}

// Installers use the job's selected tools rather than the daemon's PATH order.
func resolveCICheckPackageTool(ctx context.Context, env []string, name string) (string, error) {
	var path string
	for _, entry := range env {
		if value, ok := strings.CutPrefix(entry, "PATH="); ok {
			path = value
		}
	}
	for _, dir := range filepath.SplitList(path) {
		if command, err := exec.LookPath(filepath.Join(dir, name)); err == nil && filepath.IsAbs(dir) {
			return resolveCICheckCommand(ctx, "/", command)
		}
	}
	return "", exec.ErrNotFound
}

func ciCheckNpmSources(lock map[string]any) []string {
	var sources []string
	for _, field := range []string{"packages", "dependencies"} {
		packages, _ := lock[field].(map[string]any)
		for _, value := range packages {
			if pkg, ok := value.(map[string]any); ok {
				// npm's source policies skip link records, which install directories
				// regardless of the resolved path's spelling; it enforces every other source.
				if pkg["link"] == true {
					sources = append(sources, "file:.")
				}
				sources = append(sources, ciCheckNpmSources(pkg)...)
			}
		}
	}
	return sources
}

// Use npm's own source and workspace semantics from its trusted installation.
func ciCheckLocalJSSources(ctx context.Context, stage string, sources, rejected []string, ancestors map[string]any, env []string) (bool, error) {
	npm, err := resolveCICheckPackageTool(ctx, env, "npm")
	if err != nil {
		return false, err
	}
	node, err := resolveCICheckPackageTool(ctx, env, "node")
	if err != nil {
		return false, err
	}
	input, err := json.Marshal(map[string]any{"sources": sources, "rejected": rejected, "workspaces": ancestors})
	if err != nil {
		return false, err
	}
	const script = `const fs = require('node:fs'), path = require('node:path');
const npm = fs.realpathSync(process.argv[1]);
const entry = [path.resolve(path.dirname(npm), '../lib/node_modules/npm/package.json'), npm].find(fs.existsSync);
const npmRequire = require('node:module').createRequire(entry);
const npa = npmRequire('npm-package-arg');
const input = JSON.parse(fs.readFileSync(0, 'utf8'));
if (input.rejected.includes('git') && !['allow-file', 'allow-directory', 'allow-git'].every(name => npmRequire('@npmcli/config/lib/definitions').definitions[name]))
 throw new Error('npm lacks the required source fetch policies');
const member = Object.entries(input.workspaces).some(([relative, workspaces]) =>
 npmRequire('@npmcli/map-workspaces').virtual({cwd: '/', lockfile: {packages: {'': {workspaces}, [relative]: {name: 'candidate'}}}}).size > 0);
const local = input.sources.some(spec => {
 try { return input.rejected.includes(npa.resolve('checker', spec).type); }
 catch { return true; }
});
process.stdout.write(JSON.stringify(member || local));`
	parse := exec.CommandContext(ctx, node, "--input-type=commonjs", "--eval", script, npm)
	parse.Dir, parse.Env, parse.Stdin = stage, env, bytes.NewReader(input)
	output, err := parse.Output()
	if err != nil {
		return false, err
	}
	var local bool
	err = json.Unmarshal(output, &local)
	return local, err
}

func ciCheckBunSources(ctx context.Context, command, stage string, data []byte, env []string) ([]string, error) {
	// Bun's JSONC parser handles comments, trailing commas, and escaped source paths.
	parse := exec.CommandContext(ctx, command, "--no-env-file", "--eval", `process.stdout.write(JSON.stringify(Bun.JSONC.parse(await Bun.stdin.text())))`)
	parse.Dir, parse.Env, parse.Stdin = stage, env, bytes.NewReader(data)
	decoded, err := parse.Output()
	if err != nil {
		return nil, err
	}
	var lock struct {
		Packages map[string][]any `json:"packages"`
	}
	if err := json.Unmarshal(decoded, &lock); err != nil {
		return nil, err
	}
	var sources []string
	for _, tuple := range lock.Packages {
		if len(tuple) == 0 {
			continue
		}
		source, _ := tuple[0].(string)
		_, spec, _ := strings.Cut(strings.TrimPrefix(source, "@"), "@")
		sources = append(sources, spec)
	}
	return sources, nil
}

func prepareCICheckJS(ctx context.Context, project, stage string, metadata map[string][]byte, ancestors map[string]any, env []string, out io.Writer) {
	var manifest map[string]any
	if err := json.Unmarshal(metadata["package.json"], &manifest); err != nil {
		fmt.Fprintf(out, "CI checker invalid package.json: %v\n", err)
		return
	}
	if manifest["workspaces"] != nil {
		fmt.Fprintln(out, "CI checker skips JavaScript workspaces requiring local packages")
		return
	}
	sources := []string{}
	manager, lockName := "npm", "package-lock.json"
	if metadata["npm-shrinkwrap.json"] != nil {
		lockName = "npm-shrinkwrap.json"
	}
	rejected := []string{"file", "directory", "git"}
	args := []string{"install", "--ignore-scripts", "--include=dev", "--no-audit", "--no-fund", "--allow-file=none", "--allow-directory=none", "--allow-git=none"}
	if metadata["bun.lock"] != nil {
		manager, lockName = "bun", "bun.lock"
		rejected = rejected[:2]
		args = []string{"install", "--ignore-scripts", "--backend=copyfile", "--frozen-lockfile"}
	}
	command, err := resolveCICheckPackageTool(ctx, env, manager)
	if err != nil {
		fmt.Fprintf(out, "CI checker package tool unavailable: %s\n", manager)
		return
	}

	for _, name := range []string{"package.json", lockName} {
		if data := metadata[name]; data != nil {
			var locked []string
			switch name {
			case "bun.lock":
				locked, err = ciCheckBunSources(ctx, command, stage, data, env)
			case "package-lock.json", "npm-shrinkwrap.json":
				var lock map[string]any
				err = json.Unmarshal(data, &lock)
				locked = ciCheckNpmSources(lock)
			}
			if err != nil {
				fmt.Fprintf(out, "CI checker invalid %s: %v\n", name, err)
				return
			}
			sources = append(sources, locked...)
			if err := os.WriteFile(filepath.Join(stage, name), data, 0o600); err != nil {
				fmt.Fprintf(out, "CI checker package staging failed: %v\n", err)
				return
			}
		}
	}
	local, err := ciCheckLocalJSSources(ctx, stage, sources, rejected, ancestors, env)
	if err != nil {
		fmt.Fprintf(out, "CI checker source classification failed: %v\n", err)
		return
	}
	if local {
		fmt.Fprintln(out, "CI checker skips JavaScript local dependencies, Git sources, or workspace members")
		return
	}
	if !runCICheckInstaller(ctx, command, stage, env, args, out) {
		return
	}
	if err := atomicfile.RenameNoReplace(filepath.Join(stage, "node_modules"), filepath.Join(project, "node_modules")); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(out, "CI checker package handoff failed: %v\n", err)
	}
}

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
	if len(requirements) == 0 {
		return false
	}
	python, err := resolveCICheckPackageTool(ctx, env, "python3")
	if err != nil {
		fmt.Fprintln(out, "CI checker package tool unavailable: python3")
		return false
	}
	const script = `import json, sys
from pip._vendor.packaging.requirements import Requirement, InvalidRequirement
accepted = []
for index, value in enumerate(sys.argv[1:], 1):
 try:
  if Requirement(value).url is None: accepted.append(value)
 except InvalidRequirement:
  print(f"CI checker invalid requirement in pyproject.toml dependency declaration {index}: invalid requirement syntax", file=sys.stderr)
json.dump(accepted, sys.stdout)`
	parse := exec.CommandContext(ctx, python, append([]string{"-I", "-c", script}, requirements...)...)
	parse.Dir, parse.Env, parse.Stderr = stage, env, out
	data, err = parse.Output()
	if err == nil {
		err = json.Unmarshal(data, &requirements)
	}
	if err != nil {
		fmt.Fprintf(out, "CI checker requirement parsing failed: %v\n", err)
		return false
	}
	wheels := filepath.Join(stage, "wheels")
	args := []string{"-I", "-m", "pip", "--isolated", "download", "--only-binary=:all:", "--dest", wheels}
	accepted := requirements
	if !runCICheckInstaller(ctx, python, stage, env, append(slices.Clone(args), requirements...), out) {
		accepted = nil
		for _, requirement := range requirements {
			if runCICheckInstaller(ctx, python, stage, env, append(append(slices.Clone(args), accepted...), requirement), out) {
				accepted = append(accepted, requirement)
			}
		}
	}
	if len(accepted) == 0 {
		return false
	}
	// This is the only host startup of the empty venv. Installed .pth files and
	// sitecustomize must never run on the host after pip populates it.
	args = append([]string{"-I", "-m", "pip", "--isolated", "install", "--only-binary=:all:", "--no-index", "--no-compile", "--find-links", wheels}, accepted...)
	if runCICheckInstaller(ctx, python, stage, env, []string{"-I", "-m", "venv", venv}, out) &&
		runCICheckInstaller(ctx, filepath.Join(venv, "bin", "python"), stage, env, args, out) {
		return true
	}
	_ = os.RemoveAll(venv)
	return false
}
