# Kit Agent Hook Integration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:executing-plans to implement this plan directly in the current
> agent, task-by-task. Never use subagent-driven development. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace roborev's harness-specific hook protocol code with kit
v0.14.0, expose all eight kit profiles, and preserve roborev's scoped reminder
policy.

**Approved spec/design:**
`docs/superpowers/specs/2026-07-29-kit-agent-hook-integration-design.md`

**Architecture:** Kit owns profile metadata, config mutation, command quoting,
payload normalization, typed dispatch, and response encoding. Roborev retains
installed-profile selection, stable binary resolution, configuration policy,
the local state daemon, and reminder delivery; a typed handler is the only
runtime bridge between the two packages.

**Tech Stack:** Go 1.26.3, `go.kenn.io/kit/agenthook` v0.14.0, Cobra,
`stretchr/testify`, Zensical Markdown.

## Global Constraints

- Support Claude Code, Codex, GitHub Copilot CLI, Cursor, Factory Droid,
  Gemini CLI, Hermes Agent, and Qwen Code.
- The default install selects profiles whose executable is on `PATH` or whose
  kit-resolved config directory exists; `--agent all` selects all eight.
- Every installed command is `roborev agent-hook run --agent PROFILE` and uses
  the application-namespaced ownership marker `roborev`, which also identifies
  commands created by older roborev releases without matching another
  application's `agent-hook run` command.
- Use one uniform `--config` override; remove `--codex-config`,
  `--claude-config`, and `--scope` without aliases.
- Keep `--timeout` and convert its `time.Duration` through kit's native units.
- Do not allow project-scoped Factory Droid hook installation.
- Every reminder-capable profile's default reminder must remain actionable
  without a separately installed skill; Cursor records events and emits empty
  responses because kit v0.14.0 has no Cursor control output at `PostToolUse`
  or `Stop`.
- Hermes pending reminders retain repository lineage across CWD changes and
  are delivered by priority, then creation time, one per `Stop`.
- Daemon communication remains fail-open; malformed native payloads remain
  hard CLI errors.
- Test roborev-owned behavior and the kit seam, not kit's native format matrix.
- Use `testify` assertions and the repository's isolated test environment.
- Never install a branch-built roborev binary into the user's `PATH` or run
  tests against `~/.roborev`.

---

## File Structure

- `cmd/roborev/agent_hook_handler.go`: typed kit handler and conversion to the
  existing daemon request.
- `cmd/roborev/agent_hook_cmd.go`: Cobra contract, profile parsing, and kit
  runtime dispatch.
- `internal/agenthook/install.go`: small kit-backed install/dump orchestration.
- `internal/agenthook/profiles.go`: stable profile order, executable discovery,
  and auto/explicit/all selection.
- `internal/agenthook/droid_path.go`: retained roborev policy that rejects
  project-scoped Factory Droid config paths.
- `internal/agenthook/types.go`: persisted pending-reminder shape and daemon
  request delivery capability.
- `internal/agenthook/state.go`: reserve, prioritize, and deliver pending
  reminders.
- `internal/agenthook/output.go`: roborev-owned continuation text only; no
  native response encoding.
- `cmd/roborev/quickstart.go`: kit-based installed-hook detection for existing
  quickstart checks.

### Task 1: Upgrade kit and route runtime hooks through typed dispatch

**Files:**

- Modify: `go.mod`
- Modify: `go.sum`
- Create: `cmd/roborev/agent_hook_handler.go`
- Modify: `cmd/roborev/agent_hook_cmd.go`
- Modify: `cmd/roborev/agent_hook_test.go`
- Modify: `internal/agenthook/config.go`
- Modify: `internal/agenthook/config_test.go`
- Modify: `internal/agenthook/output.go`
- Modify: `internal/agenthook/output_test.go`

**Interfaces:**

- Consumes: `kitagenthook.Handle`, `kitagenthook.NoopHandler`, typed
  `PreToolUseInput`, `PostToolUseInput`, and `StopInput`.
- Produces: `roborevAgentHookHandler`,
  `newRoborevAgentHookHandler(agent, opts, stderr)`, and
  `agenthook.PostToolUseAdditionalContext(reason string) string`.

- [ ] **Step 1: Add kit v0.14.0 without changing production behavior**

Run:

```bash
go test ./internal/agenthook ./cmd/roborev
go get go.kenn.io/kit@v0.14.0
go mod tidy
go test ./internal/agenthook ./cmd/roborev
```

Expected: `go.mod` requires `go.kenn.io/kit v0.14.0`, `go.sum` is updated, and
`go test ./internal/agenthook ./cmd/roborev` still passes.

- [ ] **Step 2: Write failing typed-runtime and profile-option tests**

