# roborev-snooze

Temporarily silence or resume roborev Agent Hook reminders for the current
worktree and branch. Reviews continue to enqueue and run while reminders are
snoozed.

## Usage

```text
/roborev-snooze [on|off] [duration]
```

This skill is human-triggered only. Invoke it explicitly with the slash command;
the model must not select it automatically.

Run the matching command:

```bash
roborev agent hook snooze on                   # defaults to eight hours
roborev agent hook snooze on --duration 2h     # custom duration
roborev agent hook snooze off                  # resume immediately
```

Do not use `roborev daemon pause`; that pauses review processing, while snooze affects
only Agent Hook reminders.
