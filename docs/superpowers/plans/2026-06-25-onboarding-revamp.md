# roborev Onboarding Revamp Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make roborev's automation legible and front-and-center — a repo-aware `roborev quickstart` command agents can read, a fixed `hooks = []` papercut, restructured automation-first docs, and one canonical "how it works" diagram.

**Architecture:** Phase 1 adds Go: a `config` writer + `omitempty` fix, an `agenthook` read-only detector, and a new read-only `roborev quickstart` command (human + `--json`). Phase 2 restructures the Zensical docs site (nav + new automation page + content). Phase 3 adds a hand-authored SVG wired into the homepage and README.

**Tech Stack:** Go (Cobra CLI, `pelletier/go-toml/v2`), `go.kenn.io/kit/git/repo` (`gitrepo`), Zensical static site (Markdown + TOML nav), testify.

## Global Constraints

- Go: run `go fmt ./...` and `go vet ./...` before every commit; stage all resulting changes.
- Tests use testify (`assert`/`require`); table-driven where natural; `t.TempDir()` for isolation; `test` agent is always available.
- `roborev quickstart` detection MUST be read-only: no daemon start/restart, no hook/config writes. Use `probeDaemonWithRetry` (not `ensureDaemon`) and `gitrepo.HooksPath` / `githook.NotInstalled` (not `EnsureAbsoluteHooksPath`).
- `fix_command` is always fully substituted — never emit a literal `<agent>` placeholder. Resolve via `config.ResolveAgent("", repoRoot, global)` (codex fallback).
- JSON contract is stable: check `id` set+ordering = `daemon_running, post_commit_hook, repo_registered, repo_config, configured_agent, agent_hook_claude, agent_hook_codex, skills_installed`; `status` ∈ `ok|missing|unknown`.
- Docs reference the new SVG relatively (`/assets/static/how-it-works.svg`); README uses the absolute URL `https://roborev.io/assets/static/how-it-works.svg`.
- No emojis in code or output. Commit after every task; never `--amend`; never `--no-verify`.
- Spec: `docs/superpowers/specs/2026-06-25-onboarding-revamp-design.md`.

---

## File Structure

**Phase 1 (Go):**
- Modify `internal/config/config.go` — `omitempty` on both `Hooks` tags; add `WriteDefaultGlobalConfigTo`.
- Modify `internal/config/config_test.go` — hooks-omitted / round-trip / writer tests.
- Modify `cmd/roborev/init_cmd.go` — route first-time global config creation through the new writer.
- Create `internal/agenthook/detect.go` — read-only `Installed(path string) (bool, error)`.
- Create `internal/agenthook/detect_test.go`.
- Create `cmd/roborev/quickstart.go` — state types, `detectState`, rendering.
- Create `cmd/roborev/quickstart_guide.md` — embedded static explainer.
- Create `cmd/roborev/quickstart_cmd.go` — Cobra command (`--json`).
- Create `cmd/roborev/quickstart_cmd_test.go`.
- Modify `cmd/roborev/main.go` — register `quickstartCmd()`.

**Phase 2 (docs):**
- Modify `docs/zensical.toml` — Automation nav group + terminology.
- Create `docs/automation/post-commit-reviews.md`.
- Modify `docs/index.md`, `README.md`, `docs/configuration.md`, `docs/guides/troubleshooting.md`.

**Phase 3 (visual):**
- Create `docs/assets/static/how-it-works.svg` (also lands on `docs-assets` orphan branch per `docs/README.md`).
- Modify `docs/assets/update-static-assets-branch.sh`, `docs/assets/hydrate-assets.sh`, `docs/index.md`, `README.md`.

---

## Phase 1 — CLI + config

### Task 1: Omit empty `hooks` from marshaled config

**Files:**
- Modify: `internal/config/config.go:226`, `internal/config/config.go:421`
- Test: `internal/config/config_test.go`

**Interfaces:**
- Produces: `Config.Hooks` / `RepoConfig.Hooks` no longer marshal to `hooks = []` when empty.

- [ ] **Step 1: Write the failing test**

Add to `internal/config/config_test.go`:

```go
func TestEmptyHooksOmittedFromMarshal(t *testing.T) {
	data, err := tomlv2.Marshal(DefaultConfig())
	require.NoError(t, err)
	assert.NotContains(t, string(data), "hooks = []",
		"empty hooks must not marshal to a value array that collides with [[hooks]]")
}

func TestNonEmptyHooksRoundTrip(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Hooks = []HookConfig{{Event: "review.*", Type: "kata", Project: "myproj"}}

	data, err := tomlv2.Marshal(cfg)
	require.NoError(t, err)

	var got Config
	require.NoError(t, tomlv2.Unmarshal(data, &got))
	require.Len(t, got.Hooks, 1)
	assert.Equal(t, "review.*", got.Hooks[0].Event)
	assert.Equal(t, "kata", got.Hooks[0].Type)
	assert.Equal(t, "myproj", got.Hooks[0].Project)
}

func TestUserAddedHooksBlockParses(t *testing.T) {
	// Regression: default config + hand-added [[hooks]] must not collide.
	data, err := tomlv2.Marshal(DefaultConfig())
	require.NoError(t, err)
	combined := string(data) + "\n[[hooks]]\nevent = \"review.*\"\ntype = \"kata\"\n"

	var got Config
	require.NoError(t, tomlv2.Unmarshal([]byte(combined), &got))
	require.Len(t, got.Hooks, 1)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/ -run 'TestEmptyHooksOmitted|TestNonEmptyHooksRoundTrip|TestUserAddedHooksBlockParses' -v`
