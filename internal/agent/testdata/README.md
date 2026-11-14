# Antigravity stream excerpts

`gemini-stream.jsonl` is a Gemini CLI stdout excerpt captured on 2026-11-14.
Its timestamp and model are from that run. The session ID is redacted, and
the CLI version wasn't recorded.

These files preserve public stdout excerpts from Antigravity CLI reports. The
source reports redact conversation IDs and other local details; those
redactions remain here. The init report elides the tool list, represented as a
single string so the line remains valid JSON.

- `antigravity-init-capture.jsonl` is the init event shown in [upstream issue
  #841](https://github.com/google-antigravity/antigravity-cli/issues/841).
- `antigravity-resume-error-capture.jsonl` contains the resume `step_update`
  and `result` lines copied from [upstream issue
  #1048](https://github.com/google-antigravity/antigravity-cli/issues/1048).
- `antigravity-success-result-capture.jsonl` is the fresh-run result from the
  same issue.

The source reports omit complete streams. Tests use the init and result
excerpts separately because each behavior needs only one event type. These
files aren't full stdout transcripts.