Add these focused contracts to `cmd/roborev/agent_hook_test.go` and
`internal/agenthook/config_test.go`:

```go
func TestRunAgentHookEncodesKitStopResponse(t *testing.T) {
	oldPost := postAgentHook
	postAgentHook = func(context.Context, agenthook.Request) (agenthook.Response, error) {
		return agenthook.Response{Triggered: true, Reason: "resolve reviews"}, nil
	}
	t.Cleanup(func() { postAgentHook = oldPost })

	var stdout bytes.Buffer
	err := runAgentHook(
		kitagenthook.AgentQwen,
		agenthook.DefaultOptions(),
		strings.NewReader(`{"session_id":"s1","hook_event_name":"Stop"}`),
		&stdout,
		io.Discard,
	)

	require.NoError(t, err)
	assert.JSONEq(t, `{"decision":"block","reason":"resolve reviews"}`, stdout.String())
}

func TestRunAgentHookCursorSuppressesUnsupportedControlOutput(t *testing.T) {
	oldPost := postAgentHook
	var got agenthook.Request
	postAgentHook = func(_ context.Context, req agenthook.Request) (agenthook.Response, error) {
		got = req
		return agenthook.Response{Triggered: true, Reason: "must not be encoded"}, nil
	}
	t.Cleanup(func() { postAgentHook = oldPost })

	var stdout bytes.Buffer
	err := runAgentHook(
		kitagenthook.AgentCursor,
		agenthook.DefaultOptions(),
		strings.NewReader(`{"session_id":"s1","hook_event_name":"Stop"}`),
		&stdout,
		io.Discard,
	)

	require.NoError(t, err)
	assert.JSONEq(t, `{}`, stdout.String())
	assert.Equal(t, agenthook.DefaultTurnThreshold, got.Threshold)
	assert.Equal(t, agenthook.DefaultCommitThreshold, got.CommitThreshold)
	assert.Equal(t, agenthook.DefaultFailedReviewThreshold, got.FailedReviewThreshold)
}

func TestResolveOptionsForEveryKitAgent(t *testing.T) {
	clearAgentHookEnv(t)
	for _, profile := range kitagenthook.Profiles() {
		t.Run(string(profile.Agent), func(t *testing.T) {
			opts, err := ResolveOptionsForAgent(string(profile.Agent), DefaultOptions(), nil)
			require.NoError(t, err)
			assert.Equal(t, DefaultInstruction, opts.Instruction)
		})
	}
}
```

The first test must fail because `runAgentHook` does not accept a kit profile;
the second must fail because five kit agents are rejected and Droid has a
different default instruction.

- [ ] **Step 3: Run the focused tests and verify the expected failures**

Run:

```bash
go test ./cmd/roborev -run 'TestRunAgentHookEncodesKitStopResponse|TestRunAgentHookFailsOpenWhenDaemonUnavailable'
go test ./internal/agenthook -run 'TestResolveOptionsForEveryKitAgent|TestResolveOptionsForAgentDroid'
```

Expected: FAIL on the new signature/profile contracts, not on fixture setup.

- [ ] **Step 4: Implement the typed handler and generic option profile**

Create `cmd/roborev/agent_hook_handler.go` with this shape:

```go
type roborevAgentHookHandler struct {
	kitagenthook.NoopHandler
	agent  kitagenthook.Agent
	opts   agenthook.Options
	stderr io.Writer
}

func newRoborevAgentHookHandler(
	agent kitagenthook.Agent,
	opts agenthook.Options,
	stderr io.Writer,
) roborevAgentHookHandler {
	return roborevAgentHookHandler{agent: agent, opts: opts, stderr: stderr}
}

func (h roborevAgentHookHandler) request(
	common kitagenthook.CommonInput,
	toolName string,
	toolInput json.RawMessage,
) (agenthook.Request, error) {
	input := agenthook.Input{
		SessionID: common.SessionID,
		CWD: common.CWD,
		HookEventName: string(common.HookEventName),
		TurnID: common.TurnID,
		ToolName: toolName,
	}
	if len(toolInput) > 0 {
		if err := json.Unmarshal(toolInput, &input.ToolInput); err != nil {
			return agenthook.Request{}, fmt.Errorf("decode normalized tool input: %w", err)
		}
	}
	return agenthook.Request{
		Event: input,
		Threshold: h.opts.TurnThreshold,
		CommitThreshold: h.opts.CommitThreshold,
		FailedReviewThreshold: h.opts.FailedReviewThreshold,
		Instruction: h.opts.Instruction,
		RoborevServerAddr: h.opts.RoborevServerAddr,
	}, nil
}
```