Expected: FAIL — `TestEmptyHooksOmittedFromMarshal` finds `hooks = []`; `TestUserAddedHooksBlockParses` errors on the duplicate `hooks` key.

- [ ] **Step 3: Add `,omitempty` to both tags**

`internal/config/config.go:226`:

```go
	Hooks []HookConfig `toml:"hooks,omitempty"`
```

`internal/config/config.go:421`:

```go
	Hooks []HookConfig `toml:"hooks,omitempty"`
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/config/ -run 'TestEmptyHooksOmitted|TestNonEmptyHooksRoundTrip|TestUserAddedHooksBlockParses' -v`
Expected: PASS

- [ ] **Step 5: Run the full config package to catch regressions**

Run: `go test ./internal/config/`
Expected: PASS (if a pre-existing test asserts literal `hooks = []`, update it to assert absence).

- [ ] **Step 6: Commit**

```bash
go fmt ./... && go vet ./...
git add internal/config/config.go internal/config/config_test.go
git commit -m "Omit empty hooks from marshaled config"
```

---

### Task 2: First-time default global config writer

**Files:**
- Modify: `internal/config/config.go` (add after `SaveGlobalTo`, near `:1240`)
- Test: `internal/config/config_test.go`

**Interfaces:**
- Produces: `func WriteDefaultGlobalConfigTo(path string, cfg *Config) error` — marshals `cfg`, appends a commented `[[hooks]]` example, writes atomically with `0600`. Used only on first creation.

- [ ] **Step 1: Write the failing test**

```go
func TestWriteDefaultGlobalConfigTo(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	require.NoError(t, WriteDefaultGlobalConfigTo(path, DefaultConfig()))

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	content := string(raw)

	// Commented example present, but does not break parsing.
	assert.Contains(t, content, "# [[hooks]]")
	assert.NotContains(t, content, "\nhooks = []")
	var parsed Config
	require.NoError(t, tomlv2.Unmarshal(raw, &parsed))

	// 0600 permissions.
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestSaveGlobalToHasNoCommentedExample(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	require.NoError(t, SaveGlobalTo(path, DefaultConfig()))
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "# [[hooks]]",
		"normal rewrites must not reintroduce the commented example")
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/ -run 'TestWriteDefaultGlobalConfigTo|TestSaveGlobalToHasNoCommentedExample' -v`
Expected: FAIL — `WriteDefaultGlobalConfigTo` undefined.

- [ ] **Step 3: Implement the writer**

Add to `internal/config/config.go` after `SaveGlobalTo`:

```go
// defaultHooksExample is appended (commented) only when creating the global
// config for the first time, so the [[hooks]] feature is discoverable without
// colliding with the (now omitted) empty hooks array.
const defaultHooksExample = `
# To run a command or built-in integration when reviews complete, add hooks.
# Uncomment and edit. See https://roborev.io for the full reference.
#
# [[hooks]]
# event = "review.failed"
# command = "notify-send 'roborev: review failed for {repo_name}'"
#
# [[hooks]]
# event = "review.*"
# type = "kata"
# project = "myproj"
`

// WriteDefaultGlobalConfigTo writes cfg to path for first-time creation,
// appending a commented [[hooks]] example. It writes atomically (temp file +
// rename) with 0600 permissions. Use SaveGlobalTo for subsequent rewrites.
func WriteDefaultGlobalConfigTo(path string, cfg *Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	data, err := tomlv2.Marshal(cfg)
	if err != nil {
		return err
	}
	data = append(data, []byte(defaultHooksExample)...)

	f, err := os.CreateTemp(filepath.Dir(path), ".roborev-config-*.toml")
	if err != nil {
		return err
	}
	tmpPath := f.Name()
	defer os.Remove(tmpPath)

	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/config/ -run 'TestWriteDefaultGlobalConfigTo|TestSaveGlobalToHasNoCommentedExample' -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
go fmt ./... && go vet ./...
git add internal/config/config.go internal/config/config_test.go
git commit -m "Add first-time default global config writer with hooks example"
```

---

### Task 3: Route `roborev init` through the new writer

**Files:**
- Modify: `cmd/roborev/init_cmd.go:45-57`

**Interfaces:**
- Consumes: `config.WriteDefaultGlobalConfigTo` (Task 2).

- [ ] **Step 1: Replace the first-creation save**

In `cmd/roborev/init_cmd.go`, change the creation branch:

```go
			configPath := config.GlobalConfigPath()
			if _, err := os.Stat(configPath); os.IsNotExist(err) {
				cfg := config.DefaultConfig()
				if agent != "" {
					cfg.DefaultAgent = agent
				}
				if err := config.WriteDefaultGlobalConfigTo(configPath, cfg); err != nil {
					return fmt.Errorf("save config: %w", err)
				}
				fmt.Printf("  Created config at %s\n", configPath)
			} else {
				fmt.Printf("  Config already exists at %s\n", configPath)
			}
```

