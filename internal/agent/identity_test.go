package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
)

// writeProbeScript creates a fake CLI that answers --version / -v with
// versionLine. Positional args (version, v, or anything else) fail and,
// when marker is non-empty, append a line so tests can detect accidental
// agentic-style prompts against Cursor's documented CLI interface.
func writeProbeScript(t *testing.T, dir, name, versionLine string) string {
	t.Helper()
	return writeProbeScriptWithMarker(t, dir, name, versionLine, "")
}

func writeProbeScriptWithMarker(t *testing.T, dir, name, versionLine, marker string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if runtime.GOOS == "windows" {
		// Cursor official interface: only --version / -v. Positionals are
		// agent prompts — mark and fail so tests catch probe regressions.
		// Use parenthesized if-blocks: bare "if cond echo x & exit" leaves
		// exit outside the conditional in cmd.exe.
		//
		// Put version text in a quoted set variable so parentheses in the
		// string (e.g. "grok 0.2.118 (abc)") cannot close the if-block early.
		// Expand it after cmd.exe parses the block so metacharacters in the
		// value are emitted as text rather than reinterpreted as syntax.
		var b strings.Builder
		b.WriteString("@echo off\r\n")
		b.WriteString("setlocal EnableDelayedExpansion\r\n")
		b.WriteString("set \"PROBE_VERSION=" + versionLine + "\"\r\n")
		b.WriteString("if \"%~1\"==\"--help-probe-etxtbsy\" exit /b 0\r\n")
		b.WriteString("if \"%~1\"==\"--version\" (\r\n")
		b.WriteString("  echo(!PROBE_VERSION!\r\n")
		b.WriteString("  exit /b 0\r\n")
		b.WriteString(")\r\n")
		b.WriteString("if \"%~1\"==\"-v\" (\r\n")
		b.WriteString("  echo(!PROBE_VERSION!\r\n")
		b.WriteString("  exit /b 0\r\n")
		b.WriteString(")\r\n")
		if marker != "" {
			b.WriteString("echo positional:%~1>>\"" + marker + "\"\r\n")
		}
		b.WriteString("exit /b 1\r\n")
		path += ".bat"
		require.NoError(t, os.WriteFile(path, []byte(b.String()), 0o755))
		return path
	}

	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("case \"$1\" in\n")
	b.WriteString("  *etxtbsy*) exit 0;;\n")
	b.WriteString("  --version|-v) echo '" + versionLine + "';;\n")
	b.WriteString("  *)\n")
	if marker != "" {
		b.WriteString("    echo \"positional:$1\" >> '" + marker + "'\n")
	}
	b.WriteString("    exit 1;;\n")
	b.WriteString("esac\n")
	require.NoError(t, os.WriteFile(path, []byte(b.String()), 0o755))
	return path
}

// writeFailingProbe always exits non-zero (or hangs) so probes stay unknown.
func writeFailingProbe(t *testing.T, dir, name string, mode string) string {
	t.Helper()
	unixScripts := map[string]string{
		"empty": "#!/bin/sh\nexit 0\n",
		"error": "#!/bin/sh\nexit 1\n",
		"hang":  "#!/bin/sh\nsleep 30\n",
	}
	winScripts := map[string]string{
		// Exit 0 with no version text → treated as empty/unknown.
		"empty": "@echo off\r\nexit /b 0\r\n",
		"error": "@echo off\r\nexit /b 1\r\n",
		// Keep hang short: process-tree kill of .bat children is imperfect on
		// Windows, and a 30s orphaned ping can contend with later package tests.
		// Probe timeout under test is << 5s, so this is still a reliable hang.
		"hang": "@echo off\r\nping -n 5 127.0.0.1 >nul\r\nexit /b 0\r\n",
	}
	path := filepath.Join(dir, name)
	if runtime.GOOS == "windows" {
		path += ".bat"
		content, ok := winScripts[mode]
		require.True(t, ok, "unknown failing probe mode %q", mode)
		require.NoError(t, os.WriteFile(path, []byte(content), 0o755))
		return path
	}
	script, ok := unixScripts[mode]
	require.True(t, ok, "unknown failing probe mode %q", mode)
	require.NoError(t, os.WriteFile(path, []byte(script), 0o755))
	return path
}

