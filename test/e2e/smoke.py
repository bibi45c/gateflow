"""End-to-end smoke test of a running local compose stack.

Start the stack first (see deploy/compose/compose.yaml), then run from the repository
root:

    python test/e2e/smoke.py [--project <compose-project>]

It sends requests through the gateway on 127.0.0.1:$GATEWAY_PORT (default 8080), to
POST /v1/messages and then to POST /v1/responses, so each passes gateway, access,
metering and the mock upstream, and then reads the metering ledger and reservations
with psql inside the compose postgres container. The seeded keys are derived from
ACCESS_SEED_SECRET (default: the compose default) exactly as `access seed` derives
them. Each check prints one PASS or FAIL line; the exit code is 0 only when every
check passed. Only the standard library is used.

On Kubernetes, --kube-context and --namespace with --postgres-pod read the ledger
through psql in a PostgreSQL Pod as the gateflow superuser. With --db-secret instead,
the smoke test creates a short-lived query Pod on a services node whose DATABASE_URL
comes from that Secret (key METERING_DATABASE_URL by default), runs psql there with
the URL as it is, sslmode included, and deletes the Pod afterwards. The URL never
reaches this process.
"""
import argparse
import contextlib
from dataclasses import dataclass
import base64
import hashlib
import hmac
import http.client
import json
import os
from pathlib import Path
import re
import secrets
import select
import subprocess
import sys
import time

ROOT = Path(__file__).resolve().parents[2]
COMPOSE_FILE = ROOT / "deploy" / "compose" / "compose.yaml"
DEFAULT_SEED_SECRET = "seed-secret-local-only"
HOST = "127.0.0.1"
HTTP_TIMEOUT = 30  # seconds, for every request and for each psql call
LEDGER_WAIT = 5  # seconds to wait for the ledger entries of the relayed calls
MAX_BODY_BYTES = 2 << 20  # the gateway's request body limit
DEFAULT_DB_SECRET_KEY = "METERING_DATABASE_URL"
DEFAULT_QUERY_IMAGE = "postgres:17"
QUERY_POD_PREFIX = "gateflow-smoke-db"
QUERY_POD_LABELS = {"app.kubernetes.io/name": "gateflow-smoke", "app.kubernetes.io/component": "db-query"}
QUERY_POD_LIFETIME = 900  # seconds the query Pod sleeps before it ends by itself
QUERY_POD_READY_TIMEOUT = 180  # seconds to wait for the query Pod to become Ready
# The user and password of a database URL, and a password parameter, which no output may carry.
URL_CREDENTIALS = re.compile(r"(postgres(?:ql)?://)\S*@", re.IGNORECASE)
URL_PASSWORD = re.compile(r"(password=)[^&\s'\"]*", re.IGNORECASE)
MESSAGES_PATH = "/v1/messages"
RESPONSES_PATH = "/v1/responses"
# The output bound the gateway holds, and sends upstream, for a Responses request
# without max_output_tokens.
DEFAULT_OUTPUT_BOUND = 32768
CALL_ID = re.compile(r"req_[0-9a-f-]{36}")
# A model that matches no channel of the seeded keys: mock-* and the Ollama model.
UNSERVED_MODEL = "e2e-unserved-model"
TOOL = {
    "name": "get_weather",
    "description": "Get the current weather of a city.",
    "input_schema": {
        "type": "object",
        "properties": {"city": {"type": "string"}},
        "required": ["city"],
    },
}
# TOOL as a Responses function tool.
FUNCTION_TOOL = {"type": "function", "name": TOOL["name"], "description": TOOL["description"],
                 "parameters": TOOL["input_schema"]}
RESPONSES_TOOL_EVENTS = ("response.created, response.in_progress, response.output_item.added, "
                         "response.function_call_arguments.delta..., "
                         "response.function_call_arguments.done, response.output_item.done, "
                         "response.completed")


def derive_key(secret, tenant_id):
    """The seeded gateway key of tenant_id, as seed.DeriveKey computes it."""
    mac = hmac.new(secret, tenant_id.encode("utf-8"), hashlib.sha256).digest()
    return "gf_" + base64.urlsafe_b64encode(mac).decode("ascii").rstrip("=")