- [ ] **Step 2: Build to verify it compiles**

Run: `go build ./cmd/roborev/`
Expected: success.

- [ ] **Step 3: Run the cmd package tests**

Run: `go test ./cmd/roborev/ -run TestInit`
Expected: PASS (existing init tests still pass; new config has commented example, valid TOML).

- [ ] **Step 4: Commit**

```bash
go fmt ./... && go vet ./...
git add cmd/roborev/init_cmd.go
git commit -m "Create first-time global config via WriteDefaultGlobalConfigTo"
```

---

### Task 4: Read-only agent-hook install detector

**Files:**
- Create: `internal/agenthook/detect.go`
- Test: `internal/agenthook/detect_test.go`

**Interfaces:**
- Produces: `func Installed(path string) (bool, error)` — true if the JSON config at `path` contains any roborev agent-hook command. Missing file → `(false, nil)`. Reuses `isRoborevAgentHookCommand`.

- [ ] **Step 1: Write the failing test**

```go
package agenthook

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInstalledMissingFile(t *testing.T) {
	ok, err := Installed(filepath.Join(t.TempDir(), "nope.json"))
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestInstalledDetectsRoborevHook(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	content := `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"roborev agent-hook run"}]}]}}`
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	ok, err := Installed(path)
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestInstalledIgnoresUnrelatedHooks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	content := `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"echo hi"}]}]}}`
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	ok, err := Installed(path)
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestInstalledInvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o644))
	_, err := Installed(path)
	assert.Error(t, err)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/agenthook/ -run TestInstalled -v`
Expected: FAIL — `Installed` undefined.

- [ ] **Step 3: Implement the detector**

Create `internal/agenthook/detect.go`:

```go
package agenthook

import (
	"encoding/json"
	"os"
)

// Installed reports whether the agent harness config at path contains a
// roborev agent-hook command. A missing file is not an error (returns false).
// Read-only: it never modifies the config.
func Installed(path string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	var root any
	if err := json.Unmarshal(data, &root); err != nil {
		return false, err
	}
	return jsonContainsRoborevHook(root), nil
}

// jsonContainsRoborevHook walks an arbitrary decoded JSON value looking for a
// string that is a roborev agent-hook command. This is schema-agnostic, so it
// works for both Claude (settings.json) and Codex (hooks.json) shapes.
func jsonContainsRoborevHook(v any) bool {
	switch t := v.(type) {
	case string:
		return isRoborevAgentHookCommand(t)
	case []any:
		for _, e := range t {
			if jsonContainsRoborevHook(e) {
				return true
			}
		}
	case map[string]any:
		for _, e := range t {
			if jsonContainsRoborevHook(e) {
				return true
			}
		}
	}
	return false
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/agenthook/ -run TestInstalled -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
go fmt ./... && go vet ./...
git add internal/agenthook/detect.go internal/agenthook/detect_test.go
git commit -m "Add read-only agent-hook install detector"
```

---

### Task 5: Quickstart state detection

**Files:**
- Create: `cmd/roborev/quickstart.go`
- Test: `cmd/roborev/quickstart_cmd_test.go`

**Interfaces:**
- Consumes: `agenthook.Installed` (Task 4); `config.ResolveAgent`, `config.LoadGlobal`, `config.LoadRepoConfig`; `githook.NotInstalled`; `probeDaemonWithRetry`, `getDaemonEndpoint` (`cmd/roborev/daemon_lifecycle.go`); `skills.IsInstalled`; `agenthook.DefaultClaudeSettingsPath`, `agenthook.DefaultCodexHooksPath`.
- Produces:
  - `type checkStatus string` with `statusOK="ok"`, `statusMissing="missing"`, `statusUnknown="unknown"`.
  - `type quickstartCheck struct { ID string; Status checkStatus; Details string; FixCommand string }` (json tags: `id`,`status`,`details,omitempty`,`fix_command,omitempty`).
  - `type quickstartState struct { InGitRepo bool; DaemonRunning bool; Checks []quickstartCheck }` (json: `in_git_repo`,`daemon_running`,`checks`).
  - `func detectState(ctx context.Context, repoRoot string, inGitRepo bool) quickstartState`.
  - `quickstartCheckIDs []string` — the stable ordered ID list.

- [ ] **Step 1: Write the failing test**

Add to `cmd/roborev/quickstart_cmd_test.go`:

