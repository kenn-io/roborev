# Fix Commit Metadata Implementation Plan

> **For agentic workers:** REQUIRED: Use superpowers:subagent-driven-development (if subagents available) or superpowers:executing-plans to implement this plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement `fix_commit_author` and `fix_commit_co_authored_by` for roborev-owned fix commits, plus best-effort prompt instructions for agent-owned fix commits.

**Architecture:** Config owns metadata resolution and validation. `internal/git` owns Git commit options for roborev-created commits. CLI and TUI code resolve metadata at workflow boundaries, pass commit options into roborev-owned commits, and render prompt instructions for agent-owned commits.

**Tech Stack:** Go, Cobra CLI, Bubble Tea TUI, BurntSushi TOML raw metadata, stdlib `net/mail`, Git CLI.

---

### Task 1: Config Model And Resolution

**Files:**
- Modify: `internal/config/config.go`
- Create: `internal/config/fix_commit_metadata.go`
- Test: `internal/config/config_test.go`

- [ ] **Step 1: Write failing config tests**
  - Add tests for global/repo parsing, independent field precedence, explicit empty repo string/list clearing global values, malformed identity rejection, bare-name rejection, plus-address acceptance, and empty config resolving empty.
  - Run: `go test ./internal/config -run 'TestResolveFixCommitMetadata|TestValidateFixCommitIdentity' -count=1`
  - Expected: FAIL because fields/resolver do not exist.

- [ ] **Step 2: Implement config fields and resolver**
  - Add `FixCommitAuthor string` and `FixCommitCoAuthoredBy []string` to `Config` and `RepoConfig`.
  - Add `FixCommitMetadata`, `ResolveFixCommitMetadata`, `ResolveFixCommitMetadataFrom`, and validation helpers using `net/mail.ParseAddress`.
  - Preserve unset-vs-explicit-empty behavior with `LoadRawGlobal`, `LoadRawRepo`, and `IsKeyInTOMLFile`.

- [ ] **Step 3: Verify config tests pass**
  - Run: `go test ./internal/config -run 'TestResolveFixCommitMetadata|TestValidateFixCommitIdentity' -count=1`
  - Expected: PASS.

### Task 2: Git Commit Options

**Files:**
- Modify: `internal/git/git.go`
- Test: `internal/git/git_test.go`

- [ ] **Step 1: Write failing git tests**
  - Add tests for `CreateCommitWithOptions` author override, multiple co-author trailers, unsupported trailer stderr classification, and hook failure classification with options.
  - Run: `go test ./internal/git -run 'TestCreateCommit.*(Author|CoAuthor|Trailer|Hook)' -count=1`
  - Expected: FAIL because options API does not exist.

- [ ] **Step 2: Implement commit options**
  - Add `CommitOptions{Author string; CoAuthors []string}`.
  - Add `CreateCommitWithOptions(repoPath, message string, opts CommitOptions)`.
  - Keep `CreateCommit` as the zero-options wrapper.
  - Build `git commit` args with `--author` and repeated `--trailer "Co-authored-by: ..."` before `-m`.
  - Detect unsupported `--trailer` stderr only when co-authors are configured and expose a clear `CommitError` signal.

- [ ] **Step 3: Verify git tests pass**
  - Run: `go test ./internal/git -run 'TestCreateCommit.*(Author|CoAuthor|Trailer|Hook)' -count=1`
  - Expected: PASS.

### Task 3: Foreground Fix Prompt Metadata

**Files:**
- Modify: `cmd/roborev/fix.go`
- Modify: `cmd/roborev/analyze.go`
- Test: `cmd/roborev/fix_test.go`
- Test: `cmd/roborev/analyze_test.go`

- [ ] **Step 1: Write failing prompt tests**
  - Add tests that configured metadata appears in fix, retry commit, batch footer, analyze fix, and analyze commit prompts.
  - Add tests that empty metadata omits the section.
  - Run: `go test ./cmd/roborev -run 'Test.*CommitMetadata.*Prompt|TestBuild.*Prompt' -count=1`
  - Expected: FAIL because prompt builders do not accept metadata.

- [ ] **Step 2: Implement prompt helper and thread metadata**
  - Add a helper that renders a short "Commit metadata" instruction section.
  - Change fix/analyze prompt builders to accept `config.FixCommitMetadata`.
  - Resolve metadata before foreground agent invocation and pass it into prompt builders.
  - Keep agent subprocess environment unchanged.

- [ ] **Step 3: Verify command prompt tests pass**
  - Run: `go test ./cmd/roborev -run 'Test.*CommitMetadata.*Prompt|TestBuild.*Prompt' -count=1`
  - Expected: PASS.

### Task 4: Roborev-Owned Commit Wiring

**Files:**
- Modify: `cmd/roborev/refine.go`
- Modify: `cmd/roborev/tui/actions.go`
- Test: `cmd/roborev/main_test.go` or `cmd/roborev/refine*_test.go`
- Test: `cmd/roborev/tui/action_test.go`

- [ ] **Step 1: Write failing wiring tests**
  - Add focused tests proving refine passes commit options through `commitWithHookRetry` and TUI `commitPatch` commits with author/trailer metadata.
  - Run: `go test ./cmd/roborev -run 'Test.*FixCommitMetadata|TestCommitWithHookRetry' -count=1` and `go test ./cmd/roborev/tui -run 'Test.*CommitPatch.*Metadata' -count=1`
  - Expected: FAIL because commit functions do not accept metadata.

- [ ] **Step 2: Implement wiring**
  - Resolve metadata in refine before committing and pass converted `git.CommitOptions` into `commitWithHookRetry`.
  - Resolve metadata in TUI `applyFixPatch` before `commitPatch`.
  - Update `commitPatch` to accept commit options and include `--author` / `--trailer`.

- [ ] **Step 3: Verify wiring tests pass**
  - Run: `go test ./cmd/roborev -run 'Test.*FixCommitMetadata|TestCommitWithHookRetry' -count=1`
  - Run: `go test ./cmd/roborev/tui -run 'Test.*CommitPatch.*Metadata' -count=1`
  - Expected: PASS.

### Task 5: Documentation And Full Verification

**Files:**
- Modify: `docs/configuration.md`
- Modify: `docs/commands.md`
- Modify: `README.md` if needed for the top-level config sample

- [ ] **Step 1: Update documentation**
  - Document keys, scope, deterministic surfaces, prompt-only foreground behavior, committer behavior, and agent-added trailer caveat.

- [ ] **Step 2: Run focused test suites**
  - Run: `go test ./internal/config ./internal/git ./cmd/roborev ./cmd/roborev/tui -count=1`
  - Expected: PASS.

- [ ] **Step 3: Run full quality gates**
  - Run: `go test ./...`
  - Run: `make lint-ci`
  - Expected: PASS.

- [ ] **Step 4: Commit and push**
  - Stage only related files.
  - Commit with a rationale-focused message.
  - Run: `git pull --rebase && git push && git status --short --branch`
  - Expected: clean branch up to date with origin.