def valid_call_id(value):
    """Whether value is a request id the gateway issues, and so safe in SQL."""
    return isinstance(value, str) and CALL_ID.fullmatch(value) is not None


def quoted_ids(call_ids):
    """call_ids as a SQL list; raises ValueError for anything but a gateway request id."""
    for call_id in call_ids:
        if not valid_call_id(call_id):
            raise ValueError(f"not a gateway request id: {call_id!r}")
    return ", ".join(f"'{call_id}'" for call_id in call_ids)


def ledger_sql(call_ids):
    """The query of the ledger entries of call_ids, one "call_id|outcome" row each."""
    return ("SELECT call_id, outcome FROM accounting.ledger_entries "
            f"WHERE call_id IN ({quoted_ids(call_ids)}) ORDER BY call_id, outcome")


def holds_sql(call_ids):
    """The query of the reservations of call_ids, one row each: call_id, then
    max_input_tokens, max_output_tokens, input_price, output_price and max_cost."""
    return ("SELECT call_id, max_input_tokens, max_output_tokens, input_price, output_price, "
            f"max_cost FROM accounting.reservations WHERE call_id IN ({quoted_ids(call_ids)}) "
            "ORDER BY call_id")


def parse_holds(output):
    """Maps each call id in psql -tA output of holds_sql to its rows of five integers.

    Raises ValueError on a row that is not a call id and five integers.
    """
    holds = {}
    for line in output.splitlines():
        if not line.strip():
            continue
        call_id, *values = line.strip().split("|")
        if len(values) != 5:
            raise ValueError(f"reservation row {line.strip()!r} does not have six columns")
        holds.setdefault(call_id, []).append(tuple(int(value) for value in values))
    return holds


def hold_problem(rows, max_output_tokens):
    """Why rows are not one reservation that holds max_output_tokens and costs what its
    bounds and prices give, or None."""
    if len(rows) != 1:
        return f"{len(rows)} reservations, want one"
    max_input, max_output, input_price, output_price, max_cost = rows[0]
    if max_output != max_output_tokens:
        return f"max_output_tokens {max_output}, want {max_output_tokens}"
    want = max_input * input_price + max_output * output_price
    if max_cost != want:
        return (f"max_cost {max_cost}, want {max_input} x {input_price} + "
                f"{max_output} x {output_price} = {want}")
    return None


def parse_ledger(output):
    """Maps each call id in psql -tA output to the outcomes of its entries."""
    entries = {}
    for line in output.splitlines():
        if not line.strip():
            continue
        call_id, _, outcome = line.strip().partition("|")
        entries.setdefault(call_id, []).append(outcome)
    return entries


def compose_command(project, *args):
    command = ["docker", "compose"]
    if project:
        command += ["-p", project]
    return command + ["-f", str(COMPOSE_FILE), *args]


@dataclass(frozen=True)
class KubernetesDatabase:
    context: str
    namespace: str
    pod: str


@dataclass(frozen=True)
class QueryPod:
    """A short-lived Pod that runs psql with a database URL it reads from a Secret."""
    context: str
    namespace: str
    pod: str


def redact(text):
    """text without the user and password of any postgres:// or postgresql:// URL."""
    return URL_PASSWORD.sub(r"\1<redacted>", URL_CREDENTIALS.sub(r"\1<redacted>@", text))


def kubectl(database, *args):
    return ["kubectl", "--context", database.context, "--namespace", database.namespace, *args]


def query_pod_name():
    return f"{QUERY_POD_PREFIX}-{secrets.token_hex(4)}"


def query_pod_spec(name, secret, key=DEFAULT_DB_SECRET_KEY, image=DEFAULT_QUERY_IMAGE):
    """The query Pod: it only sleeps, on a services node, as a non-root user, with
    DATABASE_URL taken from key of secret by Kubernetes, never from this process."""
    container = {
        "name": "psql", "image": image, "command": ["sleep", str(QUERY_POD_LIFETIME)],
        "env": [{"name": "DATABASE_URL", "valueFrom": {"secretKeyRef": {"name": secret, "key": key}}},
                {"name": "HOME", "value": "/tmp"}],
        "securityContext": {"allowPrivilegeEscalation": False, "capabilities": {"drop": ["ALL"]}},
        "resources": {"requests": {"cpu": "10m", "memory": "32Mi"}, "limits": {"memory": "128Mi"}},
    }
    return {
        "apiVersion": "v1", "kind": "Pod",
        "metadata": {"name": name, "labels": dict(QUERY_POD_LABELS)},
        "spec": {
            "containers": [container], "restartPolicy": "Never",
            "automountServiceAccountToken": False, "terminationGracePeriodSeconds": 1,
            "nodeSelector": {"gateflow.io/workload": "services"},
            "securityContext": {"runAsNonRoot": True, "runAsUser": 65532, "runAsGroup": 65532,
                                "seccompProfile": {"type": "RuntimeDefault"}},
        },
    }