Implement `PreToolUse`, `PostToolUse`, and `Stop` by calling
`postAgentHook`. On post errors, write `roborev agent-hook: ERR` to stderr and
return a zero typed output with a nil error. Return
`PostToolUseOutput{AdditionalContext: agenthook.PostToolUseAdditionalContext(resp.Reason)}`
for delivered post-tool reminders and
`StopOutput{Decision: kitagenthook.DecisionBlock, Reason: resp.Reason}` for
stop reminders. `Stop` must copy `StopHookActive` and
`LastAssistantMessage` from the typed stop input into the internal event before
posting; pre/post methods must copy `ToolUseID`, and post must also copy the
typed tool response. Add a table-driven handler test that captures each posted
request and asserts those fields survive kit normalization. Cursor posts the
same request and thresholds as every other profile, then returns zero typed
output regardless of the daemon response.

Change `runHook` to accept `kitagenthook.Agent` and delegate the finite stdin
reader and stdout writer to:

```go
return kitagenthook.Handle(
	context.Background(),
	agent,
	stdin,
	stdout,
	newRoborevAgentHookHandler(agent, opts, stderr),
)
```

Change `ResolveOptionsForAgent` so all kit profiles except Droid use
`[agent_hook]` and Droid keeps `[droid_hook]`. Replace both old defaults with
one self-contained `DefaultInstruction` containing the skill-first and CLI
fallback workflow from the approved spec. Remove `BuildOutput`; retain and
export only `PostToolUseAdditionalContext`.

- [ ] **Step 5: Run runtime and configuration tests**

Run:

```bash
go test ./cmd/roborev -run 'TestRunAgentHook|TestAgentHookDaemon'
go test ./internal/agenthook -run 'TestResolveOptions|TestPostToolUseAdditionalContext'
```

Expected: PASS, including the existing daemon-unavailable fail-open case.

- [ ] **Step 6: Keep the typed runtime seam uncommitted until profile-bearing
  commands land**

Task 1 intentionally remains in the working tree. Existing installed commands
do not all carry `--agent`, so typed dispatch, the kit-backed installer, and the
required CLI profile contract form one atomic commit at the end of Task 4.

### Task 2: Persist and deliver scoped Hermes reminders

**Files:**

- Modify: `internal/agenthook/types.go`
- Modify: `internal/agenthook/state.go`
- Modify: `internal/agenthook/state_test.go`
- Modify: `cmd/roborev/agent_hook_handler.go`
- Modify: `cmd/roborev/agent_hook_test.go`

**Interfaces:**

- Consumes: Task 1's `roborevAgentHookHandler` and existing `hookScope` lineage
  helpers.
- Produces: `Request.DeferPostToolReminder bool`,
  `SessionState.PendingReminders map[string]PendingReminder`,
  `pendingReminderKey`, `queuePendingReminder`, and `deliverPendingReminder`.

- [ ] **Step 1: Write failing cross-repository delivery tests**

Add an integration test to `internal/agenthook/state_test.go` that uses two real
temporary git repos and the existing jobs-response fixture pattern:

```go
func TestDeferredPostToolReminderKeepsDashCRepositoryAtStop(t *testing.T) {
	outer := testutil.NewGitRepo(t)
	outer.CommitFile("outer.go", "package outer\n", "outer")
	inner := testutil.NewGitRepo(t)
	inner.CommitFile("inner.go", "package inner\n", "inner")

	closed := false
	verdict := "F"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		jobs := []storage.ReviewJob{}
		if r.URL.Query().Get("repo") == inner.Path() {
			jobs = append(jobs, storage.ReviewJob{
				Status: storage.JobStatusDone, Closed: &closed, Verdict: &verdict,
			})
		}
		require.NoError(t, json.NewEncoder(w).Encode(jobsResponse{Jobs: jobs}))
	}))
	t.Cleanup(server.Close)

	store := &StateStore{path: filepath.Join(t.TempDir(), "state.json"), sessions: map[string]SessionState{}}
	command := `git -C "` + inner.Path() + `" commit -m feature`
	raw, err := json.Marshal(command)
	require.NoError(t, err)
	base := Request{
		Event: Input{SessionID: "s1", CWD: outer.Path(), ToolName: "Bash", ToolInput: map[string]json.RawMessage{"command": raw}},
		CommitThreshold: 1, Instruction: "resolve reviews", RoborevServerAddr: server.URL,
		DeferPostToolReminder: true,
	}
	pre := base
	pre.Event.HookEventName = "PreToolUse"
	_, err = store.Record(pre)
	require.NoError(t, err)
	inner.CommitFile("feature.go", "package inner\n", "feature")
	post := base
	post.Event.HookEventName = "PostToolUse"
	postResp, err := store.Record(post)
	require.NoError(t, err)
	assert.False(t, postResp.Triggered)

	stopResp, err := store.Record(Request{Event: Input{SessionID: "s1", CWD: outer.Path(), HookEventName: "Stop"}})
	require.NoError(t, err)
	assert.True(t, stopResp.Triggered)
	assert.Equal(t, "commit", stopResp.TriggeredBy)
	assert.Contains(t, stopResp.Reason, repoDisplayName(inner.Path()))
	assert.Contains(t, stopResp.Reason, inner.Path())
	assert.Contains(t, stopResp.Reason, "change to")
}
```

