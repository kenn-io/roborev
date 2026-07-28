# Multiple ACP Agents and Goose Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Support multiple named ACP agents and validate one end-to-end review
through Goose authenticated with a ChatGPT subscription.

**Architecture:** Decode `[acp.<name>]` into maps, resolve repository entries by
name over global entries, and preserve each configured ACP name at runtime.
Agent/model/backup/synthesis paths receive the selected name so one ACP entry
cannot borrow another entry's command or model. Goose remains a normal external
ACP server launched as `goose acp`.

**Tech Stack:** Go, BurntSushi TOML, testify, ACP Go SDK v0.13.5, mise, Goose
ACP v1, Zensical Markdown.

## Global Constraints

- Use `[acp.<name>]`; the table key is the agent name and there is no nested
  `name` field.
- Repository ACP entries replace complete same-name global entries and retain
  unrelated global entries.
- Do not add a compatibility reader for the old singleton `[acp]` shape.
- Preserve the bare built-in `acp` adapter as distinct from configured names.
- Reject configured names that collide with built-in names or aliases.
- Keep `github.com/coder/acp-go-sdk` v0.13.5 unless the Goose acceptance test
  proves a concrete incompatibility; ACP v2 is alpha and out of scope.
- Never store ChatGPT OAuth credentials in roborev configuration or the repo.
- Build branch code into scratch space and isolate `ROBOREV_DATA_DIR`; never
  replace the installed roborev binary or touch the live review database.
- Respect mise's configured release-age policy.
- Use testify assertions and test only owned resolution/validation behavior.

---

### Task 1: Named ACP Configuration and Merge Semantics

**Files:**

- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`

**Interfaces:**

- Produces: `type ACPAgentConfigs map[string]ACPAgentConfig`
- Produces: `ResolveACPAgentConfig(name, repoPath string, globalCfg *Config) (ACPAgentConfig, bool)`
- Produces: `ResolveACPAgentConfigFromConfig(name string, repoCfg *RepoConfig, globalCfg *Config) (ACPAgentConfig, bool)`
- Produces: `ResolveACPAgentConfigsFromConfig(repoCfg *RepoConfig, globalCfg *Config) ACPAgentConfigs`

- [ ] **Step 1: Write focused failing configuration tests**

Add tests that exercise roborev-owned semantics rather than TOML itself:

```go
func TestResolveACPAgentConfigFromConfigMergesByName(t *testing.T) {
    global := &Config{ACP: ACPAgentConfigs{
        "goose": {Command: "global-goose", Model: "global-model"},
        "foo":   {Command: "foo-acp"},
    }}
    repo := &RepoConfig{ACP: ACPAgentConfigs{
        "goose": {Command: "repo-goose"},
    }}

    goose, ok := ResolveACPAgentConfigFromConfig("goose", repo, global)
    require.True(t, ok)
    assert.Equal(t, "repo-goose", goose.Command)
    assert.Empty(t, goose.Model)

    foo, ok := ResolveACPAgentConfigFromConfig("foo", repo, global)
    require.True(t, ok)
    assert.Equal(t, "foo-acp", foo.Command)
}
```

Also update an existing config-load test to use `[acp.goose]` and assert the
decoded command/model through the resolver. Do not add a serialization
round-trip test.

- [ ] **Step 2: Run the focused tests and confirm the old singleton types fail**

Run:

```bash
go test ./internal/config -run 'TestResolveACPAgentConfig|TestLoad.*ACP' -count=1
```

Expected: compilation or assertion failure because `ACP` is still a pointer and
resolution is not name-aware.

- [ ] **Step 3: Implement the map types and lookup functions**

Change both `Config.ACP` and `RepoConfig.ACP` to `ACPAgentConfigs`. Remove
`ACPAgentConfig.Name`. Implement complete-entry repository precedence:

```go
func ResolveACPAgentConfigFromConfig(
    name string, repoCfg *RepoConfig, globalCfg *Config,
) (ACPAgentConfig, bool) {
    name = strings.TrimSpace(name)
    if name == "" {
        return ACPAgentConfig{}, false
    }
    if repoCfg != nil {
        if cfg, ok := repoCfg.ACP[name]; ok {
            return cfg, true
        }
    }
    if globalCfg != nil {
        cfg, ok := globalCfg.ACP[name]
        return cfg, ok
    }
    return ACPAgentConfig{}, false
}
```

The all-config resolver copies global entries and overwrites them with repo
entries so callers cannot mutate source configuration accidentally.

- [ ] **Step 4: Run configuration tests**

Run:

```bash
go test ./internal/config -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit the configuration model**