def run_kubectl(command, timeout=HTTP_TIMEOUT, **kwargs):
    """Runs a kubectl command; raises RuntimeError, with database URLs redacted, when it fails."""
    try:
        result = subprocess.run(command, capture_output=True, text=True, encoding="utf-8",
                                errors="replace", timeout=timeout, check=False, **kwargs)
    except (OSError, subprocess.TimeoutExpired) as error:
        raise RuntimeError(redact(f"kubectl {command[5]} failed: {error}")) from error
    if result.returncode != 0:
        raise RuntimeError(redact(f"kubectl {command[5]} exited with {result.returncode}: "
                                  f"{result.stderr.strip()}"))
    return result.stdout


def start_query_pod(database, secret, key=DEFAULT_DB_SECRET_KEY, image=DEFAULT_QUERY_IMAGE):
    """Creates the query Pod and waits until it is Ready; raises RuntimeError when it cannot."""
    spec = json.dumps(query_pod_spec(database.pod, secret, key, image))
    run_kubectl(kubectl(database, "create", "-f", "-"), input=spec)
    run_kubectl(kubectl(database, "wait", "--for=condition=Ready", f"pod/{database.pod}",
                        f"--timeout={QUERY_POD_READY_TIMEOUT}s"),
                timeout=QUERY_POD_READY_TIMEOUT + HTTP_TIMEOUT)


def delete_query_pod(database):
    """Deletes the query Pod without waiting; returns why it could not, or None."""
    try:
        run_kubectl(kubectl(database, "delete", "pod", database.pod, "--wait=false",
                            "--ignore-not-found"))
    except RuntimeError as error:
        return str(error)
    return None


@contextlib.contextmanager
def query_pod(context, namespace, secret, key=DEFAULT_DB_SECRET_KEY, image=DEFAULT_QUERY_IMAGE):
    """Yields a ready QueryPod and deletes it afterwards, however the block ends, even
    when the Pod never became ready."""
    database = QueryPod(context, namespace, query_pod_name())
    try:
        start_query_pod(database, secret, key, image)
        yield database
    finally:
        problem = delete_query_pod(database)
        if problem:
            print(f"WARNING could not delete query Pod {database.pod}: {problem}",
                  file=sys.stderr, flush=True)


def psql_command(project, sql):
    if isinstance(project, QueryPod):
        # The URL stays inside the Pod; the SQL is an argument, never part of the script.
        return kubectl(project, "exec", project.pod, "--", "sh", "-c",
                       'exec psql -X -tAc "$1" "$DATABASE_URL"', "smoke", sql)
    if isinstance(project, KubernetesDatabase):
        return ['kubectl', '--context', project.context, '--namespace', project.namespace,
                'exec', project.pod, '--', 'psql', '-U', 'gateflow', '-d', 'gateflow', '-tAc', sql]
    return compose_command(project, "exec", "-T", "postgres",
                           "psql", "-U", "gateflow", "-d", "gateflow", "-tAc", sql)


def run_psql(project, sql):
    """Returns the output of sql run by psql; raises RuntimeError when psql fails.
    Database URLs in the error are redacted."""
    command = psql_command(project, sql)
    try:
        result = subprocess.run(command, capture_output=True, text=True, encoding="utf-8",
                                errors="replace", timeout=HTTP_TIMEOUT, check=False)
    except (OSError, subprocess.TimeoutExpired) as error:
        raise RuntimeError(redact(f"database exec failed: {error}")) from error
    if result.returncode != 0:
        raise RuntimeError(redact(f"psql exited with {result.returncode}: {result.stderr.strip()}"))
    return result.stdout


