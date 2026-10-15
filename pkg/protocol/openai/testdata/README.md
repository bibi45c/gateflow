# Responses test data

Every file uses LF line endings and holds no `\r`; the tests make other line endings
themselves. Each `.sse` file ends with the blank line that ends its last event.

## Recorded on Ollama

Recorded on Ollama 0.40.3 with the model `qwen3:4b-instruct-2507-q4_K_M` on
2026-10-14, and kept byte for byte:

| File | Contents |
|---|---|
| `ollama-tool-stream.request.json` | A streaming request with one function tool |
| `ollama-tool-stream.sse` | Its stream: one `function_call`, usage 157 input and 19 output tokens |
| `ollama-text.request.json` | A non-streaming text request |
| `ollama-text.response.json` | Its response body |
| `ollama-text-stream.request.json` | The same text request, streaming |
| `ollama-text-stream.sse` | Its stream |
| `ollama-unknown-model.request.json` | A request for a model Ollama does not have; no newline at the end |
| `ollama-unknown-model.response.json` | Its HTTP 404 body |

## Recorded from Codex

`codex-style.request.json` is the second request of a `codex exec` session of Codex
CLI 0.162.1 on Windows, on 2026-10-14 (issue #66): the turn after one tool call, sent
to a custom provider with `wire_api = "responses"` and recorded by a proxy in front of
Ollama 0.40.3, model `qwen3:8b`. It has no `instructions` and no `max_output_tokens`;
its `input` starts with two `developer` messages and ends with a `function_call` to
`exec_command`, the `reasoning` item of that turn and the `function_call_output`; its
`tools` hold functions, the namespace `multi_agent_v1` and the built-in `web_search`.

It is not byte for byte. The recorded body of 43,514 bytes was written back as compact
JSON in the same member order, with these changes:

- the text of each `developer` message and every `description` in `tools`, which are
  Codex's own wording, is replaced by a note of its length;
- the local paths are replaced by `C:\work\calc` and `C:\Users\dev\.codex`, and the
  time zone by `Etc/UTC`;
- each UUID (installation, session, thread, turn and item ids) is replaced by
  `00000000-0000-7000-8000-00000000000N`, the same value for the same id.