```go
package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTempGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.Command("git", "init")
	cmd.Dir = dir
	require.NoError(t, cmd.Run())
	return dir
}

func TestDetectStateSchema(t *testing.T) {
	assert := assert.New(t)
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	repo := newTempGitRepo(t)

	state := detectState(context.Background(), repo, true)

	assert.True(state.InGitRepo)

	// Exactly the eight stable IDs, in order.
	var ids []string
	for _, c := range state.Checks {
		ids = append(ids, c.ID)
	}
	assert.Equal(quickstartCheckIDs, ids)

	for _, c := range state.Checks {
		assert.Contains([]checkStatus{statusOK, statusMissing, statusUnknown}, c.Status, c.ID)
		if c.Status == statusMissing {
			assert.NotEmpty(c.FixCommand, "missing check %s must have a fix_command", c.ID)
			assert.NotContains(c.FixCommand, "<agent>", "fix_command must be fully substituted")
		}
	}
}

func TestDetectStateOutsideRepoMarksRepoChecksUnknown(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	state := detectState(context.Background(), "", false)

	assert.False(t, state.InGitRepo)
	repoDependent := map[string]bool{
		"post_commit_hook": true, "repo_registered": true,
		"repo_config": true, "configured_agent": true,
	}
	for _, c := range state.Checks {
		if repoDependent[c.ID] {
			assert.Equal(t, statusUnknown, c.Status, c.ID)
		}
	}
}

func TestDetectStateIsReadOnly(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	repo := newTempGitRepo(t)
	hookPath := filepath.Join(repo, ".git", "hooks", "post-commit")

	_, errBefore := os.Stat(hookPath)
	require.True(t, os.IsNotExist(errBefore), "precondition: no hook yet")

	_ = detectState(context.Background(), repo, true)

	// Detection must not create a post-commit hook.
	_, errAfter := os.Stat(hookPath)
	assert.True(t, os.IsNotExist(errAfter), "detectState must not create a post-commit hook")
}

func TestStateJSONMarshalsStableFields(t *testing.T) {
	state := quickstartState{
		InGitRepo:     true,
		DaemonRunning: false,
		Checks:        []quickstartCheck{{ID: "daemon_running", Status: statusMissing, FixCommand: "roborev daemon start"}},
	}
	raw, err := json.Marshal(state)
	require.NoError(t, err)
	var back map[string]any
	require.NoError(t, json.Unmarshal(raw, &back))
	assert.Contains(t, back, "in_git_repo")
	assert.Contains(t, back, "daemon_running")
	assert.Contains(t, back, "checks")
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/roborev/ -run 'TestDetectState|TestStateJSON' -v`
Expected: FAIL — `detectState`, `quickstartCheckIDs`, types undefined.

- [ ] **Step 3: Implement the detector**

Create `cmd/roborev/quickstart.go`:

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"go.kenn.io/roborev/internal/agenthook"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/githook"
	"go.kenn.io/roborev/internal/skills"
)

type checkStatus string

const (
	statusOK      checkStatus = "ok"
	statusMissing checkStatus = "missing"
	statusUnknown checkStatus = "unknown"
)

type quickstartCheck struct {
	ID         string      `json:"id"`
	Status     checkStatus `json:"status"`
	Details    string      `json:"details,omitempty"`
	FixCommand string      `json:"fix_command,omitempty"`
}

type quickstartState struct {
	InGitRepo     bool              `json:"in_git_repo"`
	DaemonRunning bool              `json:"daemon_running"`
	Checks        []quickstartCheck `json:"checks"`
}

// quickstartCheckIDs is the stable, ordered set of check IDs (JSON contract).
var quickstartCheckIDs = []string{
	"daemon_running",
	"post_commit_hook",
	"repo_registered",
	"repo_config",
	"configured_agent",
	"agent_hook_claude",
	"agent_hook_codex",
	"skills_installed",
}

func detectState(ctx context.Context, repoRoot string, inGitRepo bool) quickstartState {
	daemonUp := daemonReachable()
	global, _ := config.LoadGlobal()
	agent := config.ResolveAgent("", repoRoot, global)

	checks := []quickstartCheck{
		checkDaemon(daemonUp),
		checkPostCommitHook(ctx, repoRoot, inGitRepo),
		checkRepoRegistered(repoRoot, inGitRepo, daemonUp),
		checkRepoConfig(repoRoot, inGitRepo),
		checkConfiguredAgent(repoRoot, inGitRepo, global, agent),
		checkAgentHook("agent_hook_claude", agenthook.DefaultClaudeSettingsPath(),
			"roborev agent-hook install --agent claude"),
		checkAgentHook("agent_hook_codex", agenthook.DefaultCodexHooksPath(),
			"roborev agent-hook install --agent codex"),
		checkSkills(),
	}

	return quickstartState{InGitRepo: inGitRepo, DaemonRunning: daemonUp, Checks: checks}
}

func daemonReachable() bool {
	_, err := probeDaemonWithRetry(getDaemonEndpoint(), 1*time.Second)
	return err == nil
}

func checkDaemon(up bool) quickstartCheck {
	if up {
		return quickstartCheck{ID: "daemon_running", Status: statusOK, Details: "daemon is running"}
	}
	return quickstartCheck{ID: "daemon_running", Status: statusMissing,
		Details: "daemon is not reachable", FixCommand: "roborev daemon start"}
}

func checkPostCommitHook(ctx context.Context, repoRoot string, inGitRepo bool) quickstartCheck {
	c := quickstartCheck{ID: "post_commit_hook"}
	if !inGitRepo {
		c.Status = statusUnknown
		return c
	}
	if githook.NotInstalled(ctx, repoRoot, "post-commit") {
		c.Status = statusMissing
		c.Details = "commits are not auto-reviewed"
		c.FixCommand = "roborev install-hook"
		return c
	}
	c.Status = statusOK
	c.Details = "every commit is auto-reviewed"
	return c
}