def query_ledger(project, call_ids):
    """Returns the ledger entries of call_ids; raises RuntimeError when psql fails."""
    return parse_ledger(run_psql(project, ledger_sql(call_ids)))


def query_holds(project, call_ids):
    """Returns the reservations of call_ids; raises RuntimeError when psql fails or
    answers rows that are not a call id and five integers."""
    output = run_psql(project, holds_sql(call_ids))
    try:
        return parse_holds(output)
    except ValueError as error:
        raise RuntimeError(str(error)) from error


def parse_sse(body):
    """Splits a server-sent event stream into (event name, data) pairs."""
    events, name, data = [], None, []
    for line in body.decode("utf-8").replace("\r\n", "\n").replace("\r", "\n").split("\n"):
        if line == "":
            if name is not None or data:
                events.append((name or "message", "\n".join(data)))
            name, data = None, []
            continue
        if line.startswith(":"):
            continue
        field, _, value = line.partition(":")
        value = value[1:] if value.startswith(" ") else value
        if field == "event":
            name = value
        elif field == "data":
            data.append(value)
    if name is not None or data:
        events.append((name or "message", "\n".join(data)))
    return events


def tool_stream_problem(events):
    """Why events are not one tool_use reply streamed in order, or None."""
    names = [name for name, _ in events]
    expected = ("message_start, content_block_start, content_block_delta..., "
                "content_block_stop, message_delta, message_stop")
    deltas = names[2:-3]
    if (len(names) < 6 or names[:2] != ["message_start", "content_block_start"]
            or names[-3:] != ["content_block_stop", "message_delta", "message_stop"]
            or any(name != "content_block_delta" for name in deltas)):
        return f"events {names}, want {expected}"
    decoded = []
    for name, data in events:
        try:
            value = json.loads(data)
        except ValueError:
            return f"{name} data is not JSON"
        if not isinstance(value, dict) or value.get("type") != name:
            return f"{name} data has another type"
        decoded.append(value)
    block = decoded[1].get("content_block") or {}
    if block.get("type") != "tool_use":
        return f"content_block_start opens a {block.get('type')!r} block, want tool_use"
    stop_reason = (decoded[-2].get("delta") or {}).get("stop_reason")
    if stop_reason != "tool_use":
        return f"message_delta stop_reason is {stop_reason!r}, want tool_use"
    return None


def message_problem(body):
    """Why body is not a complete non-streaming text reply, or None."""
    try:
        message = json.loads(body)
    except ValueError:
        return "body is not JSON"
    if not isinstance(message, dict) or message.get("type") != "message":
        return "body is not a message"
    content = message.get("content")
    if (not isinstance(content, list) or not content or not isinstance(content[0], dict)
            or content[0].get("type") != "text" or not content[0].get("text")):
        return "message has no text content"
    if message.get("role") != "assistant" or message.get("stop_reason") != "end_turn":
        return "message is not a finished assistant reply"
    if not isinstance(message.get("usage"), dict):
        return "message has no usage"
    return None


def error_type(body):
    """The type of an Anthropic error body, or None."""
    try:
        value = json.loads(body)
    except ValueError:
        return None
    if not isinstance(value, dict) or not isinstance(value.get("error"), dict):
        return None
    return value["error"].get("type")


def as_object(value):
    """value when it is a JSON object, else an empty one, whose members are all absent."""
    return value if isinstance(value, dict) else {}


def responses_tool_stream_problem(events):
    """Why events are not one function_call response streamed in order and numbered
    from 0, or None."""
    names = [name for name, _ in events]
    head = ["response.created", "response.in_progress", "response.output_item.added"]
    tail = ["response.function_call_arguments.done", "response.output_item.done",
            "response.completed"]
    deltas = names[3:-3]
    if (len(names) < 7 or names[:3] != head or names[-3:] != tail
            or any(name != "response.function_call_arguments.delta" for name in deltas)):
        return f"events {names}, want {RESPONSES_TOOL_EVENTS}"
    decoded = []
    for name, data in events:
        try:
            value = json.loads(data)
        except ValueError:
            return f"{name} data is not JSON"
        if not isinstance(value, dict) or value.get("type") != name:
            return f"{name} data has another type"
        decoded.append(value)
    numbers = [value.get("sequence_number") for value in decoded]
    if any(type(number) is not int for number in numbers) or numbers != list(range(len(numbers))):
        return f"sequence numbers {numbers}, want 0 to {len(numbers) - 1}"
    item = as_object(decoded[2].get("item"))
    if item.get("type") != "function_call":
        return f"response.output_item.added adds a {item.get('type')!r} item, want function_call"
    response = as_object(decoded[-1].get("response"))
    if response.get("status") != "completed" or not isinstance(response.get("usage"), dict):
        return "response.completed holds no completed response with usage"
    return None


