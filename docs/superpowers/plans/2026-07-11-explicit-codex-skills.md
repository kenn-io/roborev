# Explicit Codex Skill Invocation Implementation Plan

> **For agentic workers:** REQUIRED: Use superpowers:subagent-driven-development (if subagents available) or superpowers:executing-plans to implement this plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make every bundled Codex roborev skill machine-enforced explicit-only and add deterministic plus live Codex conformance coverage.

**Architecture:** Add Codex-native `agents/openai.yaml` policy files beside each skill and extend roborev's embedded installer/status logic to manage them. Keep exact-invocation wording as compatibility defense, then evaluate the boundary through Codex JSONL command events with a stubbed `roborev` executable.

**Tech Stack:** Go embedding/filesystem APIs, testify, Codex CLI JSONL, Make.

---

## File Map

- `internal/skills/codex/*/agents/openai.yaml`: authoritative Codex activation policy.
- `internal/skills/codex/*/SKILL.md`: explicit-only compatibility wording.
- `internal/skills/claude/*/SKILL.md`, `internal/skills/droid/*/SKILL.md`: generated wording variants where the existing derivation applies.
- `internal/skills/skills.go`: embed, install, and compare optional Codex metadata.
- `internal/skills/skills_test.go`: deterministic policy/install/update/status tests.
- `internal/skills/testmain_test.go`: required git-test isolation wrapper.
- `internal/skills/codex_conformance_eval_test.go`: opt-in live Codex behavioral eval.
- `.codex-plugin/plugin.json`: namespaced explicit default prompts.
- `Makefile`: opt-in eval target and model selection.
- `docs/guides/agent-skills.md`, `docs/development.md`: user and contributor contract.

### Task 1: Lock the machine-readable policy contract

**Files:**
- Modify: `internal/skills/skills_test.go`
- Create: `internal/skills/codex/*/agents/openai.yaml`
- Modify: `internal/skills/skills.go`

- [ ] **Step 1: Write the failing embedded-policy test**

Add a test that enumerates Codex skill directories and requires an embedded
`agents/openai.yaml` for each with exactly:

```yaml
policy:
  allow_implicit_invocation: false
```

Use `require.*`/`assert.*` and ensure all nine Codex skills are covered.

- [ ] **Step 2: Run the focused test and verify RED**

Run:

```bash
go test ./internal/skills -run TestCodexSkillsDisableImplicitInvocation -count=1
```

Expected: FAIL because no policy files are embedded.

- [ ] **Step 3: Add the policy files and embed pattern**

Create the same `agents/openai.yaml` under every Codex skill and extend the
Codex `go:embed` directive to include them.

- [ ] **Step 4: Run the focused test and verify GREEN**

Run the command from Step 2. Expected: PASS.

### Task 2: Install and track Codex metadata

**Files:**
- Modify: `internal/skills/skills.go`
- Modify: `internal/skills/skills_test.go`

- [ ] **Step 1: Write failing install and status tests**

Cover these behaviors:

```text
Codex install writes agents/openai.yaml for every skill.
An installation with current SKILL.md but no policy is outdated.
A changed policy is outdated.
Update adds policy metadata to an older SKILL.md-only installation.
Claude and Droid installs do not gain Codex metadata.
```

- [ ] **Step 2: Run the focused tests and verify RED**

```bash
go test ./internal/skills -run 'TestInstall.*OpenAI|TestStatus.*OpenAI|TestUpdate.*OpenAI' -count=1
```

Expected: FAIL because only `SKILL.md` is copied and compared.

- [ ] **Step 3: Implement optional embedded metadata**

Extend `embeddedSkill` with optional `OpenAIYAML []byte`. Read
`<agent>/<skill>/agents/openai.yaml` when present. During install, create the
`agents` directory and write `openai.yaml`. During status, require every
embedded file to exist and match; preserve `SKILL.md` as the installation
presence signal used by `IsInstalled` and `Update`.

- [ ] **Step 4: Run focused and package tests**

```bash
go test ./internal/skills -count=1
```

Expected: PASS.

### Task 3: Add explicit-only wording and plugin entry points