Add a second test that queues a commit reminder for repo A, then a failed-review
reminder for repo B. Assert the first `Stop` delivers repo B's failed-review
entry, retains repo A's entry, and the second `Stop` delivers repo A. Add a
third test that queues a failed-review reminder while the event CWD is repo A,
uses repo B as the subsequent `Stop` CWD, and asserts repo A's stored reason is
delivered independently of repo B. Add a
state-load test that writes a session without `pending_reminders`, calls
`LoadState`, and asserts the new map is empty.

Add tests that cross the same lineage-and-trigger threshold twice before a
`Stop` and assert there is one coalesced entry whose `CreatedAt` is unchanged
and whose relevant count is accumulated. Add a failed-review test that queues
an entry, resolves the review before `Stop`, and asserts delivery discards the
stale entry without incrementing `ReminderPromptCount`. Assert pending entries
appear through the existing status payload and disappear through the existing
session reset path rather than adding a separate cleanup API.

Add a handler integration test in `cmd/roborev/agent_hook_test.go` that runs a
native Hermes `PostToolUse` payload, captures the daemon request, and asserts
`DeferPostToolReminder` is true. The same test must assert the handler emits an
empty Hermes response when the daemon reports a trigger.

- [ ] **Step 2: Run the new state tests and verify RED**

Run:

```bash
go test ./internal/agenthook -run 'TestDeferredPostToolReminder|TestPendingReminderPriority|TestPendingReminderSurvivesCWDChange|TestLoadStateWithoutPendingReminders'
go test ./cmd/roborev -run 'TestRunAgentHookHermesDefersPostToolReminder'
```

Expected: FAIL because the request field, persisted type, and delivery queue do
not exist.

- [ ] **Step 3: Add the persisted pending-reminder model**

Add these fields in `internal/agenthook/types.go`:

```go
type PendingReminder struct {
	TriggeredBy       string    `json:"triggered_by"`
	Reason            string    `json:"reason"`
	TrackedRepoRoot   string    `json:"tracked_repo_root"`
	WorktreeRoot      string    `json:"worktree_root"`
	Branch            string    `json:"branch,omitempty"`
	Head              string    `json:"head,omitempty"`
	LineageKey        string    `json:"lineage_key"`
	CommitCount       int       `json:"commit_count,omitempty"`
	FailedReviewCount int       `json:"failed_review_count,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
}

// Request field; zero preserves immediate delivery.
DeferPostToolReminder bool `json:"defer_post_tool_reminder,omitempty"`