func TestCommandLooksLikeGrok_SameFileSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink identity covered by version probe on Windows")
	}
	clearIdentityProbeCache()
	dir := t.TempDir()
	grok := writeProbeScript(t, dir, "grok", "grok 0.2.118 (abc) [stable]")
	agent := filepath.Join(dir, "agent")
	require.NoError(t, os.Symlink(grok, agent))

	assert.True(t, commandLooksLikeGrok(agent))
	assert.True(t, commandLooksLikeGrok(grok))
	assert.False(t, commandIsUsableCursorCandidate(agent))
	assert.Equal(t, identityGrok, resolveCommandIdentity(agent))
}

func TestCommandLooksLikeGrok_VersionProbeCopy(t *testing.T) {
	// Simulate Windows installer: separate copies that both identify as Grok
	// via --version (official install validation path).
	clearIdentityProbeCache()
	dir := t.TempDir()
	grok := writeProbeScript(t, dir, "grok", "grok 0.2.118 (abc) [stable]")
	agent := writeProbeScript(t, dir, "agent", "grok 0.2.118 (abc) [stable]")

	assert.True(t, commandLooksLikeGrok(grok))
	assert.True(t, commandLooksLikeGrok(agent), "copied agent that versions as grok must be detected")
	assert.False(t, commandIsUsableCursorCandidate(agent))
}

func TestResolveExecutableRejectsDirectory(t *testing.T) {
	_, err := resolveExecutable(t.TempDir())
	assert.Error(t, err)
}

func TestResolveExecutableRejectsNonExecutableFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows executable checks use file extensions instead of mode bits")
	}
	path := filepath.Join(t.TempDir(), "not-executable")
	require.NoError(t, os.WriteFile(path, []byte("not a command"), 0o644))

	_, err := resolveExecutable(path)
	assert.Error(t, err)
}

func TestCursorNeverReceivesPositionalProbe(t *testing.T) {
	// Cursor fixture records any positional argv. Probing identity must not
	// write the marker — only --version / -v are allowed.
	clearIdentityProbeCache()
	dir := t.TempDir()
	marker := filepath.Join(dir, "positional.log")
	cursor := writeProbeScriptWithMarker(t, dir, "agent", "cursor agent 1.2.3", marker)

	assert.True(t, commandIsUsableCursorCandidate(cursor))
	assert.False(t, commandLooksLikeGrok(cursor))

	// Sanity: official flags work; bare positionals would mark.
	out, err := exec.Command(cursor, "--version").CombinedOutput()
	require.NoError(t, err)
	assert.Contains(t, string(out), "cursor")
	_, err = exec.Command(cursor, "version").CombinedOutput()
	require.Error(t, err)
	data, err := os.ReadFile(marker)
	require.NoError(t, err)
	assert.Contains(t, string(data), "positional:version")

	// Clear marker and re-probe through identity path — must stay clean.
	require.NoError(t, os.WriteFile(marker, nil, 0o644))
	clearIdentityProbeCache()
	assert.True(t, commandIsUsableCursorCandidate(cursor))
	assert.Equal(t, identityNotGrok, resolveCommandIdentity(cursor))
	data, err = os.ReadFile(marker)
	require.NoError(t, err)
	assert.Empty(t, strings.TrimSpace(string(data)),
		"identity probe must not send positional prompts to Cursor")
}

func TestCommandIsUsableCursorCandidate_FailClosedOnUnknown(t *testing.T) {
	clearIdentityProbeCache()
	dir := t.TempDir()

	t.Run("process error", func(t *testing.T) {
		clearIdentityProbeCache()
		bin := writeFailingProbe(t, dir, "err-agent", "error")
		assert.Equal(t, identityUnknown, resolveCommandIdentity(bin))
		assert.False(t, commandIsUsableCursorCandidate(bin))
		assert.False(t, commandLooksLikeGrok(bin))
	})

	t.Run("empty version output", func(t *testing.T) {
		clearIdentityProbeCache()
		bin := writeFailingProbe(t, dir, "empty-agent", "empty")
		assert.Equal(t, identityUnknown, resolveCommandIdentity(bin))
		assert.False(t, commandIsUsableCursorCandidate(bin))
	})

	t.Run("probe timeout", func(t *testing.T) {
		clearIdentityProbeCache()
		prev := identityProbeTimeout
		identityProbeTimeout = 80 * time.Millisecond
		t.Cleanup(func() { identityProbeTimeout = prev })

		bin := writeFailingProbe(t, dir, "hang-agent", "hang")
		assert.Equal(t, identityUnknown, resolveCommandIdentity(bin))
		assert.False(t, commandIsUsableCursorCandidate(bin),
			"timeout must not classify binary as Cursor")
	})
}

