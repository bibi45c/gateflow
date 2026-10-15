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

## Written by hand

`codex-style.request.json` is written by hand, not recorded. It follows the public
OpenAI Responses request format in the shape Codex sends: `instructions`, an `input`
array with `message`, `function_call` and `function_call_output` items, function
`tools` with `parameters`, `stream: true`, `store: false`, `include` and `reasoning`,
and no `max_output_tokens`. A request recorded from Codex 0.162.1 replaces it in
issue #66.
