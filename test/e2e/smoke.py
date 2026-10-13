"""End-to-end smoke test of a running local compose stack.

Start the stack first (see deploy/compose/compose.yaml), then run from the repository
root:

    python test/e2e/smoke.py [--project <compose-project>]

It sends requests through the gateway on 127.0.0.1:$GATEWAY_PORT (default 8080), so
each passes gateway, access, metering and the mock upstream, and then reads the
metering ledger with psql inside the compose postgres container. The seeded keys are
derived from ACCESS_SEED_SECRET (default: the compose default) exactly as
`access seed` derives them. Each check prints one PASS or FAIL line; the exit code
is 0 only when every check passed. Only the standard library is used.
"""
import argparse
import base64
import hashlib
import hmac
import http.client
import json
import os
from pathlib import Path
import re
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


def derive_key(secret, tenant_id):
    """The seeded gateway key of tenant_id, as seed.DeriveKey computes it."""
    mac = hmac.new(secret, tenant_id.encode("utf-8"), hashlib.sha256).digest()
    return "gf_" + base64.urlsafe_b64encode(mac).decode("ascii").rstrip("=")


def valid_call_id(value):
    """Whether value is a request id the gateway issues, and so safe in SQL."""
    return isinstance(value, str) and CALL_ID.fullmatch(value) is not None


def ledger_sql(call_ids):
    """The query of the ledger entries of call_ids, one "call_id|outcome" row each."""
    for call_id in call_ids:
        if not valid_call_id(call_id):
            raise ValueError(f"not a gateway request id: {call_id!r}")
    listed = ", ".join(f"'{call_id}'" for call_id in call_ids)
    return ("SELECT call_id, outcome FROM accounting.ledger_entries "
            f"WHERE call_id IN ({listed}) ORDER BY call_id, outcome")


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


def psql_command(project, sql):
    return compose_command(project, "exec", "-T", "postgres",
                           "psql", "-U", "gateflow", "-d", "gateflow", "-tAc", sql)


def query_ledger(project, call_ids):
    """Returns the ledger entries of call_ids; raises RuntimeError when psql fails."""
    command = psql_command(project, ledger_sql(call_ids))
    try:
        result = subprocess.run(command, capture_output=True, text=True, encoding="utf-8",
                                errors="replace", timeout=HTTP_TIMEOUT, check=False)
    except (OSError, subprocess.TimeoutExpired) as error:
        raise RuntimeError(f"docker compose exec failed: {error}") from error
    if result.returncode != 0:
        raise RuntimeError(f"psql exited with {result.returncode}: {result.stderr.strip()}")
    return parse_ledger(result.stdout)


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


class Response:
    def __init__(self, status, request_id, content_type, body):
        self.status = status
        self.request_id = request_id
        self.content_type = content_type
        self.body = body


def headers_for(key):
    headers = {"content-type": "application/json", "anthropic-version": "2023-06-01"}
    if key is not None:
        headers["x-api-key"] = key
    return headers


def post(port, payload, key):
    """Sends payload to POST /v1/messages and reads the whole response."""
    conn = http.client.HTTPConnection(HOST, port, timeout=HTTP_TIMEOUT)
    try:
        conn.request("POST", "/v1/messages", body=json.dumps(payload).encode("utf-8"),
                     headers=headers_for(key))
        response = conn.getresponse()
        return Response(response.status, response.getheader("request-id"),
                        response.getheader("content-type", ""), response.read())
    finally:
        conn.close()


def post_oversized(port, key):
    """Sends a body over the gateway's limit, stopping once the gateway answers.

    The gateway refuses the body by its Content-Length without reading it, so sending
    all of it could end in a connection reset before the answer is read.
    """
    text = "x" * MAX_BODY_BYTES
    body = json.dumps({"model": "mock-text", "max_tokens": 16,
                       "messages": [{"role": "user", "content": text}]}).encode("utf-8")
    conn = http.client.HTTPConnection(HOST, port, timeout=HTTP_TIMEOUT)
    try:
        conn.putrequest("POST", "/v1/messages")
        for name, value in headers_for(key).items():
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


class Run:
    """Collects the check results and the calls to look up in the ledger."""

    def __init__(self):
        self.failed = 0
        self.charged = []  # (check, call id, expected outcome)
        self.refused = []  # (check, call id)

    def report(self, name, problem):
        if problem:
            self.failed += 1
            print(f"FAIL {name}: {problem}", flush=True)
        else:
            print(f"PASS {name}", flush=True)

    def call(self, name, send, status, check=None, outcome=None):
        """Sends one request and checks its status and request id, then its body.

        outcome is the ledger outcome a relayed call must get; None means the call is
        refused and must leave no ledger entry.
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
            else:
                self.refused.append((name, response.request_id))


def expect_error(error):
    def check(response):
        got = error_type(response.body)
        return None if got == error else f"error type {got!r}, want {error!r}"
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


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__.split("\n", 1)[0])
    parser.add_argument("--project", help="compose project name, passed to docker compose -p")
    args = parser.parse_args(argv)
    try:
        port = int(os.environ.get("GATEWAY_PORT") or 8080)
    except ValueError:
        print("FAIL setup: GATEWAY_PORT is not a number")
        return 1
    secret = (os.environ.get("ACCESS_SEED_SECRET") or DEFAULT_SEED_SECRET).encode("utf-8")

    run = Run()
    send_requests(run, port, secret)
    check_ledger(run, args.project)
    print(f"{run.failed} check(s) failed" if run.failed else "All checks passed")
    return 1 if run.failed else 0


if __name__ == "__main__":
    sys.exit(main())