func TestIdentityProbe_OrphanPipeBounded(t *testing.T) {
	// Regression for WaitDelay == 0: killing the parent while a descendant
	// keeps stdout/stderr open must not leave CombinedOutput blocked forever.
	if runtime.GOOS == "windows" {
		t.Skip("orphan pipe fixture uses a Unix process tree")
	}
	clearIdentityProbeCache()
	prevTimeout := identityProbeTimeout
	prevWait := identityProbeWaitDelay
	identityProbeTimeout = 80 * time.Millisecond
	identityProbeWaitDelay = 50 * time.Millisecond
	t.Cleanup(func() {
		identityProbeTimeout = prevTimeout
		identityProbeWaitDelay = prevWait
	})

	dir := t.TempDir()
	path := filepath.Join(dir, "hang-orphan")
	// Parent sleeps forever; background child inherits pipes. When the
	// context kills the shell, the orphan may keep pipes open — WaitDelay
	// must still unstick Wait.
	script := "#!/bin/sh\n" +
		"case \"$1\" in *etxtbsy*) exit 0;; esac\n" +
		"(sleep 60) &\n" +
		"sleep 60\n"
	require.NoError(t, os.WriteFile(path, []byte(script), 0o755))

	start := time.Now()
	id := resolveCommandIdentity(path)
	elapsed := time.Since(start)

	assert.Equal(t, identityUnknown, id)
	assert.False(t, commandIsUsableCursorCandidate(path))
	// Bound: timeout + WaitDelay + small scheduling margin (not 60s sleep).
	maxWait := identityProbeTimeout + identityProbeWaitDelay + 500*time.Millisecond
	assert.Less(t, elapsed, maxWait,
		"probe must return within timeout+WaitDelay (got %v, max %v)", elapsed, maxWait)
}

func TestIdentityProbe_OversizeOutputUnknown(t *testing.T) {
	clearIdentityProbeCache()
	dir := t.TempDir()
	path := filepath.Join(dir, "huge-version")
	if runtime.GOOS == "windows" {
		path += ".bat"
		// Emit more than identityProbeMaxOutput bytes then pretend success.
		script := "@echo off\r\n" +
			"if \"%~1\"==\"--version\" (\r\n" +
			"  for /L %%i in (1,1,500) do echo xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx\r\n" +
			"  exit /b 0\r\n" +
			")\r\n" +
			"if \"%~1\"==\"-v\" (\r\n" +
			"  for /L %%i in (1,1,500) do echo xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx\r\n" +
			"  exit /b 0\r\n" +
			")\r\n" +
			"exit /b 1\r\n"
		require.NoError(t, os.WriteFile(path, []byte(script), 0o755))
	} else {
		script := "#!/bin/sh\n" +
			"case \"$1\" in\n" +
			"  *etxtbsy*) exit 0;;\n" +
			"  --version|-v) dd if=/dev/zero bs=1024 count=20 2>/dev/null | tr '\\0' 'x';;\n" +
			"  *) exit 1;;\n" +
			"esac\n"
		require.NoError(t, os.WriteFile(path, []byte(script), 0o755))
	}

	assert.Equal(t, identityUnknown, resolveCommandIdentity(path))
	assert.False(t, commandIsUsableCursorCandidate(path),
		"oversize version dump must not classify as Cursor")
}

func TestIdentityProbeCache_DoesNotCacheUnknown(t *testing.T) {
	clearIdentityProbeCache()
	dir := t.TempDir()
	counter := filepath.Join(dir, "count")
	require.NoError(t, os.WriteFile(counter, nil, 0o644))

	path := filepath.Join(dir, "agent")
	if runtime.GOOS == "windows" {
		path += ".bat"
		script := "@echo off\r\n" +
			"echo.>>\"" + counter + "\"\r\n" +
			"exit /b 1\r\n"
		require.NoError(t, os.WriteFile(path, []byte(script), 0o755))
	} else {
		script := "#!/bin/sh\n" +
			"echo x >> '" + counter + "'\n" +
			"exit 1\n"
		require.NoError(t, os.WriteFile(path, []byte(script), 0o755))
	}

	assert.Equal(t, identityUnknown, versionProbeIdentity(path))
	first := countProbeInvocations(t, counter)
	require.Positive(t, first)

	assert.Equal(t, identityUnknown, versionProbeIdentity(path))
	second := countProbeInvocations(t, counter)
	assert.Greater(t, second, first,
		"unknown identity must not be cached as a conclusive result")
}

