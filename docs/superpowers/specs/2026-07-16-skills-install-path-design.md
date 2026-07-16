# Custom Path for Skill Installation

## Summary

Add `--path` support to `roborev skills install` so bundled skills can be installed directly into an arbitrary skills directory, including Pi's personal skill directory:

```bash
roborev skills install --path ~/.pi/agent/skills/
```

The supplied path is the final skills directory. Each skill is written beneath it as `<path>/<skill-name>/SKILL.md`.

## Command Interface

`roborev skills install` gains two flags:

```text
--path <directory>
--agent <claude|codex|droid>
```

- Without `--path`, the command retains its current behavior and installs skills for every detected supported agent.
- With `--path`, the command installs one agent variant directly into the supplied directory.
- With `--path` and no `--agent`, the agent variant defaults to `claude`.
- With both flags, the selected variant is installed.
- `--agent` without `--path` is rejected because automatic installation already targets all detected agents and an agent-specific automatic mode is outside this feature's scope.
- An unsupported `--agent` value is rejected with an error listing the accepted values.

Examples:

```bash
# Install the Claude-compatible variant for Pi.
roborev skills install --path ~/.pi/agent/skills/

# Install the Codex variant, including agents/openai.yaml files.
roborev skills install --path /tmp/custom-skills --agent codex

# Install the Factory Droid variant.
roborev skills install --path /tmp/custom-skills --agent droid
```

## Installation Behavior

The custom-path installer accepts a final skills directory rather than an agent configuration directory. It creates the directory if it does not exist, then installs every embedded skill supported by the selected agent.

For the default Claude variant, the resulting layout is:

```text
~/.pi/agent/skills/
├── roborev-fix/
│   └── SKILL.md
├── roborev-review/
│   └── SKILL.md
└── ...
```

For the Codex variant, any embedded invocation policy is installed alongside the skill:

```text
<path>/roborev-review/
├── SKILL.md
└── agents/
    └── openai.yaml
```

Installation remains idempotent. A missing destination file is reported as installed; an existing destination file is overwritten and reported as updated. Legacy skill cleanup is applied within the custom skills directory so rerunning the installer removes skill directories that roborev no longer ships.

The command's output identifies the selected agent variant and reports installed or updated skills. It does not print the existing "No agents found" guidance because custom installation does not depend on detected agent configuration directories.

## Internal Design

Keep automatic installation and destination resolution unchanged. Add a focused internal entry point that installs a selected agent's embedded skills into an explicit final skills directory.

The implementation should reuse the existing embedded skill catalog and file-writing logic rather than duplicating agent-specific behavior. The shared low-level installer receives an `agentSpec` and a final skills directory. The existing automatic installer resolves its configured skills directory before calling it; the custom-path entry point validates the public agent value and passes the caller's path directly.

Custom-path installation does not alter `Status`, `IsInstalled`, or `Update`. Those operations continue to describe and maintain the standard agent configuration locations. Users manage a custom destination by rerunning `roborev skills install --path ...`.

## Error Handling

- Reject an empty `--path` value through normal Cobra flag parsing or explicit validation.
- Reject `--agent` unless `--path` is present.
- Reject unknown agents before creating or modifying the destination.
- Return filesystem errors with context identifying the directory or skill file that could not be created or written.
- Expand `~` only through the invoking shell. Roborev receives and uses the resulting path; it does not implement independent tilde expansion.

## Testing

Add behavior-focused tests covering:

1. Installing to a nonexistent custom path creates it and writes the Claude-compatible skills by default.
2. Selecting `codex` writes Codex skill content and its `agents/openai.yaml` policy files.
3. Selecting `droid` writes the Droid variant.
4. Reinstalling into the same custom path reports updates and remains successful.
5. Legacy skill cleanup is scoped to the supplied custom skills directory.
6. The CLI rejects `--agent` without `--path` and rejects unsupported agent names without writing files.

Tests should assert installed files and returned behavior, not Cobra or standard-library flag mechanics.

## Documentation

Update the Agent Skills guide with the custom-path syntax, Pi example, agent selection behavior, and the fact that custom destinations are updated by rerunning the same install command. Add a changelog entry describing the new option.

## Out of Scope

- Discovering Pi or adding Pi as a first-class supported agent.
- Including custom paths in `roborev skills` status output.
- Updating custom paths automatically from `roborev update`.
- Supporting multiple custom destinations in one invocation.
- Changing default automatic installation behavior.
