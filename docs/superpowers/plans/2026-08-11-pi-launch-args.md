# Pi Launch Arguments Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan directly in the current agent, task-by-task. Never use subagent-driven development. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add global, tokenized `launch_args` configuration that is prepended to every built-in Pi invocation, allowing explicit provider extensions to coexist with roborev-managed workflow and safety arguments.

**Approved spec/design:** `docs/superpowers/specs/2026-08-11-pi-launch-args-design.md`

**Architecture:** Extend the existing Pi-specific config and agent value objects with an owned `[]string` argument slice. Agent resolution copies the configured slice, and both Pi argument builders start with that slice before appending roborev-managed arguments, following the existing Codex-style ordering for well-formed options.

**Tech Stack:** Go, BurntSushi TOML configuration, Testify assertions, Zensical Markdown documentation.

**Execution precondition:** This reviewed plan is committed before Task 1 begins.

## Global Constraints

- `launch_args` is global under `[agent.pi]` and applies to every built-in Pi invocation.
- Each TOML array element becomes exactly one process argument; roborev performs no shell parsing, interpolation, or word splitting.
- User launch arguments precede roborev-managed model, reasoning, session, output, and safety arguments.
- Parser-control tokens, early-exit flags, and options missing required values are unsupported; extension-defined flags make generic argv validation impractical.
- The classifier retains `--no-extensions`; explicitly supplied `--extension` arguments remain additive.
- Do not add a shared raw-argument abstraction for other built-in agents.
- Do not add dependencies or test Pi's external extension-deduplication behavior.
- Use Testify for every new assertion and preserve the original agent and config slices when cloning.

---

### Task 1: Carry Pi launch arguments from configuration into every invocation

**Files:**
- Modify: `internal/config/config.go:87-89`
- Modify: `internal/config/config_test.go:146-159`
- Modify: `internal/agent/spec.go:260-275`
- Modify: `internal/agent/spec_test.go:87-103`
- Modify: `internal/agent/pi.go:21-164`
- Modify: `internal/agent/pi_test.go:58-117`

**Interfaces:**
- Consumes: `config.PiConfig`, `applyAgentConfigOverrides(Agent, *config.Config) Agent`, `PiAgent.clone(...agentCloneOption) *PiAgent`, `PiAgent.buildArgs(string) []string`, and `PiAgent.classifyArgs(string, string, json.RawMessage) []string`.
- Produces: `config.PiConfig.LaunchArgs []string` and `agent.PiAgent.LaunchArgs []string`, both copied at configuration and clone boundaries.

- [ ] **Step 1: Write failing configuration and resolution tests**

Replace `TestLoadGlobalPiJSONSchemaExtension` in `internal/config/config_test.go` with a test covering the complete Pi config:

```go
func TestLoadGlobalPiConfig(t *testing.T) {
	testenv.SetDataDir(t)

	path := filepath.Join(DataDir(), "config.toml")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(`[agent.pi]
jsonschemaextension = "/opt/roborev/pi-json-schema/index.ts"
launch_args = ["--extension", "npm:@example/pi-provider"]
`), 0o600))

	cfg, err := LoadGlobalFrom(path)
	require.NoError(t, err)
	assert.Equal(t, "/opt/roborev/pi-json-schema/index.ts", cfg.Agent.Pi.JSONSchemaExtension)
	assert.Equal(t, []string{"--extension", "npm:@example/pi-provider"}, cfg.Agent.Pi.LaunchArgs)
}
```

Replace `TestApplyAgentConfigOverridesPiJSONSchemaExtension` in `internal/agent/spec_test.go` with a copy-isolation test:

