# Stream output captures

These files contain stdout lines copied from CLI captures or public issue reports. The source records below preserve each line's origin. Redacted IDs are replaced with stable UUIDs, and local path or tool details are omitted where the report hid or abbreviated them.

- `gemini-stream.jsonl` is a four-line Gemini CLI stdout excerpt captured on 2026-11-14. Its timestamp and `gemini-2.5-flash` model are unchanged. The capture did not record the CLI version. Its redacted session ID is replaced with `d91bc4f1-01d9-4c9f-9b07-23ae0ecb39f2`.
- `antigravity-init-capture.jsonl` reproduces the captured `init` event in the 1.1.17 report at [upstream issue #841](https://github.com/google-antigravity/antigravity-cli/issues/841). The report redacts the conversation ID and cwd and abbreviates the tool list. The fixture substitutes a stable UUID and omits those local fields.
- `antigravity-quota-result-capture.jsonl` is the captured error `result` event from the 1.2.6 report at [upstream issue #1048](https://github.com/google-antigravity/antigravity-cli/issues/1048). Its redacted conversation ID is replaced with a stable UUID. It represents a historical quota error in a completed response, not a failed resume.
- `antigravity-success-result-capture.jsonl` is the fresh-run `result` event from the same 1.2.6 report. Its redacted conversation ID is replaced with the stable UUID used in the init fixture so the fallback test can assert the newly captured session ID.

The Antigravity init and result lines are separate captured excerpts. The fallback test combines them to exercise session capture and result parsing; it does not claim they came from one recorded invocation. The version-gate tests use synthetic CLI version output to check argument selection and do not assign those versions to these captures.