def response_problem(body):
    """Why body is not a completed non-streaming Responses text reply, or None."""
    try:
        response = json.loads(body)
    except ValueError:
        return "body is not JSON"
    if not isinstance(response, dict) or response.get("object") != "response":
        return "body is not a response"
    if response.get("status") != "completed":
        return f"response status is {response.get('status')!r}, want completed"
    output = response.get("output")
    item = as_object(output[0]) if isinstance(output, list) and output else {}
    content = item.get("content")
    part = as_object(content[0]) if isinstance(content, list) and content else {}
    if (item.get("type") != "message" or item.get("role") != "assistant"
            or part.get("type") != "output_text" or not part.get("text")):
        return "response has no assistant output_text"
    if not isinstance(response.get("usage"), dict):
        return "response has no usage"
    return None


def responses_error_problem(body, error, param=None):
    """Why body is not a Responses error body of type error whose param is param, or
    None. The Responses body has error alone at the top, with param and code."""
    try:
        value = json.loads(body)
    except ValueError:
        return "body is not JSON"
    if not isinstance(value, dict) or list(value) != ["error"] or not isinstance(value["error"], dict):
        return f"body {body[:300]!r} is not a Responses error body"
    detail = value["error"]
    if detail.get("type") != error:
        return f"error type {detail.get('type')!r}, want {error!r}"
    if not isinstance(detail.get("message"), str) or not detail["message"]:
        return "error has no message"
    for member in ("param", "code"):
        if member not in detail:
            return f"error has no {member} member"
    if detail["param"] != param:
        return f"error param {detail['param']!r}, want {param!r}"
    return None


class Response:
    def __init__(self, status, request_id, content_type, body):
        self.status = status
        self.request_id = request_id
        self.content_type = content_type
        self.body = body


def headers_for(key, path=MESSAGES_PATH):
    """The headers of a request to path. Messages carries the key in x-api-key, and
    Responses as a bearer token, as Codex sends it."""
    if path == RESPONSES_PATH:
        headers = {"content-type": "application/json"}
        if key is not None:
            headers["authorization"] = f"Bearer {key}"
        return headers
    headers = {"content-type": "application/json", "anthropic-version": "2023-06-01"}
    if key is not None:
        headers["x-api-key"] = key
    return headers


def post(port, payload, key, path=MESSAGES_PATH):
    """Sends payload to POST path and reads the whole response."""
    conn = http.client.HTTPConnection(HOST, port, timeout=HTTP_TIMEOUT)
    try:
        conn.request("POST", path, body=json.dumps(payload).encode("utf-8"),
                     headers=headers_for(key, path))
        response = conn.getresponse()
        return Response(response.status, response.getheader("request-id"),
                        response.getheader("content-type", ""), response.read())
    finally:
        conn.close()


def oversized_body(path):
    """A request body for path that is over the gateway's limit."""
    text = "x" * MAX_BODY_BYTES
    if path == RESPONSES_PATH:
        payload = {"model": "mock-text", "input": text}
    else:
        payload = {"model": "mock-text", "max_tokens": 16,
                   "messages": [{"role": "user", "content": text}]}
    return json.dumps(payload).encode("utf-8")


