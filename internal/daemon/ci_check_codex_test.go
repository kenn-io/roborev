package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func installCICheckNPM(t *testing.T, codex string) (string, string) {
	t.Helper()
	prefix := t.TempDir()
	packageDir := filepath.Join(prefix, "lib", "node_modules", "@openai", "codex")
	arch := runtime.GOARCH
	if arch == "amd64" {
		arch = "x64"
	}
	nativePackage := filepath.Join(filepath.Dir(packageDir), "codex-"+runtime.GOOS+"-"+arch)
	native := filepath.Join(nativePackage, "vendor", "x86_64-unknown-linux-musl", "bin", "codex")
	require.NoError(t, os.MkdirAll(filepath.Dir(native), 0o700))
	copyCmd := exec.CommandContext(t.Context(), "cp", codex, native)
	output, err := copyCmd.CombinedOutput()
	require.NoError(t, err, "%s", output)
	require.NoError(t, os.WriteFile(filepath.Join(nativePackage, "package.json"), []byte(`{"name":"@openai/codex-linux-x64"}`), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(packageDir, "bin"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(packageDir, "package.json"), []byte(`{"type":"module"}`), 0o600))
	// Match the npm launcher's sibling-package resolution at rust-v0.164.0.
	launcher := `#!/usr/bin/env node
import { createRequire } from 'node:module';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
const require = createRequire(import.meta.url);
const pkg = require.resolve('@openai/codex-' + process.platform + '-' + process.arch + '/package.json');
const native = path.join(path.dirname(pkg), 'vendor', 'x86_64-unknown-linux-musl', 'bin', 'codex');
const child = spawnSync(native, process.argv.slice(2), { stdio: 'inherit' });
if (child.error) throw child.error;
process.exit(child.status ?? 1);
`
	require.NoError(t, os.WriteFile(filepath.Join(packageDir, "bin", "codex.js"), []byte(launcher), 0o700))
	bin := filepath.Join(prefix, "bin")
	require.NoError(t, os.Mkdir(bin, 0o700))
	require.NoError(t, os.Symlink(filepath.Join(packageDir, "bin", "codex.js"), filepath.Join(bin, "codex")))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return packageDir, nativePackage
}

func TestCICheckCodexResolution(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires shell fixtures and symlinks")
	}
	for _, name := range []string{"native", "launcher", "npm", "mise"} {
		t.Run(name, func(t *testing.T) {
			tools := t.TempDir()
			installed := filepath.Join(tools, "installed", "codex")
			require.NoError(t, os.Mkdir(filepath.Dir(installed), 0o700))
			require.NoError(t, os.WriteFile(installed, []byte("native fixture"), 0o700))
			command := installed
			wantPaths := []string{filepath.Dir(installed)}
			wantPathDir := ""
			switch name {
			case "launcher":
				installed = filepath.Join(tools, "installed", "launcher")
				require.NoError(t, os.WriteFile(installed, []byte("#!/bin/sh\n[ \"${0##*/}\" = codex ] || exit 2\n"), 0o700))
				command = filepath.Join(tools, "codex")
				require.NoError(t, os.Symlink(installed, command))
				require.NoError(t, os.WriteFile(filepath.Join(tools, "node"), []byte("native node fixture"), 0o700))
				t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
				wantPaths = append(wantPaths, tools)
				wantPathDir = tools
			case "npm":
				node, err := resolveCICheckCommand(t.Context(), tools, "node")
				if err != nil {
					t.Skip("requires node")
				}
				nodeReal, err := filepath.EvalSymlinks(node)
				require.NoError(t, err)
				// env node must find the invocation name even when its target differs.
				require.NoError(t, os.Symlink(nodeReal, filepath.Join(tools, "node")))
				t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
				pkg, native := installCICheckNPM(t, installed)
				command, err = exec.LookPath("codex")
				require.NoError(t, err)
				wantPaths = []string{pkg, native, filepath.Dir(nodeReal)}
				wantPathDir = tools
			case "mise":
				script := "#!/bin/sh\n[ \"$1 $2\" = 'which codex' ] || exit 1\n[ \"$PWD\" = '" + tools + "' ] || exit 3\nprintf '%s\\n' '" + installed + "'\n"
				require.NoError(t, os.WriteFile(filepath.Join(tools, "mise"), []byte(script), 0o700))
				require.NoError(t, os.Symlink("mise", filepath.Join(tools, "codex")))
				command = filepath.Join(tools, "codex")
			}
			got, err := resolveCICheckCodex(t.Context(), tools, command)
			require.NoError(t, err)
			wantCommand := command
			if name == "mise" {
				wantCommand = installed
			}
			assert.Equal(t, wantCommand, got.Command)
			for _, path := range wantPaths {
				assert.Contains(t, got.ReadPaths, path)
			}
			assert.Equal(t, wantPathDir, got.PathDir)
			if name == "launcher" {
				output, err := exec.CommandContext(t.Context(), got.Command).CombinedOutput()
				require.NoError(t, err, "%s", output)
			}
		})
	}
}