**Files:**
- Modify: `internal/skills/codex/*/SKILL.md`
- Regenerate: `internal/skills/claude/*/SKILL.md`, `internal/skills/droid/*/SKILL.md`
- Modify: `.codex-plugin/plugin.json`
- Modify: `internal/skills/skills_test.go`

- [ ] **Step 1: Write failing wording/default-prompt tests**

Require each Codex description to begin with `Use only when the user explicitly
invokes $<skill-name>`. Decode `.codex-plugin/plugin.json` and assert these
exact default-prompt mappings rather than a shared prefix:

```text
branch review -> $roborev:roborev-review-branch
fix findings  -> $roborev:roborev-fix
respond       -> $roborev:roborev-respond
```

- [ ] **Step 2: Verify RED**

```bash
go test ./internal/skills -run 'TestCodexSkillDescriptionsAreExplicitOnly|TestCodexPluginDefaultPromptsAreExplicit' -count=1
```

Expected: FAIL on the current broad descriptions and natural-language plugin
prompts.

- [ ] **Step 3: Update skill wording and plugin prompts**

Place an `Explicit invocation only` section near the top of each Codex body.
State that ordinary requests must use the agent's native workflow and must not
run roborev. Include the concrete non-trigger most relevant to each workflow.
Update plugin defaults to literal namespaced invocations.

- [ ] **Step 4: Regenerate derived skills**

```bash
go generate ./internal/skills
```

- [ ] **Step 5: Verify GREEN**

```bash
go test ./internal/skills -count=1
```

Expected: PASS, including derived-file freshness.

- [ ] **Step 6: Commit the policy, installer, wording, and plugin entry points**

Use the mandatory commit workflow. This checkpoint leaves all installed Codex
skills explicit-only and independently testable before adding the external
model eval.

### Task 4: Add the live Codex conformance eval

**Files:**
- Create: `internal/skills/testmain_test.go`
- Create: `internal/skills/codex_conformance_eval_test.go`
- Modify: `Makefile`

- [ ] **Step 1: Write failing parser and command-classifier table tests**

Under the `codexeval` build tag, add deterministic tests for:

```text
item.completed + nested command_execution -> command collected
item.started or non-command completed item -> ignored
malformed JSONL -> explicit parse error
direct roborev review invocation -> matched
/bin/zsh -lc wrapped and quoted invocation -> matched
command -v roborev -> ignored
rg/printf/prose mentioning roborev -> ignored
unrelated roborev subcommand -> not matched as branch review
```

Run:

```bash
go test -tags=codexeval ./internal/skills \
  -run 'TestParseCodexCommandEvents|TestClassifyRoborevWorkflowCommand' -count=1
```

Expected: FAIL because the parser and classifier do not exist.

- [ ] **Step 2: Implement and verify the parser/classifier helpers**

Implement only enough JSONL decoding and shell-wrapper normalization to pass
the table tests, then rerun the Step 1 command and expect PASS. These helpers
must remain offline and must not inspect authentication or invoke Codex.

- [ ] **Step 3: Add the isolated live eval fixture**

Gate it behind build tag `codexeval` and
`ROBOREV_RUN_CODEX_SKILL_EVAL=1`. The test must:

1. In `TestMain`, capture the authenticated source `CODEX_HOME` before calling
   `testenv.RunIsolatedMain`, which intentionally clears that variable. Store
   the resolved path only in a package variable; do not export or log it.
2. Resolve `codex`. Create an isolated `CODEX_HOME`, copy `auth.json` from the
   captured source home with its restrictive permission bits preserved, and
   install the in-tree Codex skills. Never symlink authentication: token refresh
   must be able to mutate only the disposable copy. Authentication errors must
   identify the missing prerequisite without printing either path.
3. Create a disposable git repository with a reviewable change.
4. Put a harmless `roborev` stub first on `PATH`. Immediately before each
   model/case subprocess, rewrite it with a fresh, unpredictable output marker
   that is not included in the prompt or repository fixture.
5. Run `codex exec --json --ephemeral --ignore-user-config --ignore-rules`
   with read-only sandbox and approval disabled.