func checkRepoRegistered(repoRoot string, inGitRepo, daemonUp bool) quickstartCheck {
	c := quickstartCheck{ID: "repo_registered"}
	if !inGitRepo {
		c.Status = statusUnknown
		return c
	}
	if !daemonUp {
		c.Status = statusUnknown
		c.Details = "daemon unreachable; cannot verify registration"
		return c
	}
	tracked, err := repoTracked(repoRoot)
	if err != nil {
		c.Status = statusUnknown
		c.Details = fmt.Sprintf("could not query daemon: %v", err)
		return c
	}
	if tracked {
		c.Status = statusOK
		c.Details = "repo is registered with the daemon"
		return c
	}
	c.Status = statusMissing
	c.Details = "repo is not registered with the daemon"
	c.FixCommand = "roborev init"
	return c
}

func repoTracked(repoRoot string) (bool, error) {
	ep := getDaemonEndpoint()
	resp, err := ep.HTTPClient(5*time.Second).Get(
		ep.BaseURL() + "/api/repos/resolve?path=" + url.QueryEscape(repoRoot))
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	var body struct {
		Tracked bool `json:"tracked"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return false, err
	}
	return body.Tracked, nil
}

func checkRepoConfig(repoRoot string, inGitRepo bool) quickstartCheck {
	c := quickstartCheck{ID: "repo_config"}
	if !inGitRepo {
		c.Status = statusUnknown
		return c
	}
	if _, err := os.Stat(filepath.Join(repoRoot, ".roborev.toml")); err == nil {
		c.Status = statusOK
		c.Details = ".roborev.toml present"
		return c
	}
	c.Status = statusMissing
	c.Details = "no per-repo .roborev.toml (using global defaults)"
	c.FixCommand = "roborev init --agent codex"
	return c
}

func checkConfiguredAgent(repoRoot string, inGitRepo bool, global *config.Config, agent string) quickstartCheck {
	c := quickstartCheck{ID: "configured_agent"}
	if !inGitRepo {
		c.Status = statusUnknown
		return c
	}
	explicit := false
	if repoCfg, err := config.LoadRepoConfig(repoRoot); err == nil && repoCfg != nil && repoCfg.Agent != "" {
		explicit = true
	}
	if global != nil && global.DefaultAgent != "" {
		explicit = true
	}
	if explicit {
		c.Status = statusOK
		c.Details = fmt.Sprintf("review agent: %s", agent)
		return c
	}
	c.Status = statusMissing
	c.Details = fmt.Sprintf("no agent configured; defaulting to %s", agent)
	c.FixCommand = fmt.Sprintf("roborev config set --local agent %s", agent)
	return c
}

func checkAgentHook(id, path, fix string) quickstartCheck {
	c := quickstartCheck{ID: id}
	installed, err := agenthook.Installed(path)
	if err != nil {
		c.Status = statusUnknown
		c.Details = fmt.Sprintf("could not read %s: %v", path, err)
		return c
	}
	if installed {
		c.Status = statusOK
		c.Details = "agent hook installed"
		return c
	}
	c.Status = statusMissing
	c.Details = "agent hook not installed (no mid-session fix nudges)"
	c.FixCommand = fix
	return c
}

func checkSkills() quickstartCheck {
	c := quickstartCheck{ID: "skills_installed"}
	if skills.IsInstalled(skills.AgentClaude) || skills.IsInstalled(skills.AgentCodex) {
		c.Status = statusOK
		c.Details = "roborev skills installed"
		return c
	}
	c.Status = statusMissing
	c.Details = "roborev skills not installed"
	c.FixCommand = "roborev skills install"
	return c
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./cmd/roborev/ -run 'TestDetectState|TestStateJSON' -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
go fmt ./... && go vet ./...
git add cmd/roborev/quickstart.go cmd/roborev/quickstart_cmd_test.go
git commit -m "Add read-only quickstart state detection"
```

---

### Task 6: Quickstart command (human + `--json`) with embedded guide

**Files:**
- Create: `cmd/roborev/quickstart_guide.md`
- Create: `cmd/roborev/quickstart_cmd.go`
- Modify: `cmd/roborev/main.go`
- Test: `cmd/roborev/quickstart_cmd_test.go` (extend)

**Interfaces:**
- Consumes: `detectState`, `quickstartState` (Task 5).
- Produces: `func quickstartCmd() *cobra.Command`; `func renderHuman(w io.Writer, s quickstartState)`; embedded `quickstartGuide string`.

- [ ] **Step 1: Create the embedded guide**

Create `cmd/roborev/quickstart_guide.md`:

```markdown
## How roborev works

roborev gives your coding agent a second set of eyes. It reviews your code
automatically, in the background, on every commit, and feeds findings back so
the agent (or you) can fix them. There are two automation layers:

**Layer 1 - Post-commit reviews (works everywhere).**
A git post-commit hook enqueues a background review of every commit. This works
with any editor or agent. Findings land in `roborev tui`, `roborev show HEAD`,
and the daemon API.

**Layer 2 - Agent hook (CLI harnesses).**
The agent hook watches your coding-agent session (turns, commits, failed
reviews). When review work piles up, it returns one instruction telling the
agent to run the `/roborev-fix` skill before the session goes cold - closing the
write -> review -> fix loop without you asking. This requires a CLI harness
(Claude Code CLI or Codex) that exposes PreToolUse / PostToolUse / Stop hooks.
Claude Desktop does not expose these, so only Layer 1 runs there.

## Configuration playbook

You are helping a human configure roborev for their repo. Use the "Current
state" section above to see what is already set up, then apply only what is
missing. Confirm changes with the user before editing their files.

### Make reviews flag what this team cares about

Add standing instructions to every review with `review_guidelines` in the repo's
`.roborev.toml`. They are injected into each review prompt.

```toml
# .roborev.toml
review_guidelines = """
Every change to UI components must include or update a Playwright e2e test.
Flag any PR that changes UI without a corresponding e2e test.
"""
```

To act on review outcomes (notify, file issues), add hooks. Because empty hooks
are omitted from the generated config, you can add a `[[hooks]]` block directly:

```toml
[[hooks]]
event = "review.*"
type = "kata"
project = "myproj"
```

### Tune the agent's own CLAUDE.md / AGENTS.md

roborev reviews each commit, so frequent, small commits produce tighter, more
useful feedback. If the user's agent batches large changes, suggest adding this
to their CLAUDE.md or AGENTS.md:

```markdown
## Committing
Commit early and often. After each self-contained change that builds and passes
tests, make a commit rather than batching many changes together. Small commits
get faster, more focused automated review.
```

### Choose the review agent and model

Set the review agent and model per repo (`.roborev.toml`) or globally
(`~/.roborev/config.toml`):

```toml
agent = "codex"          # codex, claude-code, gemini, copilot, ...
model = "gpt-5-codex"    # optional, agent-specific
```

For per-workflow routing and reasoning levels (fast / standard / thorough), see
https://roborev.io/configuration/.
```

- [ ] **Step 2: Write the failing test**

Add to `cmd/roborev/quickstart_cmd_test.go`:

```go
func TestRenderHumanIncludesGuideAndState(t *testing.T) {
	var buf bytes.Buffer
	renderHuman(&buf, quickstartState{
		InGitRepo: true,
		Checks:    []quickstartCheck{{ID: "daemon_running", Status: statusOK, Details: "daemon is running"}},
	})
	out := buf.String()
	assert.Contains(t, out, "How roborev works")     // embedded guide
	assert.Contains(t, out, "daemon_running")        // detected state
}

func TestQuickstartJSONOmitsGuide(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	repo := newTempGitRepo(t)

	cmd := quickstartCmd()
	cmd.SetArgs([]string{"--json"})
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	// Run from the repo dir.
	t.Chdir(repo)
	require.NoError(t, cmd.Execute())

	out := buf.String()
	assert.NotContains(t, out, "How roborev works")
	var state quickstartState
	require.NoError(t, json.Unmarshal([]byte(out), &state))
	assert.Len(t, state.Checks, len(quickstartCheckIDs))
}
```

Add `"bytes"` to the test imports.

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./cmd/roborev/ -run 'TestRenderHuman|TestQuickstartJSON' -v`
Expected: FAIL — `renderHuman`, `quickstartCmd` undefined.

- [ ] **Step 4: Implement the command**

Create `cmd/roborev/quickstart_cmd.go`:

```go
package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"go.kenn.io/roborev/internal/git"
)

//go:embed quickstart_guide.md
var quickstartGuide string

func quickstartCmd() *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "quickstart",
		Short: "Print an agent-oriented guide to setting up roborev in this repo",
		Long: `Print a repo-aware guide describing how roborev works and what is
configured. Designed to be read by a coding agent ("run roborev quickstart")
so it can help you finish setup. Detection is read-only.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()

			repoRoot, rootErr := git.GetRepoRoot(".")
			inGitRepo := rootErr == nil

			state := detectState(cmd.Context(), repoRoot, inGitRepo)

			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(state)
			}

			if !inGitRepo {
				fmt.Fprintln(cmd.ErrOrStderr(),
					"Not inside a git repository. Run roborev quickstart from your repo, then 'roborev init'.")
				return silentExit(cmd, 1)
			}

			renderHuman(out, state)
			return nil
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false, "emit detected state as JSON (no explainer)")
	return cmd
}

func renderHuman(w io.Writer, s quickstartState) {
	fmt.Fprintln(w, "# roborev setup")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "## Current state")
	fmt.Fprintln(w)
	for _, c := range s.Checks {
		mark := map[checkStatus]string{
			statusOK: "[ok]", statusMissing: "[missing]", statusUnknown: "[unknown]",
		}[c.Status]
		fmt.Fprintf(w, "%-10s %s", mark, c.ID)
		if c.Details != "" {
			fmt.Fprintf(w, " - %s", c.Details)
		}
		fmt.Fprintln(w)
		if c.Status == statusMissing && c.FixCommand != "" {
			fmt.Fprintf(w, "           fix: %s\n", c.FixCommand)
		}
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, quickstartGuide)
}
```

- [ ] **Step 5: Register the command**

In `cmd/roborev/main.go`, add to the command registration block (near the other `rootCmd.AddCommand(...)` calls):

```go
	rootCmd.AddCommand(quickstartCmd())
```

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test ./cmd/roborev/ -run 'TestRenderHuman|TestQuickstartJSON|TestDetectState|TestStateJSON' -v`
Expected: PASS

- [ ] **Step 7: Manual smoke check**

Run: `go run ./cmd/roborev quickstart` (from this repo) and `go run ./cmd/roborev quickstart --json`
Expected: human output shows "## Current state" + "## How roborev works"; `--json` is valid JSON with eight checks and no guide text.

- [ ] **Step 8: Commit**

```bash
go fmt ./... && go vet ./...
git add cmd/roborev/quickstart_cmd.go cmd/roborev/quickstart_guide.md cmd/roborev/quickstart_cmd_test.go cmd/roborev/main.go
git commit -m "Add roborev quickstart command"
```

---

## Phase 2 — Docs restructure & content

> Docs validation: the site builds via `docs/vercel-build.sh`. For local checks, the maintainer workflow is in `docs/README.md`. Each docs task ends by confirming the referenced files exist and nav paths resolve.

### Task 7: Automation nav group + new automation page

**Files:**
- Modify: `docs/zensical.toml:42` (the `nav` array)
- Create: `docs/automation/post-commit-reviews.md`

- [ ] **Step 1: Restructure the nav**

In `docs/zensical.toml`, replace the two scattered bottom entries:

```toml
  {"Review Hooks" = "guides/hooks.md"},
  {"Agent Hook" = "agent-hook.md"},
```

Remove those two lines, and insert a new top-level group immediately after the `{"Installation" = "installation.md"},` line:

```toml
  {"Automation" = [
    {"Post-Commit Reviews" = "automation/post-commit-reviews.md"},
    {"Agent Hook"          = "agent-hook.md"},
    {"Review Event Hooks"  = "guides/hooks.md"},
  ]},
```

- [ ] **Step 2: Create the automation landing page**

Create `docs/automation/post-commit-reviews.md`:

```markdown
# Automation: hands-off reviews

roborev is built to run hands-off. There are two automation layers - turn on
both for the full loop.

![How roborev works](/assets/static/how-it-works.svg){ loading=lazy }

## Layer 1 - Post-commit reviews

A git post-commit hook reviews every commit in the background. This works with
any editor or agent.

```bash
roborev init      # installs the hook, starts the daemon, registers the repo
```

Verify it is live:

```bash
roborev status        # daemon + queue
roborev show HEAD     # the latest commit's review
```

## Layer 2 - Agent hook

The agent hook watches your coding-agent session and, once review work piles up,
tells the agent to run the `/roborev-fix` skill before the session ends - closing
the write -> review -> fix loop automatically.

```bash
roborev skills install        # install the /roborev-fix skill
roborev agent-hook install    # wire the hook into Claude Code / Codex
```

See [Agent Hook](../agent-hook.md) for thresholds and configuration.

### Why CLI, not Desktop?

The agent hook relies on harness hooks (`PreToolUse` / `PostToolUse` / `Stop`)
that the Claude Code CLI and Codex expose. Claude Desktop does not expose these
hooks, so Layer 2 does not run there. Layer 1 (post-commit reviews) works
regardless of which agent or app you use.

## Let an agent finish setup

Point your coding agent at the built-in guide and it will inspect this repo and
help you finish configuration:

```bash
roborev quickstart            # human-readable
roborev quickstart --json     # machine-readable state for agents
```

## Acting on results

To notify or file issues when reviews complete, add [Review Event
Hooks](../guides/hooks.md).
```

- [ ] **Step 3: Confirm files and nav resolve**

Run: `ls docs/automation/post-commit-reviews.md docs/agent-hook.md docs/guides/hooks.md`
Expected: all three exist. Confirm `docs/zensical.toml` no longer contains a top-level `"Review Hooks"` entry: `grep -n 'Review Hooks' docs/zensical.toml` returns nothing.

- [ ] **Step 4: Commit**

```bash
git add docs/zensical.toml docs/automation/post-commit-reviews.md
git commit -m "Promote automation to top-level docs nav"
```

---

### Task 8: Homepage + README automation framing + content additions

**Files:**
- Modify: `docs/index.md`
- Modify: `README.md`
- Modify: `docs/configuration.md`
- Modify: `docs/guides/troubleshooting.md`

- [ ] **Step 1: Add the "How roborev works" section to the homepage**

In `docs/index.md`, immediately above the agent matrix, add:

```markdown
## How roborev works

roborev reviews every commit in the background and feeds findings back to your
coding agent so the write -> review -> fix loop runs hands-off.

- **Post-commit reviews** - every commit is reviewed automatically, with any agent.
- **Agent hook** - nudges your CLI agent to fix findings mid-session.

[Set up automation ->](automation/post-commit-reviews.md)
```

(The `how-it-works.svg` image is wired into the homepage hero in Task 9, after the asset exists.)

- [ ] **Step 2: Mirror the framing in the README**

In `README.md`, add an "Automation" subsection near the top of the existing
"How It Works" content:

```markdown
### Automation, two layers

- **Post-commit reviews** - a git hook reviews every commit in the background (any agent).
- **Agent hook** - watches your Claude Code / Codex session and tells the agent to run `/roborev-fix` when findings pile up.

```bash
roborev init                  # layer 1: per-commit reviews
roborev skills install
roborev agent-hook install    # layer 2: mid-session fix loop
```

New here? Run `roborev quickstart` and point your agent at it.
```

- [ ] **Step 3: Add the review-tuning section to configuration docs**

In `docs/configuration.md`, add a subsection (after the existing
`review_guidelines` mention, or in the per-repo config section):

```markdown
### Make roborev always flag something

Use `review_guidelines` in `.roborev.toml` to inject standing instructions into
every review for the repo:

```toml
review_guidelines = """
Every change to UI components must include or update a Playwright e2e test.
Flag any PR that changes UI without a corresponding e2e test.
"""
```

Because empty hooks are omitted from the generated config, you can add a
`[[hooks]]` block directly without removing anything:

```toml
[[hooks]]
event = "review.*"
type = "kata"
project = "myproj"
```
```

- [ ] **Step 4: Add the CLI-vs-Desktop FAQ entry**

In `docs/guides/troubleshooting.md`, add:

```markdown
## Automation works in Claude Code CLI but not Claude Desktop

The agent hook (mid-session fix nudges) relies on harness hooks
(`PreToolUse` / `PostToolUse` / `Stop`) that the Claude Code CLI and Codex
expose. Claude Desktop does not expose these hooks, so the agent-hook layer does
not run there. Post-commit reviews still work in any environment - check
`roborev status` and `roborev show HEAD` to confirm reviews are running.
```

- [ ] **Step 5: Confirm references**

Run: `grep -rn "automation/post-commit-reviews" docs/index.md`
Expected: one match. Confirm `docs/configuration.md` and `docs/guides/troubleshooting.md` contain the new headings.

- [ ] **Step 6: Commit**

```bash
git add docs/index.md README.md docs/configuration.md docs/guides/troubleshooting.md
git commit -m "Add automation framing, review-tuning, and CLI-vs-Desktop docs"
```

---

## Phase 3 — "How roborev works" SVG

### Task 9: Author the concept SVG and wire it in

**Files:**
- Create: `docs/assets/static/how-it-works.svg`
- Modify: `docs/assets/update-static-assets-branch.sh:12` (`expected_assets`)
- Modify: `docs/assets/hydrate-assets.sh:15` (`static_assets`)
- Modify: `docs/index.md` (hero), `README.md` (absolute URL)

- [ ] **Step 1: Author the SVG**

Create `docs/assets/static/how-it-works.svg` — a horizontal flow matching the
visual weight of `architecture.svg`/`federation.svg`. Nodes and arrows:

`You + agent write code` -> `git commit` -> `roborev reviews in background (codex, claude, gemini ...)` -> `findings` -> `agent hook nudges agent` -> `/roborev-fix` -> (loop arrow back to `write code`).

Start from this skeleton and refine spacing/colors to match the existing assets
(open `docs/assets/static/architecture.svg` for the palette and font):

```xml
<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 1200 360" font-family="Inter, system-ui, sans-serif">
  <rect width="1200" height="360" fill="none"/>
  <!-- nodes: rounded rects with labels; arrows between; one curved loop arrow
       from the last node back to the first. Reuse architecture.svg colors. -->
</svg>
```

- [ ] **Step 2: Validate the SVG renders**

Run: `open docs/assets/static/how-it-works.svg` (or load in a browser).
Expected: the flow is readable left-to-right with a visible loop-back arrow; no
overlapping text.

- [ ] **Step 3: Register the asset in both manifests**

In `docs/assets/update-static-assets-branch.sh`, add to the `expected_assets`
array (alphabetical, near `federation.svg`):

```bash
  "how-it-works.svg"
```

In `docs/assets/hydrate-assets.sh`, add the same entry to the `static_assets`
array.

- [ ] **Step 4: Wire into the homepage hero and README**

In `docs/index.md`, under the "How roborev works" heading from Task 8, add the
image as the lead visual (demoting the TUI hero below it):

```markdown
![How roborev works](/assets/static/how-it-works.svg){ loading=eager }
```

In `README.md`, add the diagram with an absolute URL near the automation
section:

```markdown
![How roborev works](https://roborev.io/assets/static/how-it-works.svg)
```

- [ ] **Step 5: Verify manifest consistency**

Run: `grep -c 'how-it-works.svg' docs/assets/update-static-assets-branch.sh docs/assets/hydrate-assets.sh`
Expected: `1` in each file. (Per `docs/README.md`, the SVG must also be pushed
to the `docs-assets` orphan branch; do that as part of the asset-update
workflow, not in this commit.)

- [ ] **Step 6: Commit**

```bash
git add docs/assets/static/how-it-works.svg docs/assets/update-static-assets-branch.sh docs/assets/hydrate-assets.sh docs/index.md README.md
git commit -m "Add how-it-works concept diagram and wire into docs"
```

---

## Self-Review notes (coverage map)

- Spec Phase 1a (hooks fix) -> Tasks 1-3.
- Spec Phase 1b (quickstart, read-only, --json schema, outside-repo behavior) -> Tasks 4-6.
- Spec Phase 2 (nav terminology, automation page, content additions, CLI-vs-Desktop) -> Tasks 7-8.
- Spec Phase 3 (canonical SVG, both asset manifests, absolute README URL) -> Task 9.
- All eight check IDs + enum + substituted fix_command enforced by Task 5 tests.
- Atomic/0600 writer enforced by Task 2 tests.
- Read-only detection enforced by Task 5 `TestDetectStateIsReadOnly`.
```