def post_oversized(port, key, path=MESSAGES_PATH):
    """Sends a body over the gateway's limit to path, stopping once the gateway answers.

    The gateway refuses the body by its Content-Length without reading it, so sending
    all of it could end in a connection reset before the answer is read.
    """
    body = oversized_body(path)
    conn = http.client.HTTPConnection(HOST, port, timeout=HTTP_TIMEOUT)
    try:
        conn.putrequest("POST", path)
        for name, value in headers_for(key, path).items():
            conn.putheader(name, value)
        conn.putheader("content-length", str(len(body)))
        conn.endheaders()
        chunk = 64 << 10
        for start in range(0, len(body), chunk):
            readable, _, _ = select.select([conn.sock], [], [], 0)
            if readable:
                break
            try:
                conn.send(body[start:start + chunk])
            except OSError:
                break
        response = conn.getresponse()
        return Response(response.status, response.getheader("request-id"),
                        response.getheader("content-type", ""), response.read())
    finally:
        conn.close()


def text_request(model, **extra):
    payload = {"model": model, "max_tokens": 64,
               "messages": [{"role": "user", "content": "Say hello."}]}
    payload.update(extra)
    return payload


def responses_request(model, **extra):
    payload = {"model": model, "max_output_tokens": 64, "input": "Say hello."}
    payload.update(extra)
    return payload


class Run:
    """Collects the check results and the calls to look up in the ledger."""

    def __init__(self):
        self.failed = 0
        self.charged = []  # (check, call id, expected outcome)
        self.refused = []  # (check, call id)
        self.holds = []  # (check, call id, max_output_tokens its reservation must hold)

    def report(self, name, problem):
        if problem:
            self.failed += 1
            print(f"FAIL {name}: {problem}", flush=True)
        else:
            print(f"PASS {name}", flush=True)

    def call(self, name, send, status, check=None, outcome=None, hold=None):
        """Sends one request and checks its status and request id, then its body.

        outcome is the ledger outcome a relayed call must get; None means the call is
        refused and must leave no ledger entry. hold is the max_output_tokens that the
        reservation of a relayed call must hold; None leaves its reservation unchecked.
        """
        try:
            response = send()
        except (OSError, http.client.HTTPException) as error:
            self.report(name, f"request failed: {error!r}")
            return
        problem = None
        if response.status != status:
            problem = f"status {response.status}, want {status}; body {response.body[:300]!r}"
        elif not valid_call_id(response.request_id):
            problem = f"request-id header {response.request_id!r} is not req_<uuid>"
        elif check:
            problem = check(response)
        self.report(name, problem)
        if valid_call_id(response.request_id):
            if outcome:
                self.charged.append((name, response.request_id, outcome))
                if hold is not None:
                    self.holds.append((name, response.request_id, hold))
            else:
                self.refused.append((name, response.request_id))


def expect_error(error):
    def check(response):
        got = error_type(response.body)
        return None if got == error else f"error type {got!r}, want {error!r}"
    return check


def expect_responses_error(error, param=None):
    def check(response):
        return responses_error_problem(response.body, error, param)
    return check


def check_tool_stream(response):
    if not response.content_type.startswith("text/event-stream"):
        return f"content-type {response.content_type!r}, want text/event-stream"
    try:
        return tool_stream_problem(parse_sse(response.body))
    except UnicodeDecodeError:
        return "stream is not UTF-8"


def check_message(response):
    return message_problem(response.body)


def check_responses_tool_stream(response):
    if not response.content_type.startswith("text/event-stream"):
        return f"content-type {response.content_type!r}, want text/event-stream"
    try:
        return responses_tool_stream_problem(parse_sse(response.body))
    except UnicodeDecodeError:
        return "stream is not UTF-8"


def check_response(response):
    return response_problem(response.body)


