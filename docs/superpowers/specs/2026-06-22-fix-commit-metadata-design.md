# Fix Commit Metadata Design

## Summary

Add a small configuration surface for metadata on commits produced by roborev fix-like workflows. The feature should let users override the commit author and add `Co-authored-by` trailers without introducing a general commit-template system.

This applies deterministically only when roborev itself creates the commit. When an external agent creates the commit, roborev will include the same metadata request in the prompt, but will not amend the resulting commit or force metadata through hooks.

## Goals

- Allow users to configure the author for fix commits.
- Allow users to configure one or more `Co-authored-by` trailers for fix commits.
- Cover all fix-like surfaces users would reasonably consider roborev fix output.
- Keep foreground agent-authored commits best-effort and honest in documentation.
- Avoid post-hoc amend behavior that changes SHAs after hooks or review enqueue logic may have observed a commit.

## Non-Goals

- No general commit message templating.
- No deterministic rewrite of agent-authored foreground commits.
- No repository hook installation to force trailers on agent commits.
- No override of the committer identity. Git should continue to use the identity of the user or process running roborev as committer.
- No environment plumbing through every agent adapter in the first implementation.

## Configuration

Add two config keys to both global config and repo config:

```toml
fix_commit_author = "Name <email@example.com>"
fix_commit_co_authored_by = [
  "Reviewer One <reviewer@example.com>",
  "Reviewer Two <reviewer2@example.com>",
]
```

The `fix_` prefix keeps the setting scoped to fix-like workflows and avoids implying that roborev manages general Git identity.

Validation should reject malformed identity strings. A valid value must be parseable as a display name plus email in the conventional `Name <email>` format.

Config precedence should follow the existing pattern:

1. Repo config
2. Global config
3. Empty/default

No CLI flag is needed in the first pass.

## Behavior

Roborev-owned commits must apply the metadata deterministically:

- `roborev refine`, where roborev applies the captured worktree patch and calls `commitWithHookRetry`.
- TUI background fix patch application, where `commitPatch` stages and commits the applied patch.

The TUI case covers daemon-created background fix jobs after the user chooses to apply the generated patch. It is intentionally separate from foreground `roborev fix`, where the configured agent usually runs `git commit` itself.

For these paths, use native Git commit support:

- `git commit --author "Name <email>"`
- `git commit --trailer "Co-authored-by: Name <email>"`

Using `--trailer` lets Git place trailers correctly and cooperate with existing trailer handling. If the installed Git version does not support `--trailer`, return a clear error that names the unsupported setting and explains that Git 2.32 or newer is required.

Agent-owned commits must receive prompt instructions only:

- Foreground `roborev fix`
- Batch `roborev fix`
- `roborev analyze --fix`

The prompt should tell the agent to use the configured author and co-author trailers when creating the commit. Documentation must describe this as best-effort because the external agent owns the actual `git commit` invocation.

## Code Shape

Add a small commit metadata value type in config or a nearby shared package:

```go
type FixCommitMetadata struct {
    Author       string
    CoAuthors    []string
}
```

Expose a resolver that loads repo/global config and returns trimmed, validated metadata.

Extend `internal/git.CreateCommit` through an options-bearing sibling or options parameter rather than duplicating ad hoc `git commit` command assembly in command code. The existing no-options behavior should remain easy for other callers.

Update TUI `commitPatch` separately because it builds its own `git commit --only ... -- <files>` command and does not call `CreateCommit`.

Add a shared prompt helper that renders the configured metadata instructions for agent-authored fix prompts, then include it in:

- `buildGenericFixPrompt`
- `buildGenericCommitPrompt`
- batch fix footer generation
- analyze fix prompt and commit prompt

## Error Handling

- Malformed identity values should fail before invoking an agent or applying a patch.
- Unsupported `git commit --trailer` should fail only when co-author trailers are configured and roborev owns the commit.
- Existing hook retry behavior in refine should keep working. Commit options must be preserved across every retry.
- TUI apply should still report the current "patch applied but commit failed" error shape if metadata causes commit failure.

## Testing

Add config tests for:

- Global and repo config parsing.
- Repo config overriding global config.
- Malformed author and co-author values.
- Empty config resolving to empty metadata.

Add git tests for:

- `CreateCommit` with author override.
- `CreateCommit` with one and multiple co-author trailers.
- Existing hook-failure classification still working with commit options.

Add command/TUI tests for:

- Refine passes metadata through `commitWithHookRetry`.
- TUI `commitPatch` includes author and trailers.
- Foreground fix and analyze prompts include the configured metadata instruction.
- Prompts omit the metadata section when config is empty.

## Documentation

Update command/config documentation to say:

- `fix_commit_author` changes the author for roborev-owned fix commits and asks foreground agents to use the same author.
- `fix_commit_co_authored_by` adds co-author trailers to roborev-owned fix commits and asks foreground agents to include them.
- The committer remains the current Git committer.
- Agent-authored foreground commits are best-effort because roborev does not amend commits after agents create them.
