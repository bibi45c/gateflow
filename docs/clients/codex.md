# Codex CLI with FlowGate

[Codex CLI](https://github.com/openai/codex) is OpenAI's open-source coding agent for
the terminal. This guide connects it to a model over the OpenAI Responses API in two
ways:

- **Through the gateway.** Codex calls FlowGate's `POST /v1/responses` with a gateway
  key. This needs the Responses route of the gateway (#65), the seeded Responses
  channels (#64) and the mock's Responses scenarios (#63); none of them is on `main`
  yet, so [Through the gateway](#through-the-gateway) is a plan until they merge. The
  runs through the gateway belong to
  [issue #66](https://github.com/bibi45c/gateflow/issues/66).
- **Direct to Ollama.** Codex calls a local Ollama with no gateway in between. Run the
  [direct check](#direct-check) first. It shows whether Codex, Ollama and the model
  work together, before anyone looks for a fault in the gateway.

The two differ only in `base_url` and the key. A hosted OpenAI model is out of scope.

## Versions

Use these versions. They were checked together on Windows 11 with an 8 GB NVIDIA GPU
on 14 October 2026. Write down your own versions with every result.

| Component | Version | Notes |
|---|---|---|
| Codex CLI | 0.162.1 | npm package `@openai/codex@0.162.1` |
| Ollama | 0.40.3 | Serves `POST /v1/responses`; only this version was checked |
| Model | `qwen3:8b`, ID `500a1f067a9f` | Lists `tools`; maximum context 40,960 tokens |

What the model did with these versions:

| Model | Context in `ollama ps` | curl checks | The fixed task in Codex |
|---|---|---|---|
| `qwen3:8b` | 40960 | `text`, `tool` and `result` pass | Commands completed in 2 of 2 runs; tests passed in 1 (198 s, 9 commands); in the other the model never edited `calc.py` (122 s, 4 commands) |

## Codex configuration

Install Codex 0.162.1 with Node.js, then check the version; it prints
`codex-cli 0.162.1`:

```text
npm install -g @openai/codex@0.162.1
codex --version
```

Give Codex a home folder of its own for this guide. Codex reads `config.toml`, and
your sign-in, from the folder in `CODEX_HOME`, by default `~/.codex`. With a separate
folder the result depends only on the file below, and Codex cannot fall back to an
OpenAI account you are signed in with. Set it in every terminal that starts Codex.
macOS:

```text
mkdir -p ~/gateflow-codex
export CODEX_HOME=~/gateflow-codex
```

Windows PowerShell:

```text
New-Item -ItemType Directory -Force "$HOME\gateflow-codex"
$env:CODEX_HOME = "$HOME\gateflow-codex"
```

Put this `config.toml` in that folder, saved as UTF-8. It holds the gateway values:

```toml
model = "qwen3:8b"
model_provider = "gateflow"
model_context_window = 40960
sandbox_mode = "workspace-write"
approval_policy = "never"
check_for_update_on_startup = false

[analytics]
enabled = false

[windows]
sandbox = "unelevated"

[model_providers.gateflow]
name = "GateFlow"
base_url = "http://localhost:8080/v1"
wire_api = "responses"
env_key = "GATEFLOW_API_KEY"
```

- `base_url` ends in `/v1`. Codex adds `/responses`, so it sends
  `POST /v1/responses`. `wire_api = "responses"` selects the Responses API.
- `env_key` names the environment variable that holds the key, so the key is never
  written to a file. Codex sends it as `Authorization: Bearer <key>`, which the gateway
  reads as the gateway key ([api/access.yaml](../../api/access.yaml)). When the
  variable is not set, Codex stops with `Missing environment variable:
  GATEFLOW_API_KEY`.
- `model_context_window` is the context the model really runs with: the `CONTEXT`
  value that `ollama ps` prints. Codex has no metadata for `qwen3:8b` and warns
  "Model metadata for `qwen3:8b` not found"; this value takes the place of the
  missing context.
- `sandbox_mode` and `approval_policy` let Codex run commands and change files without
  asking, but only inside the folder it starts in and the temporary folder. Use a
  folder that holds nothing else.
- `[windows] sandbox = "unelevated"` is needed on Windows. Without it, Codex 0.162.1
  refused every command on the test machine with "blocked by policy", before the
  command started. The unelevated sandbox needs no administrator rights: it runs each
  command under a restricted token, and for that it adds an access-control entry to
  the start folder and to `%TEMP%`, one for each run. macOS ignores this table.
- `check_for_update_on_startup` and `analytics` stop Codex from checking for a new
  version and from sending usage analytics.

Only these values change between the two set-ups:

| Value | Through the gateway | Direct to Ollama |
|---|---|---|
| `base_url` | `http://localhost:8080/v1` | `http://localhost:11434/v1` |
| `GATEFLOW_API_KEY` | A seeded gateway key | Any text, such as `local-ollama`; Ollama ignores it |
| `model` | A model the gateway key may use | A name that `ollama list` prints |

Set the key in the terminal that starts Codex; these lines use the value for Ollama.
macOS:

```text
export GATEFLOW_API_KEY=local-ollama
```

Windows PowerShell:

```text
$env:GATEFLOW_API_KEY = "local-ollama"
```

### Check that Codex uses this file

Always pass `--strict-config`: a misspelt key is then an error instead of being
ignored. Without `--json`, `codex exec` prints a header before its first request.
Check these lines of it before you look at anything else:

```text
model: qwen3:8b
provider: gateflow
approval: never
sandbox: workspace-write [workdir, /tmp, $TMPDIR]
```

Any other `provider`, such as `openai`, means Codex did not read this file: check
`CODEX_HOME`. Through the gateway, also check the gateway log, as
[Through the gateway](#through-the-gateway) describes.

### What Codex sends

Recorded from Codex 0.162.1 against Ollama:

- `stream: true`, `store: false` and no `previous_response_id`. Each request carries
  the whole conversation, which the stateless gateway needs: it refuses
  `previous_response_id` ([api/errors.md](../../api/errors.md)).
- No `max_output_tokens`. The gateway then holds 32,768 output tokens for each call
  and sends that bound upstream ([api/metering.yaml](../../api/metering.yaml)). The
  first request of a session is already about 41 KB, so under the seeded price
  version `pv-seed-1` it holds 40,960 × 3 + 32,768 × 15 = 614,400 micro-units, and
  later requests hold more as the conversation grows.
- The instructions as `developer` messages in `input`, not as `instructions`.
- Function tools, one `namespace` tool (`multi_agent_v1`) with nested tools, and the
  hosted `web_search` tool.
- Headers such as `session-id` and `x-codex-turn-metadata`, which carry an
  installation ID and session IDs. The gateway forwards none of them upstream
  ([api/access.yaml](../../api/access.yaml)).

## Direct check

Do the steps in order; each one rules out a layer.

### 1. Start Ollama and pull the model

Follow steps 1 and 2 of the OpenCode guide's direct check:
[install and start Ollama](opencode.md#1-install-and-start-ollama) with a 64,000-token
context, then [pull and check the model](opencode.md#2-pull-and-check-the-model).
Put the `CONTEXT` value of `ollama ps` in `model_context_window`.

### 2. Check Ollama with curl

This step uses no Codex. Save each request body as a file, as UTF-8 without a
byte-order mark.

`text.json`:

```json
{"model": "qwen3:8b", "max_output_tokens": 2048, "stream": true, "input": "Reply with the word pong."}
```

`tool.json`:

```json
{"model": "qwen3:8b", "max_output_tokens": 2048, "stream": true, "tools": [{"type": "function", "name": "read_file", "description": "Read a file in the project", "parameters": {"type": "object", "properties": {"path": {"type": "string"}}, "required": ["path"]}}], "input": "What is in calc.py? Use the read_file tool."}
```

`result.json`, the tool round trip that Codex relies on:

```json
{"model": "qwen3:8b", "max_output_tokens": 2048, "stream": true, "tools": [{"type": "function", "name": "read_file", "description": "Read a file in the project", "parameters": {"type": "object", "properties": {"path": {"type": "string"}}, "required": ["path"]}}], "input": [{"type": "message", "role": "user", "content": "What is in calc.py? Use the read_file tool."}, {"type": "function_call", "call_id": "call_1", "name": "read_file", "arguments": "{\"path\": \"calc.py\"}"}, {"type": "function_call_output", "call_id": "call_1", "output": "def add(a, b):\n    return a - b\n"}]}
```

Send each file. On Windows write `curl.exe`, and keep the quotes around `@text.json`;
on macOS write `curl`:

```text
curl.exe -sN http://localhost:11434/v1/responses -H "content-type: application/json" --data-binary "@text.json"
```

| File | Pass when |
|---|---|
| `text.json` | The events start with `response.created` and end with `response.completed`, which has `usage`, and the `output_text` is "pong" |
| `tool.json` | A `response.output_item.done` holds an item of type `function_call` with `"name":"read_file"` and `"arguments":"{\"path\":\"calc.py\"}"` |
| `result.json` | The text answer says that `add` subtracts |

`qwen3:8b` streams a reasoning summary (`response.reasoning_summary_text.delta`)
before the answer or the tool call. That is expected. If `tool.json` or `result.json`
fails, the fault is in Ollama or the model, not in Codex.

### 3. Run the fixed task in Codex

Create a folder outside the repository with `calc.py` and `test_calc.py` from the
OpenCode guide's [fixed task](opencode.md#4-run-the-fixed-task-in-opencode), and check
that `python -m unittest` fails there. Set `base_url` to the Ollama value, then start
Codex in that folder:

```text
codex exec --strict-config --skip-git-repo-check --ephemeral "Run python -m unittest in this folder, find why the test fails, fix calc.py, and run the tests again."
```

- `--skip-git-repo-check` is needed because the folder is not a Git repository;
  `--ephemeral` keeps no session files.
- On Windows Codex runs each command through PowerShell. The model writes some
  PowerShell commands that fail, such as a parameter that does not exist; one
  recorded run had two. That is the model, not the sandbox.
- Do not edit any file by hand. Run the task 3 times in all. Before each run, put the
  subtraction back in `calc.py`.

- **Pass (required):** in at least one run, at least one command completes and its
  output goes back to the model. You see the test output, and then a further answer or
  command that uses it.
- **Recorded, not required:** in how many of the 3 runs `python -m unittest` passes
  at the end.

### 4. Record the result

Add a comment to [issue #66](https://github.com/bibi45c/gateflow/issues/66) with this
table:

| Item | Value |
|---|---|
| Machine | OS, CPU, GPU and memory |
| Versions | `ollama --version`, `codex --version`, and the model name and ID from `ollama list` |
| Context | `CONTEXT` and `PROCESSOR` from `ollama ps` |
| curl checks | `text`, `tool` and `result`: pass or fail |
| Codex runs | For each of the 3 runs: commands completed, tests passed, total time |
| Failures | For each failure: the layer (Codex, Ollama or model) and the error text |

## Through the gateway

This section is a plan: it is completed, and its commands checked, when #63, #64 and
#65 have merged. The runs and their pass rules are those of
[issue #66](https://github.com/bibi45c/gateflow/issues/66).

1. Start the stack and get a key, as for OpenCode
   ([issue #37](https://github.com/bibi45c/gateflow/issues/37)):

   ```text
   docker compose -f deploy/compose/compose.yaml up --build --detach
   docker compose -f deploy/compose/compose.yaml logs seed-access
   ```

   Copy the key on the `tenant-01` line into `GATEFLOW_API_KEY`, and set `base_url`
   to `http://localhost:8080/v1`.
2. **Mock run**, by someone who did not write this guide: set `model` to
   `mock-coding` and run the fixed task of the mock; its files and task text follow
   #63.
3. **Ollama run**, with a GPU: set `model` to `qwen3:8b`, with Ollama on the host as in
   the [direct check](#direct-check), and run the fixed task above.
4. Check that every request went through the gateway:
   - the Codex header shows `provider: gateflow`;
   - the gateway log has one `request` line for each request, with the route of
     `/v1/responses` and its `status`:

     ```text
     docker compose -f deploy/compose/compose.yaml logs gateway
     ```

   - the ledger has one settled row for each request:

     ```text
     docker compose -f deploy/compose/compose.yaml exec postgres psql -U gateflow -d gateflow -c "select call_id, outcome, amount from accounting.ledger_entries order by created_at desc limit 10;"
     ```

5. Stop the stack with `docker compose -f deploy/compose/compose.yaml down --volumes`.

## Troubleshooting

| Symptom | Likely cause | Try |
|---|---|---|
| Every command fails with `rejected: blocked by policy`, and Codex suggests running as administrator | The default Windows sandbox could not run the command | Set `[windows] sandbox = "unelevated"` |
| The first command is refused: "you cannot ask for escalated permissions if the approval policy is Never" | The model asked to run outside the sandbox (`sandbox_permissions: require_escalated`) | Nothing to fix; in the recorded run the model then tried again without it. Count the run as it ends |
| `Missing environment variable: GATEFLOW_API_KEY` | The key is not set in this terminal | Set `GATEFLOW_API_KEY` |
| `Reconnecting... waiting for network`, repeated | Nothing answers at `base_url` | Start Ollama or the stack; check the port in `base_url` |
| The header shows another `provider` | Codex did not read this `config.toml` | Set `CODEX_HOME` to the folder that holds it, in the same terminal |
| `Model metadata for qwen3:8b not found` | Codex has no built-in metadata for this model | Expected; keep `model_context_window` |
| `codex exec` prints `Reading additional input from stdin...`, and may wait | Its input is not a terminal, for example in a script: Codex reads it to the end and adds it to the prompt | Start it from a terminal, or give it an empty file as input |
| 500 with `llama-server process no longer running` and `CUDA error` on the first request | Ollama failed to load the model the first time | Send the request again; it loaded on the second try on the test machine |
| The task reports that `python` was not found, or the Microsoft Store opens | Windows has only the Store alias; macOS has `python3` | Install Python 3.13 on Windows; write `python3` in the task on macOS |
| Codex reads the files and names the fix, but `calc.py` is unchanged at the end | The model ended without an edit command | A model limit; record the run and run the task again |
| More than 60 s to the first output | The model is loading, or runs partly on the CPU | See the OpenCode guide's [troubleshooting](opencode.md#troubleshooting) |

If the model is the problem, fall back as the OpenCode guide describes: a smaller
model, another model with tool calling, or a faster machine. Record on the issue
which layer failed and the error text.
