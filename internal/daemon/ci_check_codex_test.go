package daemon

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCICheckCodexResolution(t *testing.T) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("requires linux/amd64 executable fixtures")
	}
	for _, name := range []string{"native", "npm", "mise npm", "mise", "unsupported"} {
		t.Run(name, func(t *testing.T) {
			tools := t.TempDir()
			installed := filepath.Join(tools, "installed", "codex")
			require.NoError(t, os.Mkdir(filepath.Dir(installed), 0o700))
			require.NoError(t, os.WriteFile(installed, []byte("\x7fELFnative fixture"), 0o700))
			command := filepath.Join(tools, "codex")
			switch name {
			case "npm", "mise npm":
				scope := filepath.Join(tools, "node_modules", "@openai")
				if name == "mise npm" {
					scope = filepath.Join(tools, "node_modules", ".mise", "@openai+codex@1.0.0", "node_modules", "@openai")
					link := filepath.Join(tools, "node_modules", "@openai", "codex")
					require.NoError(t, os.MkdirAll(filepath.Dir(link), 0o700))
					require.NoError(t, os.Symlink(filepath.Join(scope, "codex"), link))
					command = filepath.Join(tools, "node_modules", ".bin", "codex")
					require.NoError(t, os.MkdirAll(filepath.Dir(command), 0o700))
				}
				launcher := filepath.Join(scope, "codex", "bin", "codex.js")
				native := filepath.Join(scope, "codex-linux-x64", "vendor", "x86_64-unknown-linux-musl", "bin", "codex")
				require.NoError(t, os.MkdirAll(filepath.Dir(launcher), 0o700))
				require.NoError(t, os.MkdirAll(filepath.Dir(native), 0o700))
				require.NoError(t, os.Rename(installed, native))
				require.NoError(t, os.WriteFile(launcher, []byte("#!/usr/bin/env node\n"), 0o700))
				if name == "mise npm" {
					require.NoError(t, os.WriteFile(command, []byte("#!/bin/sh\nbasedir=$(dirname \"$0\")\nexec node \"$basedir/../@openai/codex/bin/codex.js\" \"$@\"\n"), 0o700))
				} else {
					require.NoError(t, os.Symlink(launcher, command))
				}
				installed = native
			case "mise":
				script := "#!/bin/sh\n[ \"$1 $2\" = 'which codex' ] || exit 1\n[ \"$PWD\" = '" + tools + "' ] || exit 3\nprintf '%s\\n' '" + installed + "'\n"
				require.NoError(t, os.WriteFile(filepath.Join(tools, "mise"), []byte(script), 0o700))
				require.NoError(t, os.Symlink("mise", command))
			default:
				require.NoError(t, os.Symlink(installed, command))
				if name == "unsupported" {
					require.NoError(t, os.WriteFile(installed, []byte("#!/bin/sh\n"), 0o700))
				}
			}
			got, err := resolveCICheckCodex(t.Context(), tools, command)
			if name == "unsupported" {
				require.ErrorContains(t, err, "unsupported checker Codex executable")
				return
			}
			require.NoError(t, err)
			if name == "native" {
				assert.Equal(t, command, got.Command)
			} else {
				assert.Equal(t, installed, got.Command)
			}
			assert.Contains(t, got.ReadPaths, filepath.Dir(installed))
			assert.NotContains(t, got.ReadPaths, tools)
		})
	}
}