```go
func TestApplyAgentConfigOverridesPiConfig(t *testing.T) {
	t.Parallel()

	base := NewPiAgent("pi")
	cfg := &config.Config{
		Agent: config.AgentConfig{
			Pi: config.PiConfig{
				JSONSchemaExtension: "/opt/roborev/pi-json-schema/index.ts",
				LaunchArgs:          []string{"--extension", "npm:@example/pi-provider"},
			},
		},
	}
	overridden := applyAgentConfigOverrides(base, cfg)

	pi, ok := overridden.(*PiAgent)
	require.True(t, ok)
	assert.Equal(t, "/opt/roborev/pi-json-schema/index.ts", pi.JSONSchemaExtension)
	assert.Equal(t, []string{"--extension", "npm:@example/pi-provider"}, pi.LaunchArgs)
	assert.Equal(t, config.DefaultPiJSONSchemaExtension, base.JSONSchemaExtension)
	assert.Empty(t, base.LaunchArgs)

	cfg.Agent.Pi.LaunchArgs[1] = "changed"
	assert.Equal(t, []string{"--extension", "npm:@example/pi-provider"}, pi.LaunchArgs)
}
```

Add the clone-isolation regression test to `internal/agent/pi_test.go` before changing `PiAgent.clone`:

```go
func TestPiCloneOwnsLaunchArgs(t *testing.T) {
	t.Parallel()

	base := NewPiAgent("pi")
	base.LaunchArgs = []string{"--extension", "npm:@example/pi-provider"}
	clone := base.WithSessionID("session-123").(*PiAgent)

	base.LaunchArgs[1] = "changed"
	assert.Equal(t, []string{"--extension", "npm:@example/pi-provider"}, clone.LaunchArgs)
}
```

- [ ] **Step 2: Run the focused tests and verify that the new fields are missing**

Run:

```bash
go test ./internal/config ./internal/agent -run 'TestLoadGlobalPiConfig|TestApplyAgentConfigOverridesPiConfig|TestPiCloneOwnsLaunchArgs'
```

Expected: compilation fails because `PiConfig.LaunchArgs` and `PiAgent.LaunchArgs` do not exist.

- [ ] **Step 3: Add configuration and agent-resolution support**

Extend `PiConfig` in `internal/config/config.go`:

```go
type PiConfig struct {
	JSONSchemaExtension string   `toml:"jsonschemaextension" comment:"Pi extension source for classifier JSON schema output."`
	LaunchArgs          []string `toml:"launch_args" comment:"Additional arguments prepended to every Pi invocation."`
}
```

Add the owned field to `PiAgent` in `internal/agent/pi.go` and copy it in `clone`:

```go
type PiAgent struct {
	Command             string
	Model               string
	Provider            string
	Reasoning           ReasoningLevel
	Agentic             bool
	SessionID           string
	JSONSchemaExtension string
	LaunchArgs          []string
}
```

```go
		JSONSchemaExtension: a.JSONSchemaExtension,
		LaunchArgs:          slices.Clone(a.LaunchArgs),
```

Add `slices` to the `internal/agent/pi.go` imports. Update the Pi branch of `applyAgentConfigOverrides` in `internal/agent/spec.go` so empty config preserves defaults and configured slices are owned:

```go
	case *PiAgent:
		ext := strings.TrimSpace(cfg.Agent.Pi.JSONSchemaExtension)
		if ext == "" {
			ext = agent.JSONSchemaExtension
		}
		launchArgs := slices.Clone(cfg.Agent.Pi.LaunchArgs)
		if ext == agent.JSONSchemaExtension && slices.Equal(launchArgs, agent.LaunchArgs) {
			return a
		}
		clone := *agent
		clone.JSONSchemaExtension = ext
		clone.LaunchArgs = launchArgs
		return &clone
```

- [ ] **Step 4: Run the focused configuration tests and verify they pass**

Run:

```bash
go test ./internal/config ./internal/agent -run 'TestLoadGlobalPiConfig|TestApplyAgentConfigOverridesPiConfig|TestPiCloneOwnsLaunchArgs'
```

Expected: PASS.

- [ ] **Step 5: Write failing invocation and clone-ordering tests**

Add `slices` to the `internal/agent/pi_test.go` imports, then add an exact argv test. It checks both the configured prefix and the unchanged managed argv for empty configuration:

```go
func TestPiLaunchArgsPrecedeManagedArgsForEveryInvocation(t *testing.T) {
	t.Parallel()

	wantPrefix := []string{"--extension", "npm:@example/pi-provider"}
	base := NewPiAgent("pi")
	base.Provider = "cpa"
	base.Model = "gpt-test"
	base.Reasoning = ReasoningFast
	configured := *base
	configured.LaunchArgs = slices.Clone(wantPrefix)
	schema := jsonRaw(`{"type":"object"}`)
	classifyInstruction := "Classify according to the attached instructions and write the result with the structured JSON output tool."

	tests := []struct {
		name          string
		withoutLaunch []string
		withLaunch    []string
		managed       []string
	}{
		{
			name:          "review",
			withoutLaunch: base.buildArgs(""),
			withLaunch:    configured.buildArgs(""),
			managed:       []string{"-p", "--mode", "json", "--provider", "cpa", "--model", "gpt-test", "--thinking", "low"},
		},
		{
			name:          "resumed review",
			withoutLaunch: base.buildArgs("/tmp/pi-session.jsonl"),
			withLaunch:    configured.buildArgs("/tmp/pi-session.jsonl"),
			managed:       []string{"-p", "--mode", "json", "--session", "/tmp/pi-session.jsonl", "--provider", "cpa", "--model", "gpt-test", "--thinking", "low"},
		},
		{
			name:          "classifier",
			withoutLaunch: base.classifyArgs("prompt.md", "result.json", schema),
			withLaunch:    configured.classifyArgs("prompt.md", "result.json", schema),
			managed: []string{
				"--no-session", "--no-extensions", "--no-builtin-tools", "--no-skills",
				"--no-prompt-templates", "--no-themes", "--no-context-files",
				"--extension", config.DefaultPiJSONSchemaExtension,
				"--json-schema", string(schema), "--json-output", "result.json",
				"--json-fallback", "none", "-p", "--provider", "cpa",
				"--model", "gpt-test", "--thinking", "low", "@prompt.md", classifyInstruction,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.managed, tt.withoutLaunch)
			wantConfigured := append(slices.Clone(wantPrefix), tt.managed...)
			assert.Equal(t, wantConfigured, tt.withLaunch)
		})
	}
}
```

- [ ] **Step 6: Run the invocation tests and verify launch arguments are absent**

Run:

```bash
go test ./internal/agent -run 'TestPiLaunchArgsPrecedeManagedArgsForEveryInvocation'
```

Expected: the empty-configuration assertions pass, while each configured invocation fails because its launch-argument prefix is absent.

- [ ] **Step 7: Prepend launch arguments in both Pi argument builders**

Change `buildArgs` in `internal/agent/pi.go`:

```go
func (a *PiAgent) buildArgs(sessionPath string) []string {
	args := slices.Clone(a.LaunchArgs)
	args = append(args, "-p", "--mode", "json")
	// Keep the existing session, provider, model, and thinking appends unchanged.
```

Change the start of `classifyArgs`:

```go
func (a *PiAgent) classifyArgs(promptPath, outputPath string, schema json.RawMessage) []string {
	args := slices.Clone(a.LaunchArgs)
	args = append(args,
		"--no-session",
		"--no-extensions",
		"--no-builtin-tools",
		"--no-skills",
		"--no-prompt-templates",
		"--no-themes",
		"--no-context-files",
		"--extension", a.jsonSchemaExtension(),
		"--json-schema", string(schema),
		"--json-output", outputPath,
		"--json-fallback", "none",
		"-p",
	)
```

Leave provider, model, thinking, prompt-file, and instruction appends after this block unchanged.

- [ ] **Step 8: Run the Pi invocation tests and verify they pass**

Run:

```bash
go test ./internal/agent -run 'TestPiLaunchArgsPrecedeManagedArgsForEveryInvocation|TestPiCloneOwnsLaunchArgs|TestPiClassifyWithSchemaUsesLockedDownSchemaOutput|TestPiReviewSessionFlag|TestPiCommandLineOmitsResolvedSessionPath'
```

Expected: PASS.

- [ ] **Step 9: Run the affected package suites**

Run:

```bash
go test ./internal/config ./internal/agent
```

Expected: PASS.

- [ ] **Step 10: Commit the behavior change**

```bash
git add internal/config/config.go internal/config/config_test.go internal/agent/spec.go internal/agent/spec_test.go internal/agent/pi.go internal/agent/pi_test.go
git commit -m "feat(agent): add Pi launch arguments"
```