// SessionState field; absent data from older state files decodes as nil.
PendingReminders map[string]PendingReminder `json:"pending_reminders,omitempty"`
```

Use `lineageKey + "\x00" + triggeredBy` as the pending map key so a failed
review and a commit for one lineage can remain independently queued. When that
key already exists, update its reason and repository metadata, accumulate the
relevant count, and preserve its original `CreatedAt`.

- [ ] **Step 4: Reserve triggers and deliver them before resolving Stop CWD**

Refactor `recordPostToolUse` so immediate profiles keep their current path. For
deferred requests, queue every ready failed-review and commit reminder with its
rendered reason, advance failed-review dedupe bookkeeping, and reset only that
scope's commit counters. Do not increment `ReminderPromptCount` or set delivery
timestamps when queueing.

At the start of `recordStop`, after the `StopHookActive` recursion guard and
before `resolveHookScope`, call:

```go
func (s *StateStore) deliverPendingReminder(sessionID string) (Response, bool, error)
```

Sort candidates by `failed_reviews` before `commit`, then by `CreatedAt`, then
by map key for deterministic ties. Re-query failed reviews using each pending
entry's stored repository, branch, and HEAD before selecting it; discard a
resolved entry without updating delivery counters and continue to the next
candidate. Remove only the selected actionable entry, increment
`ReminderPromptCount`, set the matching delivery timestamp, save state, and
return a triggered response carrying the stored reason and counts. A pending
delivery must work even when the stop CWD is outside any repository. The stored
reason must identify the absolute `WorktreeRoot` and explicitly tell the agent
to change to that worktree before running fallback commands.

Set `DeferPostToolReminder: h.agent == kitagenthook.AgentHermes` in Task 1's
handler request construction.

- [ ] **Step 5: Run state and handler tests**

Run:

```bash
go test ./internal/agenthook -run 'TestRecord|TestDeferred|TestPending|TestLoadState'
go test ./cmd/roborev -run 'TestRunAgentHook'
```

Expected: PASS; existing immediate Codex/Claude/Droid behavior remains green.

- [ ] **Step 6: Keep scoped delivery with the atomic profile migration**

Do not commit yet. Task 2 depends on Task 1's typed handler, which becomes
user-reachable only with Task 4's required profile-bearing commands. Commit the
complete runtime and installation migration after Task 4 passes.

### Task 3: Replace config mutation with kit and add profile discovery

**Files:**

- Create: `internal/agenthook/profiles.go`
- Create: `internal/agenthook/profiles_test.go`
- Create: `internal/agenthook/droid_path.go`
- Create: `internal/agenthook/droid_path_test.go`
- Rewrite: `internal/agenthook/install.go`
- Rewrite: `internal/agenthook/install_test.go`
- Modify: `internal/agenthook/detect.go`
- Modify: `internal/agenthook/detect_test.go`

**Interfaces:**

- Consumes: `kitagenthook.Profiles`, `ParseAgent`, `ConfigPath`, `Install`,
  `PlanInstall`, and `PlanUninstall`.
- Produces: `SelectProfiles(raw string) ([]kitagenthook.Agent, error)`,
  kit-backed `RunInstall`, `RunDump`, and
  `Installed(agent kitagenthook.Agent, path string) (bool, error)`.

- [ ] **Step 1: Write failing selection and kit-seam tests**

Create `internal/agenthook/profiles_test.go` with real PATH and config-directory
fixtures:

```go
func TestSelectProfilesAutoDetectsExecutableOrConfigDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	bin := t.TempDir()
	gemini := filepath.Join(bin, "gemini")
	if runtime.GOOS == "windows" {
		gemini += ".bat"
	}
	require.NoError(t, os.WriteFile(gemini, []byte("exit 0\n"), 0o755))
	t.Setenv("PATH", bin)
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".qwen"), 0o755))

	agents, err := SelectProfiles("")

	require.NoError(t, err)
	assert.Equal(t, []kitagenthook.Agent{
		kitagenthook.AgentGemini,
		kitagenthook.AgentQwen,
	}, agents)
}

func TestSelectProfilesAllUsesKitOrder(t *testing.T) {
	agents, err := SelectProfiles("all")
	require.NoError(t, err)
	assert.Equal(t, []kitagenthook.Agent{
		kitagenthook.AgentClaude, kitagenthook.AgentCodex,
		kitagenthook.AgentCopilot, kitagenthook.AgentCursor,
		kitagenthook.AgentDroid, kitagenthook.AgentGemini,
		kitagenthook.AgentHermes, kitagenthook.AgentQwen,
	}, agents)
}
```

The auto-detection fixture must redirect `HOME`, `USERPROFILE`,
`LOCALAPPDATA`, `CODEX_HOME`, `CLAUDE_CONFIG_DIR`, `COPILOT_HOME`,
`GEMINI_CLI_HOME`, `HERMES_HOME`, and `QWEN_HOME` into the test's private
scratch root so host agent installations cannot affect the result.

In `install_test.go`, add one integration seam test that installs Qwen to a
temporary settings file and asserts the resulting command contains
`agent-hook run --agent qwen`, the ownership marker replaces an older
`agent-hook run` entry, and a second install reports unchanged. Do not recreate
kit's matcher, timeout-unit, JSON, or YAML matrix tests.

Add these roborev-owned RED contracts:

- `TestSelectProfilesAutoReturnsActionableErrorWhenNothingDetected` uses an
  empty PATH and private HOME, then asserts the error names `--agent` and
  `--agent all`.
- `TestInstallOptionsUseProfileSpecificRunArguments` iterates all eight kit
  profiles and asserts the pure kit-option builder returns
  `[]string{"agent-hook", "run", "--agent", string(agent)}`.
- `TestRunInstallRejectsCommandForDifferentProfile` passes a Qwen command to a
  Gemini install and asserts validation fails before the config file exists.
- `TestRunInstallContinuesAfterProfileError` puts invalid Claude JSON at its
  default scratch config, runs `--agent all`, asserts the joined error names
  Claude, and asserts a later Qwen config contains the ownership marker.

- [ ] **Step 2: Run selection and install tests and verify RED**

Run:

```bash
go test ./internal/agenthook -run 'TestSelectProfiles|TestInstallOptions|TestRunInstall|TestInstalled'
```

Expected: FAIL because selection and kit-backed options do not exist.

- [ ] **Step 3: Implement stable profile discovery**

Create `profiles.go` with this mapping and kit profile order:

```go
var profileExecutables = map[kitagenthook.Agent][]string{
	kitagenthook.AgentClaude:  {"claude"},
	kitagenthook.AgentCodex:   {"codex"},
	kitagenthook.AgentCopilot: {"copilot"},
	kitagenthook.AgentCursor:  {"agent"},
	kitagenthook.AgentDroid:   {"droid"},
	kitagenthook.AgentGemini:  {"gemini"},
	kitagenthook.AgentHermes:  {"hermes"},
	kitagenthook.AgentQwen:    {"qwen"},
}
```

`SelectProfiles("")` walks `kitagenthook.Profiles()` and selects a profile
when any candidate resolves through `exec.LookPath` or `os.Stat` reports the
parent of `kitagenthook.ConfigPath` exists. Ignore config-path errors only for
auto detection; explicit selection uses `ParseAgent` and later surfaces config
errors from kit. Return an actionable error when auto detection selects none.

- [ ] **Step 4: Replace installer and detector internals with kit calls**

Rewrite public options as:

```go
type InstallOptions struct {
	Agent      string
	Executable string
	Command    string
	ConfigPath string
	Timeout    time.Duration
	DryRun     bool
}