def send_requests(run, port, secret):
    """Sends checks a to g, using tenant-01 unless a check needs another key."""
    key = derive_key(secret, "tenant-01")

    def send(payload, gateway_key=key):
        return lambda: post(port, payload, gateway_key)

    tool_call = {"model": "mock-tool", "max_tokens": 256, "stream": True, "tools": [TOOL],
                 "messages": [{"role": "user", "content": "What is the weather in Singapore?"}]}
    hello = text_request("mock-text")
    no_max_tokens = {name: value for name, value in hello.items() if name != "max_tokens"}

    run.call("a. mock-tool stream relays a tool_use reply", send(tool_call),
             200, check_tool_stream, "completed")
    run.call("b. mock-text returns a complete message", send(hello),
             200, check_message, "completed")
    run.call("c. wrong key is refused", send(hello, "gf_not-a-seeded-key"),
             401, expect_error("authentication_error"))
    run.call("c. missing key is refused", send(hello, None),
             401, expect_error("authentication_error"))
    run.call("d. model without a channel is refused", send(text_request(UNSERVED_MODEL)),
             403, expect_error("permission_error"))
    run.call("e. tenant-00 without balance is refused", send(hello, derive_key(secret, "tenant-00")),
             402, expect_error("billing_error"))
    run.call("f. missing max_tokens is refused", send(no_max_tokens),
             400, expect_error("invalid_request_error"))
    run.call("f. body over 2 MiB is refused", lambda: post_oversized(port, key),
             413, expect_error("request_too_large"))
    run.call("g. upstream 500 becomes 502", send(text_request("mock-500")),
             502, expect_error("api_error"), "upstream_error")
    run.call("g. upstream 429 passes through", send(text_request("mock-429")),
             429, expect_error("rate_limit_error"), "upstream_error")


def send_responses_requests(run, port, secret):
    """Sends checks a to g again to POST /v1/responses, with the key as a bearer token,
    using tenant-01 unless a check needs another key. The tool call sends no
    max_output_tokens, as Codex does, so its reservation must hold DEFAULT_OUTPUT_BOUND.
    """
    key = derive_key(secret, "tenant-01")

    def send(payload, gateway_key=key):
        return lambda: post(port, payload, gateway_key, RESPONSES_PATH)

    tool_call = {"model": "mock-tool", "stream": True, "store": False, "tools": [FUNCTION_TOOL],
                 "input": [{"role": "user", "content": "What is the weather in Singapore?"}]}
    hello = responses_request("mock-text")

    run.call("a. responses: mock-tool stream relays a function_call reply", send(tool_call),
             200, check_responses_tool_stream, "completed", hold=DEFAULT_OUTPUT_BOUND)
    run.call("b. responses: mock-text returns a complete response", send(hello),
             200, check_response, "completed", hold=hello["max_output_tokens"])
    run.call("c. responses: wrong key is refused", send(hello, "gf_not-a-seeded-key"),
             401, expect_responses_error("authentication_error"))
    run.call("c. responses: missing key is refused", send(hello, None),
             401, expect_responses_error("authentication_error"))
    run.call("d. responses: model without a channel is refused",
             send(responses_request(UNSERVED_MODEL)), 403, expect_responses_error("permission_error"))
    run.call("e. responses: tenant-00 without balance is refused",
             send(hello, derive_key(secret, "tenant-00")), 402, expect_responses_error("billing_error"))
    run.call("f. responses: max_output_tokens 0 is refused",
             send(responses_request("mock-text", max_output_tokens=0)),
             400, expect_responses_error("invalid_request_error"))
    run.call("f. responses: max_output_tokens over 32768 is refused",
             send(responses_request("mock-text", max_output_tokens=DEFAULT_OUTPUT_BOUND + 1)),
             400, expect_responses_error("invalid_request_error"))
    run.call("f. responses: previous_response_id is refused",
             send(responses_request("mock-text", previous_response_id="resp_e2e")),
             400, expect_responses_error("invalid_request_error", "previous_response_id"))
    run.call("f. responses: body over 2 MiB is refused",
             lambda: post_oversized(port, key, RESPONSES_PATH),
             413, expect_responses_error("request_too_large"))
    run.call("g. responses: upstream 500 becomes 502", send(responses_request("mock-500")),
             502, expect_responses_error("api_error"), "upstream_error")
    run.call("g. responses: upstream 429 passes through", send(responses_request("mock-429")),
             429, expect_responses_error("rate_limit_error"), "upstream_error")