### Task 2: Document Pi launch arguments and run repository quality gates

**Files:**
- Modify: `docs/configuration.md:1154-1174`
- Modify: `docs/agents/index.md:237-259`
- Modify: `docs/changelog.md:9-12`

**Interfaces:**
- Consumes: the `[agent.pi].launch_args` contract implemented in Task 1.
- Produces: user-facing configuration guidance and an Unreleased changelog entry.

- [ ] **Step 1: Document the configuration contract**

After the existing `jsonschemaextension` example in `docs/configuration.md`, add:

````markdown
Additional Pi CLI arguments can be prepended to every Pi invocation with
`launch_args`. Each array entry is passed as one argument without shell parsing.
Roborev appends its managed arguments afterward so its workflow and safety
settings retain precedence for well-formed duplicate options. This ordering is
not a validation boundary: parser-control tokens such as standalone `--`,
early-exit flags, and options missing required values are unsupported and may
prevent Pi from running the managed invocation.

```toml
[agent.pi]
launch_args = [
  "--extension",
  "npm:@example/pi-provider",
]
```

This is useful when a model provider is registered by an extension. Classifier
jobs retain `--no-extensions`, but Pi still loads extensions named explicitly
with `--extension`. The same launch arguments are also passed to normal reviews
and agentic Pi runs.
````

Do not combine a flag and its value into one array element and do not document shell expansion.

- [ ] **Step 2: Add the agent-guide example**

After the existing structured-output override in `docs/agents/index.md`, add a short cross-reference and example:

````markdown
If the selected Pi model is registered by another extension, pass that
extension explicitly on every Pi launch:

```toml
[agent.pi]
launch_args = ["--extension", "npm:@example/pi-provider"]
```

See [Pi Classifier Options](/configuration/#pi-classifier-options) for argument
ordering and tokenization details.
````

- [ ] **Step 3: Add the Unreleased changelog entry**

Under `## Unreleased` in `docs/changelog.md`, add:

```markdown
**New features**

- Pi agents accept global `[agent.pi] launch_args`, passed as tokenized arguments
  to every Pi invocation before roborev-managed workflow and safety options.
  This allows isolated classifier jobs to load extension-defined model
  providers explicitly while retaining `--no-extensions` discovery isolation.
```

- [ ] **Step 4: Format and validate Markdown**

Run:

```bash
make markdown
make markdown-ci
```

Expected: both commands exit successfully and only the three intended user-facing documents change.

- [ ] **Step 5: Run repository quality gates**

Run:

```bash
go test ./...
make lint-ci
```

Expected: both commands exit successfully.

- [ ] **Step 6: Inspect the final diff and working tree**

Run:

```bash
git diff
git status --short
```

Expected: only the three documentation files are uncommitted; Task 1's source and tests are already committed.

- [ ] **Step 7: Commit the documentation**

```bash
git add docs/configuration.md docs/agents/index.md docs/changelog.md
git commit -m "docs: explain Pi launch arguments"
```

- [ ] **Step 8: Verify all work is committed**

Run:

```bash
git status --short --branch
git log --oneline -4
```

Expected: the working tree is clean and the design, plan, implementation, and documentation commits are the latest four commits.

### Task 3: Synchronize and push the completed branch

**Files:**
- No file changes.

**Interfaces:**
- Consumes: the clean, fully validated commits from Tasks 1 and 2.
- Produces: an upstream branch synchronized with `origin/main` and containing every local commit.

- [ ] **Step 1: Confirm there is nothing left to file or commit**

Run:

```bash
git status --porcelain
```

Expected: no output. The approved scope has no remaining follow-up work requiring an issue.

- [ ] **Step 2: Rebase on the current remote default branch and establish the upstream branch**

Run:

```bash
git fetch origin main
git rebase origin/main
git push --set-upstream origin HEAD
```

Expected: the rebase succeeds without changing branches and the current branch is published to `origin`.

- [ ] **Step 3: Execute the repository's final pull/push verification sequence**

Run:

```bash
git pull --rebase
git push
git status
```

Expected: both network commands succeed and status reports that the branch is up to date with its upstream with a clean working tree.
