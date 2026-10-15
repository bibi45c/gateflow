# OpenCode with FlowGate

[OpenCode](https://opencode.ai) is an open-source coding agent. This guide connects it
to a model over the Anthropic Messages API in two ways:

- **Through the gateway.** OpenCode calls FlowGate with a gateway key. This is the
  set-up the project is built for. The gateway relay and its key check are on `main`
  (#54, #55); it also needs the access and metering endpoints (#26, #27) and the
  seeded stack. The check through the gateway belongs to
  [issue #37](https://github.com/bibi45c/gateflow/issues/37).
- **Direct to Ollama.** OpenCode calls a local Ollama with no gateway in between. Run
  the [direct check](#direct-check) first. It shows whether OpenCode, Ollama and the
  model work together, before anyone looks for a fault in the gateway.

The two differ only in `baseURL` and the key.

## Versions

Use these versions. They were checked together on Windows 11 with an 8 GB NVIDIA GPU;
the full record is on [issue #36](https://github.com/bibi45c/gateflow/issues/36).
Write down your own versions with every result.

| Component | Version | Notes |
|---|---|---|
| OpenCode | 1.19.0 | |
| Ollama | 0.40.3 | `/v1/messages` exists from 0.14.0; older versions answer 404 |
| Model | `qwen3:8b`, ID `500a1f067a9f` | Lists `tools`; maximum context 40,960 tokens |

What each model did with these versions:

| Model | Context in `ollama ps` | curl tool check | The fixed task in OpenCode |
|---|---|---|---|
| `qwen3:8b` | 40960 | Pass | Tool calls completed and tests passed in 3 of 3 runs |
| `qwen2.5-coder:7b` | 32768 | Fail: the tool call arrives as text | No tool call in 6 of 6 runs |

## OpenCode configuration

Install OpenCode 1.19.0 in one of two ways:

- With Node.js: `npm install -g opencode-ai@1.19.0`.
- Without Node.js: download the archive for your OS from the
  [v1.19.0 release](https://github.com/anomalyco/opencode/releases/tag/v1.19.0),
  unpack it, and put the folder that holds `opencode` on your `PATH`.

Check the version:

```text
opencode --version
```

Put this `opencode.json` in the folder where you start OpenCode. It holds the gateway
values:

```json
{
  "$schema": "https://opencode.ai/config.json",
  "provider": {
    "gateflow": {
      "npm": "@ai-sdk/anthropic",
      "name": "GateFlow",
      "options": {
        "baseURL": "http://localhost:8080/v1",
        "apiKey": "{env:GATEFLOW_API_KEY}"
      },
      "models": {
        "qwen3:8b": {
          "name": "Qwen3 8B",
          "limit": {"context": 40960, "output": 4096}
        }
      }
    }
  },
  "model": "gateflow/qwen3:8b",
  "small_model": "gateflow/qwen3:8b",
  "enabled_providers": ["gateflow"],
  "share": "disabled",
  "autoupdate": false
}
```

- `baseURL` ends in `/v1`. The Anthropic client adds `/messages`, so OpenCode sends
  `POST /v1/messages`.
- `apiKey` reads the key from the environment variable `GATEFLOW_API_KEY`, so the key
  is never written to a file. OpenCode sends it in the `x-api-key` header.
- `limit.context` is the context the model really runs with: the `CONTEXT` value that
  `ollama ps` prints, not the value you asked Ollama for. With a larger number,
  OpenCode may send prompts that do not fit, and Ollama cuts them short.
- `limit.output` is the value OpenCode sends as `max_tokens`. Keep it at or below
  32,768, the gateway's cap on `max_tokens`.
- `small_model` is the model OpenCode uses for short side requests: every new session
  starts with one that writes the session title. Set it to a model the gateway key
  may use, so that these requests are not refused.
- `enabled_providers` keeps OpenCode to this one provider. `share` and `autoupdate`
  stop it from uploading sessions or replacing the checked version.

Only these values change between the two set-ups:

| Value | Through the gateway | Direct to Ollama |
|---|---|---|
| `baseURL` | `http://localhost:8080/v1` | `http://localhost:11434/v1` |
| `GATEFLOW_API_KEY` | A seeded gateway key | Any text, such as `local-ollama`; Ollama ignores it |
| Model | A model the gateway key may use | A name that `ollama list` prints |

Set the key in the terminal that starts OpenCode; these lines use the value for
Ollama. macOS:

```text
export GATEFLOW_API_KEY=local-ollama
```

Windows PowerShell:

```text
$env:GATEFLOW_API_KEY = "local-ollama"
```

OpenCode also reads your own global configuration, in `~/.config/opencode` on every
OS, and merges it with this file. If you have providers, MCP servers or permissions
there, point `XDG_CONFIG_HOME` at an empty folder for the check, so the result
depends only on this file.

## Name the model

In both set-ups, start OpenCode with `-m` and the model from `opencode.json`:

```text
opencode -m gateflow/qwen3:8b
opencode run -m gateflow/qwen3:8b '<task>'
```

Without `-m`, an OpenCode that did not load `opencode.json` falls back to its own
hosted model, such as `opencode/big-pickle`. The session looks normal, but no request
reaches the gateway or Ollama. With `-m`, a missing `gateflow` provider is an error.

## Confirm the provider

Check every session; a normal-looking answer does not prove where it came from.

- In OpenCode's output, the provider is `gateflow`.
- Through the gateway, the gateway log has one line per request with
  `"msg":"request"` and `"route":"/v1/messages"`. In the compose stack, read it with:

```text
docker compose -f deploy/compose/compose.yaml logs gateway
```

## Direct check

Do the steps in order; each one rules out a layer. Allow half a day, most of it
downloads: the Windows installer is about 1.6 GB and the model 5.2 GB.

### 1. Install and start Ollama

Install Ollama on the host, not in Docker, so it can use the GPU. Docker on macOS has
no GPU. For the container, see [Ollama in compose](#ollama-in-compose).

- **macOS:** the app from [ollama.com](https://ollama.com/download), or
  `brew install ollama`.
- **Windows:** the installer from [ollama.com](https://ollama.com/download), or
  `winget install --id Ollama.Ollama --exact`. Open a new terminal afterwards, so that
  `ollama` is on the `PATH`.

Check the version. It must be 0.14.0 or later:

```text
ollama --version
```

The installer starts a tray or menu-bar app; on Windows the app also starts at every
sign-in. It runs its own server with the default context, which Ollama derives from
the GPU memory: 4,096 tokens on the 8 GB test GPU, less than OpenCode's first request
of about 6,400 tokens. Quit the app from its icon, then start the server in a
terminal and ask for a 64,000-token context, so that long prompts from a coding
client are not cut short. Leave this terminal open.

macOS:

```text
export OLLAMA_CONTEXT_LENGTH=64000
ollama serve
```

Windows PowerShell:

```text
$env:OLLAMA_CONTEXT_LENGTH = "64000"
ollama serve
```

Ollama caps the context at the model's own maximum: 40,960 tokens for `qwen3:8b`. If
port 11434 is taken or reserved (on Windows,
`netsh interface ipv4 show excludedportrange protocol=tcp` lists reserved ranges),
set `OLLAMA_HOST=127.0.0.1:<free port>` in the same way before `ollama serve`, and
use that port everywhere below.

### 2. Pull and check the model

In a second terminal:

```text
ollama pull qwen3:8b
ollama show qwen3:8b
ollama run qwen3:8b --verbose "Say hi"
ollama ps
```

- `ollama show` must list `tools` under Capabilities.
- `ollama run --verbose` prints `eval rate` in tokens per second. Run it twice and
  record the second value; the first run also loads the model.
- `ollama ps` shows the loaded model. Record `CONTEXT` and `PROCESSOR`. A `PROCESSOR`
  value such as `48%/52% CPU/GPU` means the model does not fit in GPU memory and
  runs partly on the CPU, which is slower.

A smaller context needs less memory. On the 8 GB test GPU, `qwen3:8b` ran at 22
tokens per second with 40,960 tokens of context, and at 39 with
`OLLAMA_CONTEXT_LENGTH=16384`, which was enough for the task in step 4. If you lower
the context, restart `ollama serve` and put the same value in `limit.context`.

### 3. Check Ollama with curl

This step uses no OpenCode. Save each request body as a file, so the same command
works on every OS. Save the files as UTF-8 without a byte-order mark; in Windows
PowerShell 5.1 the `>` operator writes UTF-16, so use an editor.

`text.json`:

```json
{"model": "qwen3:8b", "max_tokens": 1024, "stream": true, "messages": [{"role": "user", "content": "Reply with the word pong."}]}
```

`tool.json`:

```json
{"model": "qwen3:8b", "max_tokens": 1024, "stream": true, "tools": [{"name": "read_file", "description": "Read a file in the project", "input_schema": {"type": "object", "properties": {"path": {"type": "string"}}, "required": ["path"]}}], "messages": [{"role": "user", "content": "What is in calc.py? Use the read_file tool."}]}
```

`result.json`, the tool round trip that OpenCode relies on:

```json
{"model": "qwen3:8b", "max_tokens": 1024, "stream": true, "tools": [{"name": "read_file", "description": "Read a file in the project", "input_schema": {"type": "object", "properties": {"path": {"type": "string"}}, "required": ["path"]}}], "messages": [{"role": "user", "content": "What is in calc.py? Use the read_file tool."}, {"role": "assistant", "content": [{"type": "tool_use", "id": "toolu_1", "name": "read_file", "input": {"path": "calc.py"}}]}, {"role": "user", "content": [{"type": "tool_result", "tool_use_id": "toolu_1", "content": "def add(a, b):\n    return a - b\n"}]}]}
```

Send each file. On Windows write `curl.exe`, because `curl` in Windows PowerShell 5.1
is a different command; on macOS write `curl`. Keep the quotes around `@text.json`:
without them PowerShell reads `@` as its own operator and stops with an error.

```text
curl.exe -sN http://localhost:11434/v1/messages -H "content-type: application/json" -H "anthropic-version: 2023-06-01" --data-binary "@text.json"
```

To measure the time to the first byte, send `text.json` once more with
`-o text.sse -w "%{time_starttransfer}"` added.

| File | Pass when |
|---|---|
| `text.json` | The events are `message_start`, `content_block_start`, one or more `content_block_delta`, `content_block_stop`, `message_delta` with `usage`, then `message_stop`, and the text is "pong" |
| `tool.json` | A `content_block_start` has `"type":"tool_use"` and `"name":"read_file"`, the `input_json_delta` events build `{"path":"calc.py"}`, and `stop_reason` is `tool_use` |
| `result.json` | The text answer says that `add` subtracts |

`qwen3:8b` thinks before it answers, so a `thinking` block comes before the text or
the tool call. That is expected. If `tool.json` or `result.json` fails, the fault is
in Ollama or the model, not in OpenCode; see [Troubleshooting](#troubleshooting).

### 4. Run the fixed task in OpenCode

Create a folder outside the repository with three files: `opencode.json` from
[OpenCode configuration](#opencode-configuration), with the `baseURL` for Ollama, and
these two.

`calc.py`:

```python
def add(a, b):
    return a - b
```

`test_calc.py`:

```python
import unittest

from calc import add


class AddTest(unittest.TestCase):
    def test_add(self):
        self.assertEqual(add(2, 3), 5)


if __name__ == "__main__":
    unittest.main()
```

Check that `python -m unittest` fails in that folder. On macOS write `python3`, here
and in the task below, if `python` is not Python 3.

Set `GATEFLOW_API_KEY`, start `opencode -m gateflow/qwen3:8b` in the folder, confirm
the provider as in [Confirm the provider](#confirm-the-provider), and send this task:

```text
Run python -m unittest in this folder, find why the test fails, fix calc.py, and run the tests again.
```

Do not edit any file by hand. If OpenCode asks for a permission, approve it. Run the
task 3 times in all. Before each run, start a new session and put the subtraction
back in `calc.py`.

The recorded check added this block to `opencode.json`. It lets OpenCode edit files
and run commands in the folder without asking, and nothing else:

```json
"permission": {"edit": "allow", "bash": "allow", "webfetch": "deny", "external_directory": "deny"}
```

It also ran the task without the interactive screen. Pass the task in single quotes:

```text
opencode run -m gateflow/qwen3:8b 'Run python -m unittest in this folder, find why the test fails, fix calc.py, and run the tests again.'
```

- **Pass (required):** in at least one run, at least one tool call completes and its
  result goes back to the model. You see the test output, and then a further answer
  or tool call that uses it.
- **Recorded, not required:** in how many of the 3 runs `python -m unittest` passes at
  the end.

OpenCode downloads `ripgrep` from GitHub the first time it starts, so that start needs
internet access.

### 5. Record the result

Add a comment to [issue #36](https://github.com/bibi45c/gateflow/issues/36) with this
table:

| Item | Value |
|---|---|
| Machine | OS, CPU, GPU and memory |
| Versions | `ollama --version`, `opencode --version`, and the model name and ID from `ollama list` |
| Context | `CONTEXT` and `PROCESSOR` from `ollama ps` |
| Speed | The eval rate in tokens per second, and the time to the first byte from step 3 |
| curl checks | `text`, `tool` and `result`: pass or fail |
| OpenCode runs | For each of the 3 runs: tool calls completed, tests passed, total time |
| Failures | For each failure: the layer (OpenCode, Ollama or model) and the error text |

## Ollama in compose

The compose stack has an optional `ollama` service for people who do not install
Ollama on the host. It starts only with `--profile ollama`, pins the image
`ollama/ollama:0.40.3`, asks for a 64,000-token context and publishes port 11434 on
localhost, so the curl checks and OpenCode work against it unchanged. It runs on the
CPU, so expect it to be much slower than Ollama on a host with a GPU.

Start the stack with the profile, then pull the model into the container:

```text
docker compose -f deploy/compose/compose.yaml --profile ollama up --build --detach
docker compose -f deploy/compose/compose.yaml --profile ollama exec ollama ollama pull qwen3:8b
docker compose -f deploy/compose/compose.yaml --profile ollama exec ollama ollama ps
```

To start Ollama alone, without building the services, replace the first command with:

```text
docker compose -f deploy/compose/compose.yaml --profile ollama up --detach ollama
```

- **Port.** A host-installed Ollama already uses port 11434. Stop it first, or put
  `OLLAMA_PORT=<free port>` in `deploy/compose/.env`, which Git ignores.
- **Context.** Put `OLLAMA_CONTEXT_LENGTH=<tokens>` in the same file to ask for
  another context. A smaller context needs less memory.
- **Models.** Pulled models live in the `ollama-models` volume. Stop the stack with
  `down` to keep them; `down --volumes` deletes them.
- **Gateway.** The stack reaches Ollama through `OLLAMA_BASE_URL`, which defaults to
  an Ollama on the host, `http://host.docker.internal:11434`. To use the container,
  put `OLLAMA_BASE_URL=http://ollama:11434` in `deploy/compose/.env`. Write it without
  `/v1`: the gateway adds the path, and `/v1` belongs only in the client's `baseURL`.
  `OLLAMA_MODEL` defaults to `qwen3:8b`, the model of this guide; set it there if you
  pull another one. Every variable of the stack is listed in
  [deploy/compose/README.md](../../deploy/compose/README.md).

## Troubleshooting

| Symptom | Likely cause | Try |
|---|---|---|
| 404 on `/v1/messages`, with `ollama --version` below 0.14.0 | Ollama is too old | Upgrade to the version under [Versions](#versions) |
| 404 with `not_found_error` and the model name | The model is not pulled, or the name is misspelt | `ollama list`; use the exact name |
| The model writes tool JSON as text and there is no `tool_use` block; OpenCode stops after one answer and runs nothing | The model, as Ollama serves it, does not produce tool calls. `qwen2.5-coder:7b` does this | Use `qwen3:8b`, or another model that passes `tool.json` |
| `text.json` ends with `stop_reason` `max_tokens` and no text | A thinking model used all of `max_tokens` on its thinking | Raise `max_tokens` |
| PowerShell stops at `@text.json` with "The splatting operator '@' cannot be used" | The file argument has no quotes | Write `"@text.json"` |
| `ollama ps` shows a smaller `CONTEXT` than you asked for | Ollama capped it at the model's maximum | Put the value shown in `limit.context` |
| The model loses instructions in long prompts | The context was not applied | Quit the tray app, restart `ollama serve` with `OLLAMA_CONTEXT_LENGTH`, and check `ollama ps` |
| OpenCode calls `api.anthropic.com`, or says the key is missing | The config was not picked up, or `baseURL` is wrong | Start OpenCode in the folder with `opencode.json`; check that `baseURL` ends in `/v1`; set `GATEFLOW_API_KEY` |
| The session looks normal, but the gateway log has no request line; OpenCode used a model such as `opencode/big-pickle` | OpenCode did not load `opencode.json` and fell back to its hosted model. On Git Bash it takes the project folder from the inherited `PWD`, which can point elsewhere | Start OpenCode from the task folder, or set `PWD` to it; always pass `-m`, as in [Name the model](#name-the-model) |
| OpenCode shows other providers, MCP servers or permissions | Your global OpenCode configuration was merged in | Point `XDG_CONFIG_HOME` at an empty folder for the check |
| More than 60 s to the first token | The model is loading, or runs partly on the CPU | Wait for the first request to finish, then check `PROCESSOR` in `ollama ps`; use a smaller `OLLAMA_CONTEXT_LENGTH`, a smaller model or a faster machine |
| Out of memory | The model with its context is too large | Use a smaller model or a smaller `OLLAMA_CONTEXT_LENGTH`; close other programs |
| The `winget` download stays at 0 MB | A stalled download | Download the installer from ollama.com |
| The task reports that `python` was not found, or the Microsoft Store opens | Windows has only the Store alias; macOS has `python3` | Install Python 3.13 on Windows; write `python3` in the task on macOS |
| The container does not start: port 11434 is in use | A host-installed Ollama is running | Stop it, or set `OLLAMA_PORT` |

If the model is the problem, fall back in this order: a smaller model, another model
with tool calling, a faster teammate machine, then Ollama Cloud, which needs an
account. Record on the issue which layer failed and the error text.
