# Stream output captures

`gemini-stream.jsonl` is a Gemini CLI stdout excerpt captured on 2026-11-14.
Its timestamp and model are from that run. The session ID is redacted, and
the CLI version wasn't recorded.

The Antigravity fixtures come from CLI stdout excerpts in public issue reports.
Their source reports redact conversation IDs and local details. The init
report also abbreviates its tool list as `[...]`; the fixture records that
placeholder as a string so the excerpt remains valid JSON.

- `antigravity-init-capture.jsonl` is the redacted `init` line from the
  Antigravity 1.1.17 report in [upstream issue
  #841](https://github.com/google-antigravity/antigravity-cli/issues/841).
- `antigravity-resume-error-capture.jsonl` contains the captured error
  `result` line from [upstream issue
  #1048](https://github.com/google-antigravity/antigravity-cli/issues/1048).
- `antigravity-success-result-capture.jsonl` is the fresh-run `result` from
  that same report. It records Antigravity 1.2.6.

The reports don't include full stdout transcripts. Tests use only the captured
`init` and `result` lines needed for those behaviors.