func TestCommandIsUsableCursorCandidate_DistinctBinary(t *testing.T) {
	clearIdentityProbeCache()
	dir := t.TempDir()
	_ = writeProbeScript(t, dir, "grok", "grok 0.2.118 (abc) [stable]")
	cursor := writeProbeScript(t, dir, "agent", "cursor agent 1.2.3")

	assert.False(t, commandLooksLikeGrok(cursor))
	assert.True(t, commandIsUsableCursorCandidate(cursor))
	assert.Equal(t, identityNotGrok, resolveCommandIdentity(cursor))
}

func TestCursorUnavailableWhenOnlyGrokAgent(t *testing.T) {
	clearIdentityProbeCache()
	dir := t.TempDir()
	grok := writeProbeScript(t, dir, "grok", "grok 0.2.0")
	agentLink := filepath.Join(dir, "agent")
	if runtime.GOOS == "windows" {
		// Separate copy with Grok version identity.
		agentLink = writeProbeScript(t, dir, "agent", "grok 0.2.0")
	} else {
		require.NoError(t, os.Symlink(grok, agentLink))
	}

	// Point Cursor agent at the Grok alias.
	cursor := NewCursorAgent(agentLink)
	assert.Empty(t, firstAvailableCommand(cursor), "cursor must be unavailable when agent is grok")

	// Grok itself remains available via its own binary.
	g := NewGrokAgent(grok)
	assert.Equal(t, grok, firstAvailableCommand(g))
}

func TestCursorAndGrokBothAvailableWhenDistinct(t *testing.T) {
	clearIdentityProbeCache()
	dir := t.TempDir()
	grok := writeProbeScript(t, dir, "grok", "grok 0.2.0")
	cursorBin := writeProbeScript(t, dir, "agent", "1.0.0")

	assert.NotEmpty(t, firstAvailableCommand(NewGrokAgent(grok)))
	assert.NotEmpty(t, firstAvailableCommand(NewCursorAgent(cursorBin)))
}

func TestIsAvailableCursorWithOnlyGrokOnPATH(t *testing.T) {
	// Integration-style: put grok-as-agent first on PATH and ensure IsAvailable("cursor") is false.
	clearIdentityProbeCache()
	dir := t.TempDir()
	grok := writeProbeScript(t, dir, "grok", "grok 9.9.9")
	if runtime.GOOS == "windows" {
		_ = writeProbeScript(t, dir, "agent", "grok 9.9.9")
	} else {
		require.NoError(t, os.Symlink(grok, filepath.Join(dir, "agent")))
	}

	oldPath := os.Getenv("PATH")
	t.Cleanup(func() { _ = os.Setenv("PATH", oldPath) })
	require.NoError(t, os.Setenv("PATH", dir+string(os.PathListSeparator)+oldPath))

	assert.True(t, IsAvailable("grok"), "grok should be available")
	assert.False(t, IsAvailable("cursor"), "cursor must not claim grok agent alias")
}

func TestIsAvailable_CursorDoesNotStartAgenticSession(t *testing.T) {
	// IsAvailable("cursor") may probe --version/-v only. A fixture that
	// marks positional prompts proves no agentic prompt was sent.
	clearIdentityProbeCache()
	dir := t.TempDir()
	marker := filepath.Join(dir, "positional.log")
	require.NoError(t, os.WriteFile(marker, nil, 0o644))
	_ = writeProbeScriptWithMarker(t, dir, "agent", "cursor agent 9.0", marker)

	oldPath := os.Getenv("PATH")
	t.Cleanup(func() { _ = os.Setenv("PATH", oldPath) })
	require.NoError(t, os.Setenv("PATH", dir+string(os.PathListSeparator)+oldPath))

	// Drop any real "agent" earlier on PATH by putting dir first.
	assert.True(t, IsAvailable("cursor"))
	data, err := os.ReadFile(marker)
	require.NoError(t, err)
	assert.Empty(t, strings.TrimSpace(string(data)),
		"IsAvailable(cursor) must not send positional agent prompts")
}