type DumpOptions struct {
	Agent      string
	Executable string
	Command    string
	ConfigPath string
	Timeout    time.Duration
}
```

Build the common kit options per selected profile:

```go
kitagenthook.InstallOptions{
	ConfigPath: opts.ConfigPath,
	Executable: opts.Executable,
	Arguments: []string{"agent-hook", "run", "--agent", string(agent)},
	Command: opts.Command,
	Marker: "roborev",
	Hooks: []kitagenthook.Hook{
		{Event: kitagenthook.EventPreToolUse, Matcher: kitagenthook.ToolBash, Timeout: opts.Timeout},
		{Event: kitagenthook.EventPostToolUse, Matcher: kitagenthook.ToolBash, Timeout: opts.Timeout},
		{Event: kitagenthook.EventStop, Timeout: opts.Timeout},
	},
}
```

Use `PlanInstall` for dry-run and dump, `Install` otherwise, and `errors.Join`
after attempting every selected profile. Validate `--command` contains the
application marker and exactly one effective `--agent PROFILE` or
`--agent=PROFILE` selection. Reject config and raw command overrides unless
`Agent` names one explicit profile.

Implement raw-command profile validation from only the suffix after
`agent-hook run`, so a quoted executable path cannot affect selection. Scan all
arguments until an argument terminator, reject missing values and any repeated
`--agent` flag (including a duplicate with the same value), and require the one
effective value to equal the requested profile:

```go
func commandAgent(command string) (kitagenthook.Agent, error) {
	index := strings.Index(command, agentHookRunner)
	if index < 0 {
		return "", errors.New("command must invoke agent-hook run")
	}
	fields := strings.Fields(command[index+len(agentHookRunner):])
	selected := ""
	for i := 0; i < len(fields); i++ {
		field := fields[i]
		if field == "--" {
			break
		}
		value := ""
		switch {
		case field == "--agent":
			if i+1 >= len(fields) || fields[i+1] == "--" {
				return "", errors.New("--agent requires a value")
			}
			i++
			value = fields[i]
		case strings.HasPrefix(field, "--agent="):
			value = strings.TrimPrefix(field, "--agent=")
		default:
			continue
		}
		if value == "" {
			return "", errors.New("--agent requires a value")
		}
		if selected != "" {
			return "", errors.New("command must select exactly one agent")
		}
		selected = value
	}
	if selected == "" {
		return "", errors.New("command must select an agent")
	}
	return kitagenthook.ParseAgent(selected)
}
```

Cover repeated identical flags, conflicting flags, both assignment forms,
missing values, and `--agent` after `--`.

Wrap per-profile failures with the kit display name and resolved config path
before joining them; successful earlier profiles remain installed.

Move only the existing Factory Droid project-path validation and its focused
tests into `droid_path.go` and `droid_path_test.go`; delete JSON mutation,
matcher, quoting, and native config helpers.

Replace the schema-agnostic JSON walker in `detect.go` with:

```go
func Installed(agent kitagenthook.Agent, path string) (bool, error) {
	result, err := kitagenthook.PlanUninstall(agent, path, "roborev")
	if err != nil {
		return false, err
	}
	return result.Changed, nil
}
```

- [ ] **Step 5: Run internal installer tests**

Run:

```bash
go test ./internal/agenthook -run 'TestSelectProfiles|TestRunInstall|TestRunDump|TestInstalled|TestDroid'
```

Expected: PASS, with no tests asserting kit's own native formatting details.

- [ ] **Step 6: Continue directly to the CLI callers**

Do not commit the rewritten option and detector APIs before their CLI and
quickstart callers are updated. Task 3 and Task 4 are one atomic implementation
unit.

### Task 4: Atomically enforce the uniform CLI and finish the kit migration

**Files:**

- Modify: `cmd/roborev/agent_hook_cmd.go`
- Modify: `cmd/roborev/agent_hook_test.go`
- Modify: `cmd/roborev/quickstart.go`
- Modify: `cmd/roborev/quickstart_cmd_test.go`
- Modify: `internal/agenthook/state.go`

**Interfaces:**

- Consumes: Task 3's `InstallOptions`, `DumpOptions`, `SelectProfiles`, and
  `Installed`.
- Produces: the approved Cobra flag contract and kit-profile-aware quickstart
  checks.

- [ ] **Step 1: Write failing CLI behavior tests**

Replace native-shape assertions in `cmd/roborev/agent_hook_test.go` with
user-visible contracts:

```go
func TestAgentHookInstallSupportsExplicitQwenProfile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	cmd := agentHookCmd()
	cmd.SetOut(io.Discard)
	cmd.SetArgs([]string{
		"install", "--agent", "qwen", "--config", path,
		"--command", "roborev agent-hook run --agent qwen",
	})

	require.NoError(t, cmd.Execute())
	body, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(body), "agent-hook run --agent qwen")
}

