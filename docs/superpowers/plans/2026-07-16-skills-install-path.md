# Skills Install Custom Path Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Allow `roborev skills install` to install a selected bundled agent variant directly into an arbitrary final skills directory, defaulting to Claude when `--agent` is omitted.

**Architecture:** Refactor the internal installer so standard agent-config installation and explicit-directory installation share one file-writing function. Expose a small `InstallToPath` entry point for the CLI, while leaving status, detection, and automatic update resolution unchanged.

**Tech Stack:** Go, Cobra, embedded filesystems, testify, Zensical Markdown documentation.

## Global Constraints

- `--path` is the final skills directory, not an agent config root.
- The default custom-path variant is `claude`.
- Accepted explicit variants are `claude`, `codex`, and `droid`.
- `--agent` without `--path` is invalid.
- Plain `roborev skills install` retains existing automatic multi-agent behavior.
- Custom paths are not added to status or automatic update tracking.
- Do not modify or stage the unrelated `.kata.toml` file.

---

### Task 1: Add custom-path installation and CLI support

**Files:**
- Modify: `internal/skills/skills.go`
- Modify: `internal/skills/skills_test.go`
- Modify: `cmd/roborev/skills.go`
- Create: `cmd/roborev/skills_test.go`
- Modify: `docs/guides/agent-skills.md`
- Modify: `docs/changelog.md`

**Interfaces:**
- Produces: `func InstallToPath(agent Agent, skillsDir string) (InstallResult, error)`
- Preserves: `func Install() ([]InstallResult, error)`, `func Update() ([]InstallResult, error)`, `func Status() []AgentStatus`, and `func IsInstalled(agent Agent) bool`

- [ ] **Step 1: Write failing internal installer tests**

Add tests that call `InstallToPath` with a temporary final skills directory and verify:

```go
result, err := InstallToPath(AgentClaude, skillsDir)
require.NoError(t, err)
assert.Equal(t, AgentClaude, result.Agent)
assert.Len(t, result.Installed, len(expectedSkillDirNamesForAgent(t, AgentClaude)))
```

Assert every expected Claude skill exists directly under `skillsDir`. Add explicit Codex coverage for `agents/openai.yaml`, Droid coverage for Droid skill names, a second invocation that reports updates, cleanup of `roborev-address` inside `skillsDir`, and an unsupported-agent case that leaves the destination absent.

- [ ] **Step 2: Run internal installer tests and verify RED**

Run:

```bash
go test ./internal/skills -run 'TestInstallToPath' -count=1
```

Expected: compilation failure because `InstallToPath` is undefined.

- [ ] **Step 3: Implement the shared destination installer**

In `internal/skills/skills.go`:

```go
func InstallToPath(agent Agent, skillsDir string) (InstallResult, error) {
    spec, ok := lookupAgent(agent)
    if !ok {
        return InstallResult{}, fmt.Errorf("unsupported agent %q (expected claude, codex, or droid)", agent)
    }
    return installSkills(spec, skillsDir)
}
```

Extract the existing skill-directory creation, embedded-file writing, installed/updated classification, and legacy cleanup from `installAgent` into `installSkills(spec agentSpec, skillsDir string)`. Keep `installAgent` responsible for home/config resolution and missing-config skipping. Change legacy cleanup to accept the already-resolved skills directory so both standard and custom installations remove legacy skill folders only in their own destination.

- [ ] **Step 4: Run internal installer tests and verify GREEN**

Run:

```bash
go test ./internal/skills -run 'TestInstallToPath|TestInstall|TestUpdate' -count=1
```

Expected: PASS.

- [ ] **Step 5: Write failing CLI tests**

Create `cmd/roborev/skills_test.go`. Execute `skillsCmd()` directly with `install --path <temp>` and verify default Claude skill files are written. Add tests for:

```go
cmd.SetArgs([]string{"install", "--agent", "codex"})
err := cmd.Execute()
assert.EqualError(t, err, "--agent requires --path")
```

Also verify `install --path <temp> --agent unknown` returns the unsupported-agent error and does not create the destination.

- [ ] **Step 6: Run CLI tests and verify RED**

Run:

```bash
go test ./cmd/roborev -run 'TestSkillsInstall' -count=1
```

Expected: FAIL because the install subcommand does not define `--path` or `--agent`.

- [ ] **Step 7: Add CLI flags and custom installation branch**

In `cmd/roborev/skills.go`, add local `installPath` and `installAgent` values. Register:

```go
installCmd.Flags().StringVar(&installPath, "path", "", "install directly into this skills directory")
installCmd.Flags().StringVar(&installAgent, "agent", string(skills.AgentClaude), "skill variant for --path (claude, codex, or droid)")
```

At the start of `RunE`, use `cmd.Flags().Changed` to distinguish ordinary installation from custom installation:

- Reject changed `--agent` when `--path` was not supplied.
- Reject an explicitly empty `--path`.
- For custom installation, call `skills.InstallToPath`, wrap the single result in the existing output formatting flow, and avoid the automatic-install "No agents found" message.
- For ordinary installation, keep calling `skills.Install()` exactly as before.

Update the long help text with the final-directory semantics and Claude default.

- [ ] **Step 8: Run CLI tests and verify GREEN**

Run:

```bash
go test ./cmd/roborev -run 'TestSkillsInstall' -count=1
```

Expected: PASS.

- [ ] **Step 9: Update user documentation**

In `docs/guides/agent-skills.md`, document:

```bash
roborev skills install --path ~/.pi/agent/skills/
roborev skills install --path /custom/skills --agent codex
```

State that the path is the final skills directory, Claude is the default variant, shells expand `~`, and custom destinations are refreshed by rerunning the same command. Add an improvement bullet to the current top changelog section.

- [ ] **Step 10: Format and run focused verification**

Run:

```bash
gofmt -w cmd/roborev/skills.go cmd/roborev/skills_test.go internal/skills/skills.go internal/skills/skills_test.go
go test ./internal/skills ./cmd/roborev -count=1
```

Expected: PASS.

- [ ] **Step 11: Run repository quality gates**

Run:

```bash
go test ./...
go build ./...
make lint-ci
```

Expected: all commands exit successfully.

- [ ] **Step 12: Commit the implementation**

Review `git diff` and `git status`, stage only the implementation, tests, docs, and plan, then create a new commit. Do not stage `.kata.toml`.