6. Parse top-level completed-item JSONL events (`type == "item.completed"`),
   then select nested items where `item.type == "command_execution"` and read
   `item.command`.
7. Skip the live test on native Windows before model execution because its
   isolation contract depends on POSIX login-shell behavior. Tagged offline
   tests must still cross-compile for Windows.

Add `TestMain` using `testenv.RunIsolatedMain` because the eval runs git.

- [ ] **Step 4: Add negative and positive cases**

For each model in `ROBOREV_CODEX_SKILL_EVAL_MODELS`:

```go
{prompt: "Review the changes in this branch.", wantRoborev: false}
{prompt: "Fix the issues you find in this branch.", wantRoborev: false}
{prompt: "$roborev-review-branch", wantCommand: "roborev review --branch --wait"}
```

Use the unique per-run stub marker as the negative execution oracle: any
completed command event whose aggregated output contains the exact marker means
the implicit prompt executed roborev, regardless of shell aliases, grouping,
wrappers, command text, status, or exit code. Diagnostics and prose do not print
the marker and therefore remain harmless. Also reject any command event that
the focused classifier confidently identifies as a direct roborev execution,
including an absolute executable path; classification uncertainty fails the
eval. The positive case must match exactly the four ordered tokens `roborev
review --branch --wait` and contain that same event's marker with completed
status and exit code zero.

- [ ] **Step 5: Add the Make target**

Add `test-codex-skill-eval` to `.PHONY`, with `CODEX_SKILL_EVAL_MODELS`
defaulting to `gpt-5.6-sol`. Its recipe must be:

```make
ROBOREV_RUN_CODEX_SKILL_EVAL=1 \
ROBOREV_CODEX_SKILL_EVAL_MODELS="$(CODEX_SKILL_EVAL_MODELS)" \
  go test -tags=codexeval ./internal/skills \
    -run TestCodexSkillExplicitInvocation -count=1 -v
```

- [ ] **Step 6: Run the live eval**

```bash
make test-codex-skill-eval CODEX_SKILL_EVAL_MODELS='gpt-5.5,gpt-5.6-sol'
```

Expected after the policy fix: both models avoid roborev for negative prompts
and invoke it for the explicit positive prompt.

- [ ] **Step 7: Commit the live eval harness**

Use the mandatory commit workflow. This checkpoint contains only the opt-in,
isolated conformance surface and its Make target.

### Task 5: Document the contract

**Files:**
- Modify: `docs/guides/agent-skills.md`
- Modify: `docs/development.md`

- [ ] **Step 1: Update user documentation**

State that Codex skills are never selected from ordinary natural-language task
requests. Document personal `$roborev-*`, plugin
`$roborev:roborev-*`, and structured skill-picker invocation.

- [ ] **Step 2: Update contributor documentation**

Document the live eval prerequisites, model override, isolation guarantees,
and the fact that it incurs model usage.

- [ ] **Step 3: Run documentation checks**

```bash
make docs-check
```

Expected: PASS.

- [ ] **Step 4: Commit the documentation**

Use the mandatory commit workflow so the public contract and contributor eval
instructions form their own reviewable checkpoint.

### Task 6: Verify and commit the implementation

**Files:** all files above.

- [ ] **Step 1: Run quality gates**

```bash
go generate ./internal/skills
go test ./internal/skills -count=1
make test-git-isolation
go test ./...
go build ./...
make lint-ci
make docs-check
```

- [ ] **Step 2: Audit the completion contract**

Verify all nine policy files are embedded and installed, status detects missing
metadata, negative live evals make no roborev command, explicit invocation does,
plugin prompts use namespaced syntax, and no unrelated files changed.

- [ ] **Step 3: Verify the logical commits without amending**

Confirm the implementation is split into the policy/installer checkpoint, live
eval checkpoint, and documentation checkpoint defined above. If quality-gate
fixes are needed, commit them as a new commit; never amend an existing one.

- [ ] **Step 4: Sync and push**

```bash
git pull --rebase
git push
git status --short --branch
```

Expected: branch is clean and up to date with its upstream.