func TestAgentHookInstallRejectsMultiProfileConfigOverride(t *testing.T) {
	cmd := agentHookCmd()
	cmd.SetArgs([]string{"install", "--agent", "all", "--config", "hooks.json"})
	err := cmd.Execute()
	require.Error(t, err)
	assert.ErrorContains(t, err, "--config requires one explicit agent")
}

func TestAgentHookRunRequiresProfile(t *testing.T) {
	cmd := agentHookCmd()
	cmd.SetArgs([]string{"run"})
	err := cmd.Execute()
	require.Error(t, err)
	assert.ErrorContains(t, err, "--agent is required")
}
```

Add a quickstart test that writes a kit-owned Codex hook config, calls
`checkAgentHook(kitagenthook.AgentCodex, path, fix)`, and observes `statusOK`.

- [ ] **Step 2: Run CLI and quickstart tests and verify RED**

Run:

```bash
go test ./cmd/roborev -run 'TestAgentHook|TestCheckAgentHook'
```

Expected: FAIL on unsupported Qwen, invalid old flag semantics, and the old
detector signature.

- [ ] **Step 3: Implement the Cobra contract**

For install, default `Agent` to empty auto selection; retain `--agent`,
`--command`, `--binary`, `--config`, `--timeout`, and `--dry-run`. Remove
`--codex-config`, `--claude-config`, and `--scope`. Resolve `--binary` once with
`githook.ResolveRoborevPath`; pass the resolved executable to Task 3 and print
its notice. Reject `--binary` with `--command` before any config write.

For dump, require one `--agent`; retain `--command`, `--config`, and
`--timeout`. Resolve the current stable executable when `--command` is absent
and keep notices on stderr. For run, require `--agent`, parse it with
`kitagenthook.ParseAgent`, resolve profile-specific roborev options, and call
Task 1's kit-backed runtime.

Update `checkAgentHook` to accept a kit agent and call
`agenthook.Installed(agent, path)`. Resolve Codex and Claude config paths with
`kitagenthook.ConfigPath`; quickstart's stable check IDs remain unchanged.

Remove the `Execute` alias from `isShellCommandTool`: kit normalizes all shell
tools to `Bash` before requests enter the daemon.

- [ ] **Step 4: Run command, quickstart, and agent-hook package tests**

Run:

```bash
go test ./cmd/roborev -run 'TestAgentHook|TestCheckAgentHook|TestDetectState'
go test ./internal/agenthook
```

Expected: PASS; all eight profile names are accepted and the two existing
quickstart check IDs still report installed state.

- [ ] **Step 5: Search for superseded native protocol code**

Run:

```bash
rg -n 'InstallSpec|ExecuteMatcher|BuildOutput|jsonContainsRoborevHook|codexSpecs|claudeSpecs|droidSpecs|DefaultCodexHooksPath|DefaultClaudeSettingsPath|DefaultDroidHooksPath' internal/agenthook cmd/roborev
```

Expected: no production references. Do not add a test asserting these symbols
stay absent.

- [ ] **Step 6: Commit the atomic runtime, installer, and CLI migration**

```bash
git add go.mod go.sum cmd/roborev/agent_hook_cmd.go cmd/roborev/agent_hook_handler.go cmd/roborev/agent_hook_test.go cmd/roborev/quickstart.go cmd/roborev/quickstart_cmd_test.go internal/agenthook
git commit -m "feat: centralize agent hooks through kit"
```

This is deliberately the first implementation commit after the reviewed design
and plan: every installed command and every runtime invocation now supplies the
same required profile, so no intermediate commit ships an ambiguous dispatcher
or uncompilable caller/API split.

### Task 5: Document migration and run repository quality gates

**Files:**

- Modify: `README.md`
- Modify: `docs/agent-hook.md`
- Modify: `docs/commands.md`
- Modify: `docs/automation/post-commit-reviews.md`
- Modify: `docs/guides/agent-skills.md`
- Modify: `cmd/roborev/quickstart_guide.md`

**Interfaces:**

- Consumes: Tasks 1-4's final CLI, profile list, reminder behavior, and Hermes
  delivery semantics.
- Produces: complete user-facing setup, migration, fallback, and declarative
  config documentation.

- [ ] **Step 1: Update user-facing documentation**

Document all of these concrete behaviors:

```text
roborev agent-hook install
roborev agent-hook install --agent all
roborev agent-hook install --agent hermes --config ~/.hermes/config.yaml
roborev agent-hook dump --agent qwen
roborev agent-hook run --agent cursor
```

Name all eight profiles. Explain executable-or-config-directory auto detection,
JSON versus Hermes YAML dumps, the self-contained CLI fallback instruction,
Hermes's lineage-scoped reminder queue, and Cursor's tracking-only empty
responses. Include this migration table:

```text
--codex-config PATH  -> --agent codex --config PATH
--claude-config PATH -> --agent claude --config PATH
--scope user         -> omit the flag
old installed run commands -> run roborev agent-hook install once after upgrade
```

Keep the existing warning that roborev will not install project-scoped Factory
Droid hooks. Clarify that bundled skills remain available for Claude, Codex,
and Droid but are optional for the other hook integrations.

- [ ] **Step 2: Format and validate Markdown**

Run:

```bash
make markdown
make markdown-ci
```

Expected: both commands pass and prose is wrapped to repository conventions.

- [ ] **Step 3: Run focused build and tests in scratch state**

Resolve the concrete Go binary before overriding `HOME`, create a private
`mktemp -d` root, and run:

```bash
task_go_bin=$(mise which go)
task_gomodcache=$($task_go_bin env GOMODCACHE)
task_gocache=$($task_go_bin env GOCACHE)
isolation_root=$(mktemp -d)
mkdir -p "$isolation_root/home" "$isolation_root/data" "$isolation_root/config"
chmod 700 "$isolation_root" "$isolation_root/home" "$isolation_root/data" "$isolation_root/config"
echo "Isolation root: $isolation_root"
env HOME="$isolation_root/home" \
  ROBOREV_DATA_DIR="$isolation_root/data" \
  XDG_CONFIG_HOME="$isolation_root/config" \
  GIT_CONFIG_GLOBAL=/dev/null \
  GIT_CONFIG_NOSYSTEM=1 \
  GOMODCACHE="$task_gomodcache" \
  GOCACHE="$task_gocache" \
  "$task_go_bin" build ./...
