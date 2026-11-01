package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCapabilityProbesRefreshAfterUpgrade(t *testing.T) {
	ctx := t.Context()
	probes := map[string]func(context.Context, string) (bool, error){
		"claude dangerous": claudeSupportsDangerousFlag,
		"claude effort": func(ctx context.Context, command string) (bool, error) {
			return claudeSupportsEffortFlag(ctx, command), nil
		},
		"claude tools": func(ctx context.Context, command string) (bool, error) {
			return claudeSupportsToolsFlag(ctx, command), nil
		},
		"claude bare": func(ctx context.Context, command string) (bool, error) {
			return claudeSupportsBareFlag(ctx, command), nil
		},
		"codex ignore config":     codexSupportsIgnoreUserConfig,
		"copilot tools":           copilotSupportsAllowAllTools,
		"copilot stream":          copilotSupportsStreamOff,
		"copilot json":            copilotSupportsJSONOutput,
		"copilot mcp":             copilotSupportsDisableBuiltInMCPs,
		"copilot available tools": copilotSupportsAvailableTools,
	}
	for _, ignoreConfig := range []bool{false, true} {
		for name, probe := range map[string]func(context.Context, string, bool) (bool, error){
			"dangerous":     codexSupportsDangerousFlag,
			"sandbox":       codexSupportsNonInteractive,
			"thread source": codexSupportsThreadSource,
		} {
			probes[fmt.Sprintf("codex %s ignore config %t", name, ignoreConfig)] = func(ctx context.Context, command string) (bool, error) {
				return probe(ctx, command, ignoreConfig)
			}
		}
	}
	for name, probe := range probes {
		t.Run(name, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "probes")
			script := fmt.Sprintf("#!/bin/sh\ncase \"$1\" in *etxtbsy*) exit 0;; esac\necho probe >> %q\necho '%%s'\n", marker)
			command := writeTempCommand(t, fmt.Sprintf(script, ""))
			for _, want := range []bool{false, true, false} {
				if want {
					flags := "--dangerously-skip-permissions --effort --tools --bare --ignore-user-config --dangerously-bypass-approvals-and-sandbox --sandbox --thread-source --allow-all-tools --stream --output-format --disable-builtin-mcps --available-tools"
					require.NoError(t, os.WriteFile(command, []byte(fmt.Sprintf(script, flags)), 0o755))
				} else {
					require.NoError(t, os.WriteFile(command, []byte(fmt.Sprintf(script, "")), 0o755))
				}
				supported, err := probe(ctx, command)
				require.NoError(t, err)
				assert.Equal(t, want, supported)
				// The unchanged executable should still use the cached result.
				supported, err = probe(ctx, command)
				require.NoError(t, err)
				assert.Equal(t, want, supported)
			}
			data, err := os.ReadFile(marker)
			require.NoError(t, err)
			assert.Equal(t, 3, strings.Count(string(data), "probe\n"))
		})
	}
}

func TestCapabilityProbeRefreshesResolvedExecutable(t *testing.T) {
	for _, change := range []string{"size", "modtime", "symlink", "PATH", "missing"} {
		t.Run(change, func(t *testing.T) {
			oldScript := "#!/bin/sh\necho '               '\n"
			newScript := "#!/bin/sh\necho '--output-format'\n"
			oldCommand := writeTempCommand(t, oldScript)
			newCommand := writeTempCommand(t, newScript)
			info, err := os.Stat(oldCommand)
			require.NoError(t, err)
			require.NoError(t, os.Chtimes(newCommand, info.ModTime(), info.ModTime()))
			command := oldCommand
			switch change {
			case "symlink":
				command = filepath.Join(t.TempDir(), "agent")
				require.NoError(t, os.Symlink(oldCommand, command))
			case "PATH":
				command = filepath.Base(oldCommand)
				t.Setenv("PATH", filepath.Dir(oldCommand))
			}
			supported, err := copilotSupportsJSONOutput(t.Context(), command)
			require.NoError(t, err)
			assert.False(t, supported)
			switch change {
			case "size":
				require.NoError(t, os.WriteFile(oldCommand, []byte(newScript+"\n"), 0o755))
				require.NoError(t, os.Chtimes(oldCommand, info.ModTime(), info.ModTime()))
			case "modtime":
				require.NoError(t, os.WriteFile(oldCommand, []byte(newScript), 0o755))
				changed := info.ModTime().Add(time.Second)
				require.NoError(t, os.Chtimes(oldCommand, changed, changed))
			case "symlink":
				require.NoError(t, os.Remove(command))
				require.NoError(t, os.Symlink(newCommand, command))
			case "PATH":
				t.Setenv("PATH", filepath.Dir(newCommand))
			case "missing":
				require.NoError(t, os.Remove(oldCommand))
				_, err := copilotSupportsJSONOutput(t.Context(), command)
				require.Error(t, err)
				return
			}
			supported, err = copilotSupportsJSONOutput(t.Context(), command)
			require.NoError(t, err)
			assert.True(t, supported)
		})
	}
}
