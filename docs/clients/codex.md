# Codex CLI with FlowGate

[Codex CLI](https://github.com/openai/codex) is OpenAI's open-source coding agent for
the terminal. This guide connects it to a model over the OpenAI Responses API in two
ways:

- **Through the gateway.** Codex calls FlowGate's `POST /v1/responses` with a gateway
  key. This needs the Responses route of the gateway (#65), the seeded Responses
  channels (#64) and the mock's Responses scenarios (#63); all are on `main`.
  [Through the gateway](#through-the-gateway) gives the two acceptance runs. The
  runs through the gateway belong to
  [issue #66](https://github.com/bibi45c/gateflow/issues/66).
- **Direct to Ollama (optional).** The [direct check](#direct-check) helps diagnose
  a model or Ollama failure. It is not required for #66 and does not establish
  that requests reached the gateway.

The two differ only in `base_url` and the key. A hosted OpenAI model is out of scope.

## Versions

Use these versions. They were checked together on Windows 11 with an 8 GB NVIDIA GPU
on 14 October 2026. Write down your own versions with every result.

| Component | Version | Notes |
|---|---|---|
| Codex CLI | 0.162.1 | npm package `@openai/codex@0.162.1` |
| Ollama | 0.40.3 | Serves `POST /v1/responses`; only this version was checked |
| Model | `qwen3:8b`, ID `500a1f067a9f` | Lists `tools`; maximum context 40,960 tokens |

The CLI version was checked locally. See the
[official configuration reference](https://developers.openai.com/codex/config-reference/)
for the provider settings; later versions need their own check. The direct trial
below is supporting evidence, not the two post-merge acceptance runs.

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

Use a new terminal and give Codex a new, empty home folder for this guide. Codex
reads `config.toml`, and your sign-in, from the folder in `CODEX_HOME`, by default
`~/.codex`. With a separate
folder the result depends only on the file below, and Codex cannot fall back to an
OpenAI account you are signed in with. Clear the inherited API overrides as below.
Do not add `--oss`, `--local-provider` or another profile. Set these variables in
every terminal that starts Codex.
macOS:

```text
mkdir -p ~/gateflow-codex-66
export CODEX_HOME=~/gateflow-codex-66
unset OPENAI_API_KEY OPENAI_BASE_URL CODEX_API_KEY
```

Windows PowerShell:

```text
New-Item -ItemType Directory -Force "$HOME\gateflow-codex-66"
$env:CODEX_HOME = "$HOME\gateflow-codex-66"
Remove-Item Env:OPENAI_API_KEY, Env:OPENAI_BASE_URL, Env:CODEX_API_KEY -ErrorAction SilentlyContinue
```

If the folder already held files before these commands, choose a new name. Put
this `config.toml` in that folder,
saved as UTF-8. These values start with the mock through the gateway:

```toml
model = "mock-coding"
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
base_url = "http://127.0.0.1:8080/v1"
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
- For Ollama, `model_context_window` is the context the model really runs with: the `CONTEXT`
  value that `ollama ps` prints. Codex has no metadata for `qwen3:8b` and warns
  "Model metadata for `qwen3:8b` not found"; this value takes the place of the
  missing context. The mock has no model context limit; 40,960 is sufficient here.
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
| `base_url` | `http://127.0.0.1:8080/v1` | `http://127.0.0.1:11434/v1` |
| `GATEFLOW_API_KEY` | A seeded gateway key | Any text, such as `local-ollama`; Ollama ignores it |
| `model` | A model the gateway key may use | A name that `ollama list` prints |

Set the key in the terminal that starts Codex. These lines are only for the optional
direct check; for gateway runs use the seeded key described below.
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
model: mock-coding
provider: gateflow
approval: never
sandbox: workspace-write [workdir, /tmp, $TMPDIR]
```

For Ollama the model must instead be `qwen3:8b`. Any other `provider`, such as
`openai`, means the intended configuration is not active: stop and check
`CODEX_HOME`, environment and CLI overrides. The header alone is insufficient;
through the gateway, also check every request in the gateway log and ledger, as
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

These are optional diagnostics; no direct runs are required for #66. Do the steps
in order when investigating a failure. Restore the gateway URL and seeded key
before collecting gateway acceptance evidence.

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
that `python -m unittest` fails there. Set `model` to `qwen3:8b`, `base_url` to the
Ollama value and `GATEFLOW_API_KEY` to `local-ollama`, then start Codex in that folder:

```text
codex exec --strict-config --skip-git-repo-check --ephemeral "Run python -m unittest in this folder, find why the test fails, fix calc.py, and run the tests again."
```

- `--skip-git-repo-check` is needed because the folder is not a Git repository;
  `--ephemeral` keeps no session files.
- On Windows Codex runs each command through PowerShell. The model writes some
  PowerShell commands that fail, such as a parameter that does not exist; one
  recorded run had two. That is the model, not the sandbox.
- Do not edit any file by hand during a run. If repeating a diagnostic, restore
  the subtraction in `calc.py` before starting a new session.

- **Pass (required):** in at least one run, at least one command completes and its
  output goes back to the model. You see the test output, and then a further answer or
  command that uses it.
- **Recorded:** whether `python -m unittest` passes at the end.

### 4. Record the result

Keep the diagnostic result locally, separate from gateway acceptance:

| Item | Value |
|---|---|
| Machine | OS, CPU, GPU and memory |
| Versions | `ollama --version`, `codex --version`, and the model name and ID from `ollama list` |
| Context | `CONTEXT` and `PROCESSOR` from `ollama ps` |
| curl checks | `text`, `tool` and `result`: pass or fail |
| Codex runs | Commands completed, tests passed, total time |
| Failures | For each failure: the layer (Codex, Ollama or model) and the error text |

## Through the gateway

### 1. Start and seed a separate stack

Use Docker Desktop (or Docker Engine with Compose on Linux), Python 3.13 or newer
and the pinned Codex version. Run compose commands from the repository root, always
with the project name `codex66`. Keep other clients off this dedicated stack while
collecting evidence. If ports are occupied, change `*_PORT` in
`deploy/compose/.env` as described in the
[compose configuration](../../deploy/compose/README.md#configuration), and update
the port in Codex's `base_url`.

```text
docker compose -p codex66 -f deploy/compose/compose.yaml up --build --detach --wait
docker compose -p codex66 -f deploy/compose/compose.yaml ps --all
docker compose -p codex66 -f deploy/compose/compose.yaml logs seed-access seed-metering
```

Both seed jobs must exit with code 0; the long-running services must be healthy.
Copy the key on the `tenant-01` line into `GATEFLOW_API_KEY` in the terminal that
starts Codex. `tenant-00` deliberately has no balance. macOS:

```text
export GATEFLOW_API_KEY="<tenant-01 key from seed-access>"
```

Windows PowerShell:

```text
$env:GATEFLOW_API_KEY = "<tenant-01 key from seed-access>"
```

Keep `base_url = "http://127.0.0.1:8080/v1"`. The seeded key allows Responses
requests to `mock-*` and the configured Ollama model. The initial wallet balance
is 100,000,000 micro-units. Do not share the key or seed log.

### 2. Prepare the fixed task

Create a new folder outside the repository for each run, containing only `calc.py`
and `test_calc.py` from the OpenCode guide's
[fixed task](opencode.md#4-run-the-fixed-task-in-opencode). Do not add its
`opencode.json`; Codex uses the isolated home above. Run `python -m unittest` in
the task folder: it must fail with `-1 != 5` before Codex starts.

Python must be available as `python` in Codex's shell. The mock's scripted command
is literally `python -m unittest`; if macOS only has `python3`, make `python`
available before this run. On Windows, check that it runs Python rather than the
Microsoft Store alias.

Before each run, record the UTC time from the database:

```text
docker compose -p codex66 -f deploy/compose/compose.yaml exec -T postgres psql -U gateflow -d gateflow -c "SELECT clock_timestamp() AT TIME ZONE 'UTC' AS run_start_utc;"
```

### 3. Run the mock

After this guide is merged, someone who did not write it (Agnes for #66) follows
the guide once. This needs no GPU or Ollama. Keep `model = "mock-coding"`, the
gateway URL and the seeded key. From the broken task folder:

```text
codex exec --strict-config --skip-git-repo-check --ephemeral "Run python -m unittest in this folder, find why the test fails, fix calc.py, and run the tests again."
python -m unittest
```

Retain the transcript locally. Expect three successful tool steps: read `calc.py`,
change subtraction to addition, run `python -m unittest`, then a final answer.
The mock advances by the number of `function_call_output` items it receives. Check
that all three steps succeed, `calc.py` contains `return a + b`, and the independent
test prints `OK`; its scripted closing text alone proves none of these.

Normally this uses four model requests, including the final one carrying the test
result. Record additional requests or retries and reconcile every request in
step 5. Pass requires task completion, all tool calls and results relayed, and
exactly one settled ledger row per request.

### 4. Run Ollama on the GPU machine

cpg25ncu runs this after the guide and
[PR #102](https://github.com/bibi45c/gateflow/pull/102) are merged. This also checks
[#101](https://github.com/bibi45c/gateflow/issues/101): Ollama 0.40.3 can send SSE
with `Content-Type: application/x-ndjson`; without the fix, the gateway can buffer
tool-call replies and miss usage, charging the full reservation. The mock is not
affected. The
[15 October pre-trial](https://github.com/bibi45c/gateflow/issues/66#issuecomment-6187076630)
found this bug; it does not replace acceptance.

- Start Ollama 0.40.3 as in [Ollama setup](opencode.md#1-install-and-start-ollama),
  requesting `OLLAMA_CONTEXT_LENGTH=40960`, then pull and check `qwen3:8b` as in
  [the model check](opencode.md#2-pull-and-check-the-model). Capabilities must include
  `tools`. Record `ollama --version`, the model ID from `ollama list`, and
  `CONTEXT` and `PROCESSOR` from `ollama ps` while the model is loaded.
- Ensure the gateway container can reach the seeded `OLLAMA_BASE_URL` (default
  `http://host.docker.internal:11434`). If a loopback-only Ollama listener is not
  reachable from Docker, configure its listener for Docker access, restrict access
  to the local machine/network and restart it. For another address or model, set
  `OLLAMA_BASE_URL` and `OLLAMA_MODEL` in `deploy/compose/.env` and repeat the stack
  `up` command; `seed-access` updates the channel settings and must exit with 0.
- Change `model` to `qwen3:8b` and `model_context_window` to its actual loaded
  context. Keep the gateway URL and seeded key. Prepare a fresh broken task,
  record its UTC start and run the same Codex command and independent test as for
  the mock. Record elapsed time and whether the task completed, even if tests fail.

Pass requires at least one successful tool call whose result is sent in a later
model request, with both requests relayed and settled. Task completion is recorded
but is not required for this model. A command printed but rejected by the sandbox
does not count. No repeated trials or direct runs are required for #66.

### 5. Match every request to its settlement

Wait for Codex to finish. From the repository root, run these commands, replacing
the example timestamp in **both** commands with that run's UTC start (include `Z`
or `+00:00`). Do not use a fixed last-ten-rows query: it can miss requests or include
another run.

```text
docker compose -p codex66 -f deploy/compose/compose.yaml logs --no-color --since "2026-10-15T08:00:00Z" gateway
docker compose -p codex66 -f deploy/compose/compose.yaml exec -T postgres psql -U gateflow -d gateflow -c "SELECT r.call_id, r.attempt_id, r.status, l.outcome, l.input_tokens, l.output_tokens, l.amount, r.max_cost, count(l.ledger_entry_id) OVER (PARTITION BY r.call_id) AS ledger_rows FROM accounting.reservations r LEFT JOIN accounting.ledger_entries l USING (reservation_id) WHERE r.tenant_id = 'tenant-01' AND r.created_at >= '2026-10-15T08:00:00Z'::timestamptz ORDER BY r.created_at;"
```

Match every gateway `msg: request` line for `POST /v1/responses` and the expected
`model` to a ledger row: log `request_id` equals ledger `call_id`. See
[request logs](../observability.md#interim-logging-on-main). A successful request
has HTTP `status: 200`, `result: completed`, reservation `status = settled`,
`ledger_rows = 1` and `outcome = completed`. Include the last request carrying the
test result. A 200 header alone does not prove the stream completed. Explain every
retry, missing request, non-completed result or missing settlement before claiming
acceptance.

For Ollama's tool-call turns, `input_tokens` and `output_tokens` must be non-NULL.
At the default seeded prices, `amount` must be 3 × input + 15 × output. The
pre-trial expected tens of thousands of micro-units, rather than the full hold
near 640,000; actual cost depends on usage. Keep `max_cost` beside the amount so
fallback charging is visible. Save the query and all matching rows, not just totals.

### 6. Record acceptance and stop the stack

Post redacted results on [issue #66](https://github.com/bibi45c/gateflow/issues/66)
by 18 October 2026:

| Item | Evidence |
|---|---|
| Run identity | Mock or Ollama; runner; date and UTC start/end; whether the runner wrote the guide |
| Code and environment | Git SHA; OS; Python, Node, Codex and Docker versions; for Ollama, server version, model ID, GPU, context and processor split |
| Configuration | Provider, model, gateway URL and context; no key |
| Tool round trips | Tool names, successful results and later requests carrying them; mock: all three steps |
| Task result | Independent test result, final `calc.py`, duration; record an Ollama failure too |
| Routing and accounting | Gateway request lines, ledger query and complete matching rows; counts and matching IDs; Ollama tool-turn tokens, amounts and holds |
| Verdict | Pass, fail or blocked against the run's criteria; explain errors and retries |

The issue stays open until both post-merge runs are recorded; an author's dry run
does not satisfy the independent mock run. After saving evidence, stop only this
project. This removes its test data and, if using the optional Ollama compose
profile, its downloaded models:

```text
docker compose -p codex66 -f deploy/compose/compose.yaml down --volumes
```

## Troubleshooting

| Symptom | Likely cause | Try |
|---|---|---|
| Every command fails with `rejected: blocked by policy`, and Codex suggests running as administrator | The default Windows sandbox could not run the command | Set `[windows] sandbox = "unelevated"` |
| The first command is refused: "you cannot ask for escalated permissions if the approval policy is Never" | The model asked to run outside the sandbox (`sandbox_permissions: require_escalated`) | Nothing to fix; in the recorded run the model then tried again without it. Count the run as it ends |
| `Missing environment variable: GATEFLOW_API_KEY` | The key is not set in this terminal | Set `GATEFLOW_API_KEY` |
| `Reconnecting... waiting for network`, repeated | Nothing answers at `base_url` | Start Ollama or the stack; check the port in `base_url` |
| The header shows another `provider` | Codex did not read this `config.toml` | Set `CODEX_HOME` to the folder that holds it, in the same terminal |
| `Model metadata for qwen3:8b not found` | Codex has no built-in metadata for this model | Expected; keep `model_context_window` |
| Ollama tool-turn tokens are NULL or amount equals the full hold | The stream usage may have been missed (#101) | Check that #102 is included and retain log and ledger evidence |
| Mock claims success but the file or test did not change | Its closing text is scripted | Check all tool results and run the independent test |
| `codex exec` prints `Reading additional input from stdin...`, and may wait | Its input is not a terminal, for example in a script: Codex reads it to the end and adds it to the prompt | Start it from a terminal, or give it an empty file as input |
| 500 with `llama-server process no longer running` and `CUDA error` on the first request | Ollama failed to load the model the first time | Send the request again; it loaded on the second try on the test machine |
| The task reports that `python` was not found, or the Microsoft Store opens | Windows has only the Store alias; macOS has `python3` | Install Python 3.13 on Windows; write `python3` in the task on macOS |
| Codex reads the files and names the fix, but `calc.py` is unchanged at the end | The model ended without an edit command | A model limit; record the run and run the task again |
| More than 60 s to the first output | The model is loading, or runs partly on the CPU | See the OpenCode guide's [troubleshooting](opencode.md#troubleshooting) |

If the model is the problem, fall back as the OpenCode guide describes: a smaller
model, another model with tool calling, or a faster machine. Record on the issue
which layer failed and the error text.