env HOME="$isolation_root/home" \
  ROBOREV_DATA_DIR="$isolation_root/data" \
  XDG_CONFIG_HOME="$isolation_root/config" \
  GIT_CONFIG_GLOBAL=/dev/null \
  GIT_CONFIG_NOSYSTEM=1 \
  GOMODCACHE="$task_gomodcache" \
  GOCACHE="$task_gocache" \
  "$task_go_bin" test ./...
```

with scratch `HOME`, `ROBOREV_DATA_DIR`, `XDG_CONFIG_HOME`,
`GIT_CONFIG_GLOBAL=/dev/null`, and `GIT_CONFIG_NOSYSTEM=1`. Reuse only the host
Go module and build caches. Do not start or stop the live roborev daemons.

Expected: build and all tests pass without opening `~/.roborev/reviews.db`.

- [ ] **Step 4: Run repository hooks**

Run:

```bash
prek run --all-files
```

Expected: git isolation, lint, Markdown, Renovate, and Actions checks pass.

- [ ] **Step 5: Commit documentation**

```bash
git add README.md docs/agent-hook.md docs/commands.md docs/automation/post-commit-reviews.md docs/guides/agent-skills.md cmd/roborev/quickstart_guide.md
git commit -m "docs: explain automatic agent-hook integration"
```

- [ ] **Step 6: Verify and push the completed branch**

Run:

```bash
git status --short --branch
git log --oneline origin/main..HEAD
git pull --rebase origin main
# If the rebase changed the tested tree, repeat Step 3 and Step 4 before push.
git push --set-upstream origin HEAD
git status --short --branch
```

Expected: the worktree is clean and the branch reports up to date with its
upstream. Any tree changed by the rebase has passed the scratch build/tests and
repository hooks again. Do not open or merge a pull request unless the user
separately asks.