Use the mandatory commit skill, inspect the complete diff/status, and commit the
config types, resolvers, and tests without bypassing hooks.

### Task 2: Preserve Named ACP Runtime Identity

**Files:**

- Modify: `internal/agent/acp_agent.go`
- Modify: `internal/agent/acp_resolution.go`
- Modify: `internal/agent/acp_test.go`
- Modify: `internal/agent/acp_resolution_test.go`

**Interfaces:**

- Consumes: name-aware config resolvers from Task 1
- Produces: `NewACPAgentFromConfig(name string, cfg *config.ACPAgentConfig) *ACPAgent`
- Produces: configured ACP agents whose `Name()` is the `[acp.<name>]` key
- Produces: exact-name discovery and selection across all configured ACP entries

- [ ] **Step 1: Rewrite selection tests for two simultaneous ACP agents**

Use two temporary executable commands and assert both identities and commands:

```go
cfg := &config.Config{ACP: config.ACPAgentConfigs{
    "goose": {Command: gooseBin},
    "foo":   {Command: fooBin},
}}

goose, err := GetAvailableExactWithConfigFromConfig(nil, "goose", cfg)
require.NoError(t, err)
assert.Equal(t, "goose", goose.Name())
assert.Equal(t, gooseBin, goose.(CommandAgent).CommandName())

foo, err := GetAvailableExactWithConfigFromConfig(nil, "foo", cfg)
require.NoError(t, err)
assert.Equal(t, "foo", foo.Name())
assert.Equal(t, fooBin, foo.(CommandAgent).CommandName())
```

Add focused cases for a configured agent as a backup, repo replacement of one
agent, an empty command, and collisions with a built-in name and alias. Update
existing ACP tests to expect the configured key rather than canonical `acp`.

- [ ] **Step 2: Run agent tests and confirm failures**

Run:

```bash
go test ./internal/agent -run 'ACP|WorkflowModel' -count=1
```

Expected: FAIL because current resolution reads one singleton and rewrites its
runtime name to `acp`.

- [ ] **Step 3: Implement exact named resolution**

Refactor configured-agent helpers to take a name. `isConfiguredACPAgentName`
checks exact map membership. `configuredACPAgentFromConfig` returns an error for
blank commands and names whose canonical alias or registry entry collides with
a built-in agent. Construct agents with the requested key:

```go
func NewACPAgentFromConfig(name string, cfg *config.ACPAgentConfig) *ACPAgent {
    agent := NewACPAgent(cfg.Command)
    agent.agentName = strings.TrimSpace(name)
    // copy args, modes, model, and timeout as today
    return agent
}
```

Update preferred, exact, backup, availability, and fallback resolution to pass
the requested key. Include configured ACP keys in config-aware unknown-agent
diagnostics. Remove singleton override helpers that no longer have callers.

- [ ] **Step 4: Run the full agent package**

Run:

```bash
go test ./internal/agent -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit named runtime selection**

Use the mandatory commit skill and commit only the agent runtime/resolution
changes and their tests.

### Task 3: Isolate Models, Backups, CI, and Synthesis by ACP Name

**Files:**

- Modify: `internal/agent/model_resolution.go`
- Modify: `internal/agent/model_resolution_test.go`
- Modify: `internal/daemon/synthesis_worker.go`
- Modify: `internal/daemon/synthesis_worker_test.go`
- Modify: `internal/daemon/worker_test.go`
- Modify: `internal/daemon/ci_poller_test.go`
- Modify: `cmd/roborev/refine_test.go`
- Modify: `internal/review/synthesize_test.go`

**Interfaces:**

- Consumes: runtime configured ACP names from Task 2
- Produces: `WorkflowConfig.resolveACPAgentConfig(selectedAgent string)`
- Produces: model pairing that treats `goose` and `foo` as different agents

- [ ] **Step 1: Add a model-isolation regression test**

Create global `goose` and `foo` entries with different models, select each in
separate `WorkflowConfig` calls, and assert only its own model is returned. Add
a backup case where a model paired with `foo` is not handed to `goose`.

- [ ] **Step 2: Update daemon seam tests to retain configured names**

Change synthesis and CI expectations from `acp` to the configured key. Add one
synthesis test with two configured ACP entries to prove the stored selected
agent determines the command/model.

- [ ] **Step 3: Run targeted tests and confirm failures**

Run:

```bash
go test ./internal/agent ./internal/daemon ./internal/review ./cmd/roborev \
  -run 'ACP|Synthesis|BackupModel|RefineAgent' -count=1
```

Expected: FAIL where model comparison still normalizes every configured ACP
agent to `acp` and synthesis still consults a singleton.

- [ ] **Step 4: Make model and daemon resolution name-aware**

Pass `selectedAgent` into ACP config lookup. For configured agents,
`workflowModelComparableAgentNameFromConfig` returns the exact configured name
instead of `defaultACPName`. Simplify synthesis matching to compare the stored
configured name directly; remove the singleton-only special case. Migrate all
test fixtures from pointer ACP config to named maps.

- [ ] **Step 5: Run affected packages**

Run:

```bash
go test ./internal/config ./internal/agent ./internal/daemon ./internal/review ./cmd/roborev -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit cross-workflow ACP isolation**

Use the mandatory commit skill and commit the model/daemon/caller migrations.

### Task 4: User Documentation and Goose Examples

**Files:**

- Modify: `docs/advanced/acp.md`
- Modify: `docs/agents/index.md`
- Modify: `docs/configuration.md`
- Modify: `docs/commands.md` if selection wording needs clarification
- Modify: `docs/changelog.md`

**Interfaces:**

- Documents: `[acp.<name>]`, merge/override rules, and exact agent selection
- Documents: mise installation and ChatGPT Codex Goose authentication

- [ ] **Step 1: Replace singleton examples with named tables**

Every current `[acp]` example becomes a named entry such as
`[acp.codex-acp]` or `[acp.agy-sdk]`; remove the repeated `name` field. Explain
that a repo table replaces only the same global key.

- [ ] **Step 2: Add the complete Goose walkthrough**

Include these copyable commands and configuration:

```bash
mise use --global --pin github:aaif-goose/goose@latest
goose --version
goose configure
```

Tell the reader to choose **Configure Providers**, **ChatGPT Codex**, complete
browser OAuth, and retain Goose's configured default model. Register it with:

```toml
[acp.goose]
command = "goose"
args = ["acp"]
```

Show these use cases:

```bash
roborev review HEAD --agent goose --panel none
```

```toml
review_agent = "goose"
review_backup_agent = "codex"
```

```toml
# .roborev.toml: replace only the global goose entry
[acp.goose]
command = "/opt/project/bin/goose-wrapper"
args = ["acp"]
```

Also show a second `[acp.foo]` entry so the multi-agent capability is obvious.
Do not include OAuth tokens, local usernames/paths, or a hard-coded Goose model.

- [ ] **Step 3: Update reference prose and changelog**