def check_ledger(run, project):
    """Waits for one entry per relayed call, then checks the refused calls have none."""
    charged = {call_id: (name, outcome) for name, call_id, outcome in run.charged}
    entries, error = {}, None
    deadline = time.monotonic() + LEDGER_WAIT
    while charged:
        try:
            entries, error = query_ledger(project, list(charged)), None
        except RuntimeError as failure:
            error = str(failure)
        if not error and all(call_id in entries for call_id in charged):
            break
        if time.monotonic() >= deadline:
            break
        time.sleep(0.25)
    for call_id, (name, outcome) in charged.items():
        found = entries.get(call_id, [])
        problem = error
        if not problem and found != [outcome]:
            problem = f"ledger entries of {call_id} are {found}, want one {outcome!r}"
        run.report(f"h. ledger: {name}", problem)

    if not run.refused:
        return
    error = None
    try:
        entries = query_ledger(project, [call_id for _, call_id in run.refused])
    except RuntimeError as failure:
        error = str(failure)
    for name, call_id in run.refused:
        found = entries.get(call_id, [])
        problem = error or (f"ledger has {len(found)} entries for {call_id}, want none" if found else None)
        run.report(f"h. ledger: {name}", problem)


def check_holds(run, project):
    """Checks that the reservation of each call with a hold held its output bound, and
    was costed at it."""
    if not run.holds:
        return
    rows, error = {}, None
    try:
        rows = query_holds(project, [call_id for _, call_id, _ in run.holds])
    except RuntimeError as failure:
        error = str(failure)
    for name, call_id, max_output_tokens in run.holds:
        problem = error or hold_problem(rows.get(call_id, []), max_output_tokens)
        run.report(f"h. reservation: {name}", problem)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__.split("\n", 1)[0])
    parser.add_argument("--project", help="compose project name, passed to docker compose -p")
    parser.add_argument('--kube-context', help='explicit context for local Kubernetes database checks')
    parser.add_argument('--namespace', help='Kubernetes namespace')
    parser.add_argument('--postgres-pod', help='local PostgreSQL fixture Pod')
    parser.add_argument('--db-secret', help='Secret whose database URL a short-lived query Pod uses')
    parser.add_argument('--db-secret-key', help=f'key of the URL in --db-secret (default: {DEFAULT_DB_SECRET_KEY})')
    parser.add_argument('--query-image', help=f'image of the query Pod (default: {DEFAULT_QUERY_IMAGE})')
    parser.add_argument('--host', default='127.0.0.1', help='ingress host (default: localhost)')
    parser.add_argument('--port', type=int, help='ingress port (default: GATEWAY_PORT or 8080)')
    args = parser.parse_args(argv)
    if (args.db_secret_key is not None or args.query_image is not None) and not args.db_secret:
        parser.error('--db-secret-key and --query-image require --db-secret')
    if args.db_secret is not None:
        if args.postgres_pod or args.project:
            parser.error('--db-secret excludes --postgres-pod and --project')
        if not (args.db_secret and args.kube_context and args.namespace):
            parser.error('--db-secret requires --kube-context and --namespace')
    else:
        kube_options = (args.kube_context, args.namespace, args.postgres_pod)
        if any(kube_options) and (not all(kube_options) or args.project):
            parser.error('Kubernetes requires --kube-context, --namespace and --postgres-pod, without --project')
    global HOST
    HOST = args.host
    try:
        port = args.port if args.port is not None else int(os.environ.get("GATEWAY_PORT") or 8080)
    except ValueError:
        print("FAIL setup: GATEWAY_PORT is not a number")
        return 1
    secret = (os.environ.get("ACCESS_SEED_SECRET") or DEFAULT_SEED_SECRET).encode("utf-8")

    with contextlib.ExitStack() as stack:
        if args.db_secret:
            try:
                database = stack.enter_context(query_pod(
                    args.kube_context, args.namespace, args.db_secret,
                    args.db_secret_key or DEFAULT_DB_SECRET_KEY, args.query_image or DEFAULT_QUERY_IMAGE))
            except RuntimeError as error:
                print(f"FAIL setup: query Pod: {error}", flush=True)
                return 1
        elif args.postgres_pod:
            database = KubernetesDatabase(args.kube_context, args.namespace, args.postgres_pod)
        else:
            database = args.project
        return run_checks(port, secret, database)


def run_checks(port, secret, database):
    """Sends every request, checks the ledger and reservations in database and
    returns the exit code."""
    run = Run()
    send_requests(run, port, secret)
    send_responses_requests(run, port, secret)
    check_ledger(run, database)
    check_holds(run, database)
    print(f"{run.failed} check(s) failed" if run.failed else "All checks passed")
    return 1 if run.failed else 0


if __name__ == "__main__":
    sys.exit(main())