func TestIsAvailableWithConfig_CursorCmdPointsToGrok(t *testing.T) {
	clearIdentityProbeCache()
	dir := t.TempDir()
	grok := writeProbeScript(t, dir, "grok", "grok 0.2.0")

	originalRegistry := registry
	registry = map[string]Agent{
		"cursor": NewCursorAgent("agent"),
		"grok":   NewGrokAgent(grok),
	}
	t.Cleanup(func() { registry = originalRegistry })

	cfg := &config.Config{CursorCmd: grok}
	assert.False(t, isAvailableWithConfig("cursor", cfg),
		"cursor_cmd pointing at Grok must fail identity validation")

	_, err := GetAvailableExactWithConfigFromConfig(nil, "cursor", cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unavailable")
}

func TestIsAvailableWithConfig_CursorCmdPointsToCursor(t *testing.T) {
	clearIdentityProbeCache()
	dir := t.TempDir()
	cursorBin := writeProbeScript(t, dir, "agent", "cursor agent 2.0.0")

	originalRegistry := registry
	registry = map[string]Agent{
		"cursor": NewCursorAgent("agent"),
	}
	t.Cleanup(func() { registry = originalRegistry })

	cfg := &config.Config{CursorCmd: cursorBin}
	assert.True(t, isAvailableWithConfig("cursor", cfg))

	resolved, err := GetAvailableExactWithConfigFromConfig(nil, "cursor", cfg)
	require.NoError(t, err)
	ca, ok := resolved.(CommandAgent)
	require.True(t, ok)
	assert.Equal(t, cursorBin, ca.CommandName())
}

func TestIsAvailableWithConfig_PreferredAndBackup(t *testing.T) {
	clearIdentityProbeCache()
	dir := t.TempDir()
	grok := writeProbeScript(t, dir, "grok", "grok 0.2.0")
	cursorBin := writeProbeScript(t, dir, "agent", "cursor 1.0")

	originalRegistry := registry
	registry = map[string]Agent{
		"cursor": NewCursorAgent("agent"),
		"grok":   NewGrokAgent(grok),
	}
	t.Cleanup(func() { registry = originalRegistry })

	// Preferred cursor_cmd is Grok → unavailable; backup grok via GrokCmd works.
	cfg := &config.Config{
		CursorCmd: grok,
		GrokCmd:   grok,
	}
	assert.False(t, isAvailableWithConfig("cursor", cfg))
	assert.True(t, isAvailableWithConfig("grok", cfg))

	// Preferred with genuine cursor override succeeds without falling through.
	cfgOK := &config.Config{CursorCmd: cursorBin}
	resolved, err := GetPreferredOrBackupWithConfigFromConfig(nil, "cursor", cfgOK)
	require.NoError(t, err)
	assert.Equal(t, "cursor", resolved.Name())

	// Preferred cursor_cmd is Grok → preferred fails; backup grok is selected.
	resolved, err = GetPreferredOrBackupWithConfigFromConfig(
		nil, "cursor", cfg, "grok",
	)
	require.NoError(t, err)
	assert.Equal(t, "grok", resolved.Name())
}

func TestIsAvailableWithConfig_PreferredCursorUnknown_BackupGrok(t *testing.T) {
	// Inconclusive cursor_cmd (probe error) must fail closed and allow backup.
	clearIdentityProbeCache()
	dir := t.TempDir()
	badCursor := writeFailingProbe(t, dir, "bad-cursor", "error")
	grok := writeProbeScript(t, dir, "grok", "grok 0.2.0")

	originalRegistry := registry
	registry = map[string]Agent{
		"cursor": NewCursorAgent("agent"),
		"grok":   NewGrokAgent(grok),
	}
	t.Cleanup(func() { registry = originalRegistry })

	cfg := &config.Config{
		CursorCmd: badCursor,
		GrokCmd:   grok,
	}
	assert.False(t, isAvailableWithConfig("cursor", cfg),
		"inconclusive cursor identity must fail closed")
	resolved, err := GetPreferredOrBackupWithConfigFromConfig(
		nil, "cursor", cfg, "grok",
	)
	require.NoError(t, err)
	assert.Equal(t, "grok", resolved.Name())
}

func TestIdentityProbeCache_AvoidsRepeatedSubprocesses(t *testing.T) {
	clearIdentityProbeCache()
	dir := t.TempDir()
	counter := filepath.Join(dir, "count")
	require.NoError(t, os.WriteFile(counter, nil, 0o644))

	path := filepath.Join(dir, "agent")
	if runtime.GOOS == "windows" {
		path += ".bat"
		script := "@echo off\r\n" +
			"echo.>>\"" + counter + "\"\r\n" +
			"if \"%~1\"==\"--version\" (\r\n" +
			"  echo grok 1.0.0\r\n" +
			"  exit /b 0\r\n" +
			")\r\n" +
			"if \"%~1\"==\"-v\" (\r\n" +
			"  echo grok 1.0.0\r\n" +
			"  exit /b 0\r\n" +
			")\r\n" +
			"exit /b 1\r\n"
		require.NoError(t, os.WriteFile(path, []byte(script), 0o755))
	} else {
		script := "#!/bin/sh\n" +
			"echo x >> '" + counter + "'\n" +
			"case \"$1\" in\n" +
			"  --version|-v) echo 'grok 1.0.0';;\n" +
			"  *) exit 1;;\n" +
			"esac\n"
		require.NoError(t, os.WriteFile(path, []byte(script), 0o755))
	}

	assert.True(t, versionOutputLooksLikeGrok(path))
	first := countProbeInvocations(t, counter)
	require.Positive(t, first)

	assert.True(t, versionOutputLooksLikeGrok(path))
	assert.True(t, versionOutputLooksLikeGrok(path))
	assert.Equal(t, first, countProbeInvocations(t, counter),
		"cached probes must not spawn additional subprocesses")
}

func countProbeInvocations(t *testing.T, counter string) int {
	t.Helper()
	data, err := os.ReadFile(counter)
	require.NoError(t, err)
	if len(data) == 0 {
		return 0
	}
	// Each probe appends one line (Unix `echo x >>` or Windows `echo.>>`).
	count := strings.Count(string(data), "\n")
	if !strings.HasSuffix(string(data), "\n") {
		count++
	}
	if count == 0 {
		return 1
	}
	return count
}

func TestVersionOutputLooksLikeGrok(t *testing.T) {
	clearIdentityProbeCache()
	dir := t.TempDir()
	g := writeProbeScript(t, dir, "g", "grok 1.0.0")
	c := writeProbeScript(t, dir, "c", "cursor-agent 1.0.0")
	assert.True(t, versionOutputLooksLikeGrok(g))
	assert.False(t, versionOutputLooksLikeGrok(c))
	assert.Equal(t, identityNotGrok, versionProbeIdentity(c))
}

func TestSameFileSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink sameFile test is Unix-oriented")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	require.NoError(t, os.WriteFile(target, []byte("x"), 0o644))
	link := filepath.Join(dir, "link")
	require.NoError(t, os.Symlink(target, link))
	assert.True(t, sameFile(target, link))
}

func TestIdentityScriptsExecutable(t *testing.T) {
	dir := t.TempDir()
	p := writeProbeScript(t, dir, "probe", "ok")
	out, err := exec.Command(p, "--version").CombinedOutput()
	require.NoError(t, err)
	assert.Contains(t, string(out), "ok")

	_, err = exec.Command(p, "version").CombinedOutput()
	require.Error(t, err, "Cursor-shaped fixture must reject positional version")
}

func TestAvailableCommandForAgent_UsesValidator(t *testing.T) {
	clearIdentityProbeCache()
	dir := t.TempDir()
	grok := writeProbeScript(t, dir, "grok", "grok 1.0.0")
	cursor := writeProbeScript(t, dir, "agent", "cursor 1.0.0")
	unknown := writeFailingProbe(t, dir, "unknown", "error")

	ca := NewCursorAgent("agent")
	assert.False(t, availableCommandForAgent(ca, grok))
	assert.True(t, availableCommandForAgent(ca, cursor))
	assert.False(t, availableCommandForAgent(ca, unknown),
		"inconclusive identity must fail closed for cursor")
	assert.False(t, availableCommandForAgent(ca, ""))
	assert.False(t, availableCommandForAgent(nil, cursor))
}
