---
title: Custom Tasks & Agentic Mode
description: Run custom AI tasks and understand review vs agentic modes
---

Use `roborev run` to execute custom tasks with AI agents. While automatic reviews focus on commits, `run` lets you target specific files, ask questions, or perform targeted analysis.

!!! tip
    For common analysis tasks like finding duplication, reducing complexity, or identifying dead code, see [Assisted Refactoring](/guides/assisted-refactoring/). The `roborev analyze` command provides built-in prompts for these use cases and can automatically apply fixes.

```bash
roborev run "Review src/auth.go for security issues"
roborev run --wait "Find simplification opportunities in this codebase"
roborev run --agentic "Add input validation to the user controller"
```

## Use Cases

### Targeted File Reviews

Review specific files for particular concerns:

```bash
# Security review
roborev run "Review src/auth/ for security vulnerabilities"
roborev run "Check database queries in src/db/ for SQL injection"

# Simplification opportunities
roborev run "Find opportunities to simplify src/handlers/user.go"
roborev run "Identify dead code or unused functions in src/utils/"

# Refactoring candidates
roborev run "Suggest refactoring opportunities in src/api/"
roborev run "Find code duplication in src/services/"
```

### Code Analysis

Ask questions about architecture and design:

```bash
roborev run "Explain the authentication flow in this codebase"
roborev run "What design patterns are used here?"
roborev run "Document the main entry points of this application"
roborev run "List all external API dependencies"
```

### Custom Review Criteria

Apply specific review criteria beyond the default review:

```bash
roborev run "Review recent changes for OWASP Top 10 vulnerabilities"
roborev run "Check for proper error handling in src/api/"
roborev run "Verify all database connections are properly closed"
roborev run "Find functions exceeding 50 lines"
```

### Making Changes (Agentic Mode)

Use `--agentic` to allow file modifications:

```bash
roborev run --agentic "Add comprehensive error handling to main.go"
roborev run --agentic "Refactor database layer to use connection pooling"
roborev run --agentic "Add input validation to all API endpoints"
roborev run --agentic "Convert callback-style code to async/await"
```

### Piped Input

Pipe complex prompts or instructions from files:

```bash
echo "Add comprehensive error handling" | roborev run --agentic --wait
cat review-checklist.txt | roborev run --wait
```

## Flags

| Flag | Description |
|------|-------------|
| `--wait` | Wait for task to complete and show result |
| `--agent` | Agent to use (default: from config) |
| `--reasoning` | Reasoning level: `fast`, `standard`, or `thorough` |
| `--no-context` | Don't include repository context in prompt |
| `--agentic` | Enable agentic mode (allow file edits and commands) |
| `--yolo` | Alias for `--agentic` |
| `--quiet` | Suppress output (just enqueue) |

## Repository Context

By default, tasks include context about the repository:
- Repository name and path
- Any project guidelines from `.roborev.toml`

Use `--no-context` for raw prompts without this context.

## Tips

- Use `--wait` to see results immediately in the terminal
- Use `--reasoning thorough` for security-sensitive analysis
- Combine with `roborev tui` to review task results later
- Tasks appear in the TUI alongside commit reviews

## Review vs Agentic Modes

Agents run in one of two modes depending on the task.

| Mode | Tools Available | Used By |
|------|-----------------|---------|
| **Review** (default) | Read, Glob, Grep | `roborev review`, `roborev run` |
| **Agentic** | Read, Glob, Grep, Edit, Write, Bash | `roborev refine`, `roborev run --agentic` |

### Review Mode

**Review mode** is read-only. Agents can inspect code but cannot make changes. This is the safe default for automatic reviews triggered by post-commit hooks.

The agent can read files, search for patterns, and analyze code structure. It cannot edit files, create files, or run commands. **No background or async operation (reviews, enqueue, or `roborev run` without `--agentic`) ever modifies your working tree.**

### Agentic Mode

**Agentic mode** allows agents to edit files and run commands. If you pass `--agentic` and the agent makes changes to your working tree, that's an explicit opt-in. roborev will never do this on its own. Enable it in three ways:

**Per-job:**

```bash
roborev run --agentic "Refactor the error handling"
roborev run --yolo "Add input validation"   # --yolo is an alias
```

**Per-command:** the `refine` command automatically enables agentic mode:

```bash
roborev refine   # Always runs in agentic mode
```

**Globally:**

```toml
# ~/.roborev/config.toml
allow_unsafe_agents = true
```

Then restart the daemon:

```bash
roborev daemon restart
```

!!! warning
    Global agentic mode means all operations can potentially write to your codebase. Use with caution.

### Agent-Specific Flags

| Agent | Review Mode | Agentic Mode |
|-------|-------------|--------------|
| Codex | `--sandbox-cmd-allowlist ""` | `--dangerously-bypass-approvals-and-sandbox` |
| Claude Code | Default behavior | `--dangerously-skip-permissions` |
| Gemini | Default behavior | `--yolo --allowed-tools` |
| Copilot | Default behavior | Manual approval required |
| Cursor | Default behavior | `--yolo` |
| OpenCode | Default behavior | Auto-approves in non-interactive mode |

### Security Considerations

When using agentic mode:

- The agent can run arbitrary commands on your machine
- This includes installing dependencies, running builds, etc.
- Safe for your own code on trusted branches
- Use isolation for untrusted code (containers, VMs)

See [Auto-Fix Agentic Loop Security](/guides/auto-fixing/#security-considerations) for detailed guidance.

## See Also

- [Assisted Refactoring](/guides/assisted-refactoring/) - Built-in analysis types with `roborev analyze` and `roborev fix`
- [Auto-Fix with Refine](/guides/auto-fixing/) - Automated issue resolution
- [Terminal UI](/integrations/tui/) - View task results