Replace `[acp].model` wording with the selected agent's
`[acp.<name>].model`. Add an Unreleased feature note for multiple ACP agents and
the Goose example.

- [ ] **Step 4: Format and validate documentation**

Run:

```bash
make markdown
make markdown-ci
make docs-check
```

Expected: PASS and no unrelated documentation changes.

- [ ] **Step 5: Commit public documentation**

Use the mandatory commit skill and commit the user-facing docs/changelog.

### Task 5: Install and Configure Goose, Then Attempt a Review

**Files and user state:**

- Modify intentionally: `~/.config/mise/config.toml`
- Create intentionally: mise-managed Goose installation
- Modify intentionally: Goose user configuration/OAuth state
- Modify intentionally: `~/.roborev/config.toml`
- Create: owner-private recovery backups outside the repository

**Interfaces:**

- Consumes: branch implementation from Tasks 1-4
- Produces: an installed `goose` command and live `[acp.goose]` entry

- [ ] **Step 1: Re-read production-isolation instructions and inspect targets**

Confirm the exact roborev processes/binary/database, Goose absence, mise config,
and config file modes without printing secrets. Create owner-private timestamped
backups of the mise, Goose (if created during the flow), and roborev configs
before changing them. Do not stop or restart the live daemon.

- [ ] **Step 2: Install the newest release permitted by mise policy**

Resolve the visible latest version, then install it globally with a pin. Do not
override `minimum_release_age`:

```bash
mise use --global --pin github:aaif-goose/goose@<visible-version>
```

Verify `mise which goose` and `goose --version`.

- [ ] **Step 3: Inspect Goose command surfaces**

Capture sanitized output from:

```bash
goose --help
goose acp --help
goose configure --help
```

Confirm `goose acp` is a stdio ACP server.

- [ ] **Step 4: Configure ChatGPT Codex authentication**

Run `goose configure` in a PTY, choose the ChatGPT Codex provider, and complete
the browser OAuth flow. If the account-consent step requires user interaction,
pause with the exact prompt rather than handling credentials. Verify provider
status using Goose's non-secret info/doctor command.

- [ ] **Step 5: Back up and edit live roborev config**

Add only:

```toml
[acp.goose]
command = "goose"
args = ["acp"]
```

Parse the resulting TOML and report only its redacted shape. Do not restart the
installed daemon because it predates the named-map implementation.

- [ ] **Step 6: Build and run branch roborev in isolation**

Create a mode-0700 scratch directory, set an isolated `ROBOREV_DATA_DIR`, write
only the Goose ACP entry to its scratch config, and build the branch binary into
the scratch directory. Run a foreground, read-only single-agent review using
the branch binary and the user-configured Goose OAuth state. Do not use the live
daemon or database.

- [ ] **Step 7: Diagnose only concrete ACP incompatibilities**

If Goose completes, retain v0.13.5. If protocol negotiation fails, capture the
sanitized JSON-RPC/error boundary, compare it to ACP v1, and implement the
smallest supported fix with a regression test. Do not adopt ACP v2 alpha.

### Task 6: Final Verification and Branch Handoff

**Files:**

- Verify all modified source, test, and documentation files
- Remove `docs/superpowers/` working documents before a PR if a PR is requested

- [ ] **Step 1: Run repository quality gates**

Run:

```bash
go test ./...
go build ./...
make lint-ci
make markdown-ci
git diff --check
```

Expected: every command exits successfully.

- [ ] **Step 2: Audit the final diff and configuration references**

Use `rg` to confirm production docs/source no longer describe the singleton
format. This is a manual deletion audit, not a new test. Inspect `git diff`,
`git status`, and commit all related files using the mandatory commit skill.

- [ ] **Step 3: Follow repository session-completion requirements**

Ensure no follow-up work remains, pull/rebase only if permitted by the current
user instructions, push the branch, and verify status reports it up to date with
origin. Never merge a pull request.
