"""Unit tests of the smoke test's helpers. They need no network and no Docker."""
import contextlib
import io
import json
import re
import unittest
from unittest import mock

import smoke

CALL_ID = "req_3f1c2a9e-7b4d-4e8a-9c61-2d5b8f0a7e34"
OTHER_ID = "req_00000000-0000-4000-8000-000000000000"


def sse(*events, newline="\n"):
    """A stream of (name, data) events, each data encoded as JSON."""
    return "".join(f"event: {name}{newline}data: {json.dumps(data)}{newline}{newline}"
                   for name, data in events).encode("utf-8")


def tool_stream(stop_reason="tool_use", block="tool_use", deltas=2):
    events = [("message_start", {"type": "message_start", "message": {"id": "msg_1"}}),
              ("content_block_start", {"type": "content_block_start", "index": 0,
                                       "content_block": {"type": block, "name": "get_weather"}})]
    events += [("content_block_delta", {"type": "content_block_delta", "index": 0,
                                        "delta": {"type": "input_json_delta", "partial_json": "{}"}})
               for _ in range(deltas)]
    events += [("content_block_stop", {"type": "content_block_stop", "index": 0}),
               ("message_delta", {"type": "message_delta", "delta": {"stop_reason": stop_reason}}),
               ("message_stop", {"type": "message_stop"})]
    return events


def responses_tool_stream(item="function_call", deltas=2, status="completed"):
    """The events of a Responses stream that calls one function, numbered from 0, as
    the mock streams them."""
    names = (["response.created", "response.in_progress", "response.output_item.added"]
             + ["response.function_call_arguments.delta"] * deltas
             + ["response.function_call_arguments.done", "response.output_item.done",
                "response.completed"])
    events = []
    for number, name in enumerate(names):
        data = {"type": name, "sequence_number": number}
        if name in ("response.created", "response.in_progress"):
            data["response"] = {"id": "resp_1", "status": "in_progress", "usage": None}
        elif name == "response.completed":
            data["response"] = {"id": "resp_1", "status": status,
                                "usage": {"input_tokens": 9, "output_tokens": 2}}
        elif name in ("response.output_item.added", "response.output_item.done"):
            data["item"] = {"type": item, "name": "get_weather"}
        else:
            data["delta" if name.endswith(".delta") else "arguments"] = "{}"
        events.append((name, data))
    return events


def response_body(**changes):
    """A completed non-streaming Responses text reply, as the mock answers mock-text."""
    body = {"id": "resp_mock_text_0", "object": "response", "status": "completed",
            "model": "mock-text",
            "output": [{"id": "msg_mock_text_0", "type": "message", "status": "completed",
                        "role": "assistant",
                        "content": [{"type": "output_text", "text": "Hello", "annotations": []}]}],
            "usage": {"input_tokens": 3, "output_tokens": 1, "total_tokens": 4}}
    body.update(changes)
    return json.dumps(body).encode("utf-8")


def responses_error(error_type, param=None, **changes):
    """A Responses error body, as the gateway writes it."""
    detail = {"message": "A fixed message.", "type": error_type, "param": param, "code": None}
    detail.update(changes)
    return json.dumps({"error": detail}).encode("utf-8")


class DeriveKeyTests(unittest.TestCase):
    def test_matches_seed_derive_key(self):
        # Computed with seed.DeriveKey in services/access/internal/seed.
        self.assertEqual("gf_HC4CiILt1WEq1MrXA9gieqvoIeqUOZj3WsGy4Vrb1MY",
                         smoke.derive_key(b"seed-secret-local-only", "tenant-01"))
        self.assertEqual("gf_KgDZba9cbVnoVFlOnUFUqwTqeVXO8nVp8h45oCZoUd4",
                         smoke.derive_key(b"test-seed-secret", "tenant-00"))

    def test_is_gf_and_43_base64url_characters(self):
        key = smoke.derive_key(b"any-secret", "tenant-02")
        self.assertRegex(key, r"^gf_[A-Za-z0-9_-]{43}$")
        self.assertNotEqual(key, smoke.derive_key(b"any-secret", "tenant-03"))


class ParseSSETests(unittest.TestCase):
    def test_reads_names_and_data_in_order(self):
        events = smoke.parse_sse(sse(*tool_stream()))
        self.assertEqual(["message_start", "content_block_start", "content_block_delta",
                          "content_block_delta", "content_block_stop", "message_delta",
                          "message_stop"], [name for name, _ in events])
        self.assertEqual({"type": "message_stop"}, json.loads(events[-1][1]))

    def test_crlf_comments_multiline_data_and_unterminated_event(self):
        body = (b": keep-alive\r\nevent: a\r\ndata: one\r\ndata:two\r\n\r\n"
                b"data: plain\n\nevent: b\ndata: last")
        self.assertEqual([("a", "one\ntwo"), ("message", "plain"), ("b", "last")],
                         smoke.parse_sse(body))

    def test_empty_body_has_no_events(self):
        self.assertEqual([], smoke.parse_sse(b""))
        self.assertEqual([], smoke.parse_sse(b"\n\n: comment\n\n"))


class ToolStreamTests(unittest.TestCase):
    def problem(self, events):
        return smoke.tool_stream_problem(smoke.parse_sse(sse(*events)))

    def test_accepts_a_tool_use_stream(self):
        self.assertIsNone(self.problem(tool_stream()))
        self.assertIsNone(self.problem(tool_stream(deltas=1)))

    def test_refuses_a_stream_without_deltas(self):
        self.assertIn("events", self.problem(tool_stream(deltas=0)))

    def test_refuses_missing_or_extra_events(self):
        events = tool_stream()
        self.assertIsNotNone(self.problem(events[:-1]))
        self.assertIsNotNone(self.problem(events[1:]))
        self.assertIsNotNone(self.problem(events + [("ping", {"type": "ping"})]))
        self.assertIsNotNone(self.problem(events[:3] + [events[0]] + events[3:]))

    def test_refuses_a_text_block_or_another_stop_reason(self):
        self.assertIn("tool_use", self.problem(tool_stream(block="text")))
        self.assertIn("stop_reason", self.problem(tool_stream(stop_reason="end_turn")))

    def test_refuses_data_that_is_not_json_or_of_another_type(self):
        events = tool_stream()
        events[2] = ("content_block_delta", {"type": "message_delta"})
        self.assertIn("another type", self.problem(events))
        body = sse(*tool_stream()).replace(b'data: {"type": "message_stop"}', b"data: {")
        self.assertIn("not JSON", smoke.tool_stream_problem(smoke.parse_sse(body)))


class CallIdTests(unittest.TestCase):
    def test_accepts_gateway_request_ids(self):
        self.assertTrue(smoke.valid_call_id(CALL_ID))

    def test_refuses_anything_else(self):
        for value in [None, "", CALL_ID + "\n", CALL_ID.upper(), CALL_ID[:-1], CALL_ID + "0",
                      "req_3f1c2a9e-7b4d-4e8a-9c61-2d5b8f0a7e3'", "req_' OR '1'='1",
                      "led_3f1c2a9e-7b4d-4e8a-9c61-2d5b8f0a7e34", " " + CALL_ID]:
            with self.subTest(value=value):
                self.assertFalse(smoke.valid_call_id(value))

    def test_ledger_sql_quotes_valid_ids_and_refuses_others(self):
        other = "req_00000000-0000-4000-8000-000000000000"
        sql = smoke.ledger_sql([CALL_ID, other])
        self.assertIn(f"IN ('{CALL_ID}', '{other}')", sql)
        self.assertIn("accounting.ledger_entries", sql)
        with self.assertRaises(ValueError):
            smoke.ledger_sql([CALL_ID, "req_'; DROP TABLE accounting.wallets; --"])


class LedgerAndCommandTests(unittest.TestCase):
    def test_parse_ledger_groups_outcomes_by_call(self):
        other = "req_00000000-0000-4000-8000-000000000000"
        output = f"{CALL_ID}|completed\n{other}|upstream_error\n{other}|completed\n\n"
        self.assertEqual({CALL_ID: ["completed"], other: ["upstream_error", "completed"]},
                         smoke.parse_ledger(output))
        self.assertEqual({}, smoke.parse_ledger(""))

    def test_psql_command_is_an_argument_list(self):
        command = smoke.psql_command(None, "SELECT 1")
        self.assertEqual(["docker", "compose", "-f"], command[:3])
        self.assertTrue(re.search(r"deploy[\\/]compose[\\/]compose\.yaml$", command[3]))
        self.assertEqual(["exec", "-T", "postgres", "psql", "-U", "gateflow", "-d", "gateflow",
                          "-tAc", "SELECT 1"], command[4:])
        self.assertEqual(["docker", "compose", "-p", "wt-1", "-f"],
                         smoke.psql_command("wt-1", "SELECT 1")[:5])


class MessageTests(unittest.TestCase):
    def test_message_and_error_bodies(self):
        message = {"id": "msg_1", "type": "message", "role": "assistant", "model": "mock-text",
                   "content": [{"type": "text", "text": "Hello"}], "stop_reason": "end_turn",
                   "usage": {"input_tokens": 3, "output_tokens": 1}}
        self.assertIsNone(smoke.message_problem(json.dumps(message).encode()))
        self.assertIsNotNone(smoke.message_problem(json.dumps({**message, "content": []}).encode()))
        self.assertIsNotNone(smoke.message_problem(b"not json"))
        error = {"type": "error", "error": {"type": "billing_error", "message": "No."}}
        self.assertEqual("billing_error", smoke.error_type(json.dumps(error).encode()))
        self.assertIsNone(smoke.error_type(b"[]"))


class ResponsesToolStreamTests(unittest.TestCase):
    def problem(self, events):
        return smoke.responses_tool_stream_problem(smoke.parse_sse(sse(*events)))

    def test_accepts_a_function_call_stream(self):
        self.assertIsNone(self.problem(responses_tool_stream()))
        self.assertIsNone(self.problem(responses_tool_stream(deltas=1)))
        body = sse(*responses_tool_stream(), newline="\r\n")
        self.assertIsNone(smoke.responses_tool_stream_problem(smoke.parse_sse(body)))

    def test_refuses_a_stream_without_deltas(self):
        self.assertIn("events", self.problem(responses_tool_stream(deltas=0)))

    def test_refuses_missing_extra_or_reordered_events(self):
        events = responses_tool_stream()
        self.assertIsNotNone(self.problem(events[:-1]))
        self.assertIsNotNone(self.problem(events[1:]))
        self.assertIsNotNone(self.problem(events + [("error", {"type": "error", "sequence_number": 8})]))
        self.assertIsNotNone(self.problem([events[1], events[0]] + events[2:]))
        self.assertIsNotNone(self.problem(events[:3] + [events[-1]] + events[3:]))

    def test_refuses_another_item_or_an_unfinished_response(self):
        self.assertIn("function_call", self.problem(responses_tool_stream(item="message")))
        self.assertIn("completed", self.problem(responses_tool_stream(status="incomplete")))
        events = responses_tool_stream()
        events[-1][1]["response"] = {"id": "resp_1", "status": "completed"}
        self.assertIn("usage", self.problem(events))

    def test_refuses_sequence_numbers_that_skip_or_are_not_integers(self):
        events = responses_tool_stream()
        events[4][1]["sequence_number"] = 7
        self.assertIn("sequence numbers", self.problem(events))
        events = responses_tool_stream()
        events[1][1]["sequence_number"] = True
        self.assertIn("sequence numbers", self.problem(events))
        events = responses_tool_stream()
        del events[0][1]["sequence_number"]
        self.assertIn("sequence numbers", self.problem(events))

    def test_refuses_data_that_is_not_json_or_of_another_type(self):
        events = responses_tool_stream()
        events[3] = ("response.function_call_arguments.delta", {"type": "response.output_text.delta",
                                                                  "sequence_number": 3})
        self.assertIn("another type", self.problem(events))
        body = sse(*responses_tool_stream()).replace(b'data: {"type": "response.completed"', b"data: {")
        self.assertIn("not JSON", smoke.responses_tool_stream_problem(smoke.parse_sse(body)))


class ResponseTests(unittest.TestCase):
    def test_accepts_a_completed_text_response(self):
        self.assertIsNone(smoke.response_problem(response_body()))

    def test_refuses_a_response_that_did_not_complete(self):
        for status in ["incomplete", "failed", "in_progress", None]:
            with self.subTest(status=status):
                self.assertIn("status", smoke.response_problem(response_body(status=status)))

    def test_refuses_a_response_without_text_or_usage(self):
        tool = {"type": "function_call", "name": "get_weather", "arguments": "{}"}
        empty = {"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": ""}]}
        for output in [[], None, [tool], [empty], ["text"], [{"type": "message", "content": "Hello"}]]:
            with self.subTest(output=output):
                self.assertIn("output_text", smoke.response_problem(response_body(output=output)))
        self.assertIn("usage", smoke.response_problem(response_body(usage=None)))

    def test_refuses_other_bodies(self):
        message = {"type": "message", "role": "assistant", "content": [{"type": "text", "text": "Hi"}],
                   "stop_reason": "end_turn", "usage": {}}
        self.assertIsNotNone(smoke.response_problem(json.dumps(message).encode()))
        self.assertIn("not JSON", smoke.response_problem(b"not json"))
        self.assertIn("not JSON", smoke.response_problem(b"\xff"))
        self.assertIsNotNone(smoke.response_problem(b"[]"))


class ResponsesErrorTests(unittest.TestCase):
    def test_accepts_the_error_of_the_type_and_param(self):
        self.assertIsNone(smoke.responses_error_problem(responses_error("api_error"), "api_error"))
        body = responses_error("invalid_request_error", "previous_response_id")
        self.assertIsNone(smoke.responses_error_problem(body, "invalid_request_error", "previous_response_id"))

    def test_refuses_another_type_or_param(self):
        self.assertIn("type", smoke.responses_error_problem(responses_error("api_error"), "billing_error"))
        body = responses_error("invalid_request_error")
        self.assertIn("param", smoke.responses_error_problem(body, "invalid_request_error", "previous_response_id"))
        body = responses_error("invalid_request_error", "previous_response_id")
        self.assertIn("param", smoke.responses_error_problem(body, "invalid_request_error"))

    def test_refuses_an_anthropic_error_body(self):
        anthropic = {"type": "error", "error": {"type": "api_error", "message": "Failed."}}
        problem = smoke.responses_error_problem(json.dumps(anthropic).encode(), "api_error")
        self.assertIn("not a Responses error body", problem)

    def test_refuses_an_error_without_message_param_or_code(self):
        self.assertIn("message", smoke.responses_error_problem(responses_error("api_error", message=""), "api_error"))
        detail = {"message": "Failed.", "type": "api_error", "code": None}
        self.assertIn("param", smoke.responses_error_problem(json.dumps({"error": detail}).encode(), "api_error"))
        detail = {"message": "Failed.", "type": "api_error", "param": None}
        self.assertIn("code", smoke.responses_error_problem(json.dumps({"error": detail}).encode(), "api_error"))
        self.assertIn("not JSON", smoke.responses_error_problem(b"<html>", "api_error"))


class RequestShapeTests(unittest.TestCase):
    def test_responses_requests_carry_the_key_as_a_bearer_token(self):
        self.assertEqual({"content-type": "application/json", "authorization": "Bearer gf_k"},
                         smoke.headers_for("gf_k", smoke.RESPONSES_PATH))
        self.assertEqual({"content-type": "application/json"}, smoke.headers_for(None, smoke.RESPONSES_PATH))

    def test_messages_requests_keep_their_headers(self):
        self.assertEqual({"content-type": "application/json", "anthropic-version": "2023-06-01",
                          "x-api-key": "gf_k"}, smoke.headers_for("gf_k"))
        self.assertEqual(smoke.headers_for("gf_k"), smoke.headers_for("gf_k", smoke.MESSAGES_PATH))
        self.assertNotIn("x-api-key", smoke.headers_for(None))

    def test_oversized_bodies_are_over_the_limit_in_the_shape_of_their_endpoint(self):
        responses = smoke.oversized_body(smoke.RESPONSES_PATH)
        messages = smoke.oversized_body(smoke.MESSAGES_PATH)
        self.assertGreater(len(responses), smoke.MAX_BODY_BYTES)
        self.assertGreater(len(messages), smoke.MAX_BODY_BYTES)
        self.assertEqual(["model", "input"], list(json.loads(responses)))
        self.assertEqual(["model", "max_tokens", "messages"], list(json.loads(messages)))

    def test_responses_request_holds_64_output_tokens_unless_told_otherwise(self):
        self.assertEqual({"model": "mock-text", "max_output_tokens": 64, "input": "Say hello."},
                         smoke.responses_request("mock-text"))
        self.assertEqual(0, smoke.responses_request("mock-text", max_output_tokens=0)["max_output_tokens"])
        self.assertEqual("resp_1", smoke.responses_request("m", previous_response_id="resp_1")["previous_response_id"])

    def test_the_function_tool_is_the_messages_tool(self):
        tool = smoke.FUNCTION_TOOL
        self.assertEqual(("function", smoke.TOOL["name"]), (tool["type"], tool["name"]))
        self.assertEqual(smoke.TOOL["input_schema"], tool["parameters"])


class HoldTests(unittest.TestCase):
    def test_holds_sql_quotes_valid_ids_and_refuses_others(self):
        sql = smoke.holds_sql([CALL_ID, OTHER_ID])
        self.assertIn(f"IN ('{CALL_ID}', '{OTHER_ID}')", sql)
        self.assertIn("accounting.reservations", sql)
        self.assertIn("max_output_tokens", sql)
        with self.assertRaises(ValueError):
            smoke.holds_sql([CALL_ID, "req_'; DROP TABLE accounting.reservations; --"])

    def test_parse_holds_reads_rows_of_five_integers(self):
        output = f"{CALL_ID}|1000|32768|3|15|494520\n{OTHER_ID}|200|64|3|15|1560\n\n"
        self.assertEqual({CALL_ID: [(1000, 32768, 3, 15, 494520)], OTHER_ID: [(200, 64, 3, 15, 1560)]},
                         smoke.parse_holds(output))
        self.assertEqual({}, smoke.parse_holds(""))
        for bad in [f"{CALL_ID}|1000|32768|3|15", f"{CALL_ID}|1000|32768|3|15|x", f"{CALL_ID}|1|2|3|4|5|6"]:
            with self.subTest(row=bad), self.assertRaises(ValueError):
                smoke.parse_holds(bad)

    def test_hold_problem_needs_one_reservation_held_and_costed_at_the_bound(self):
        # The responses-default-output example of api/metering.yaml.
        self.assertIsNone(smoke.hold_problem([(1000, 32768, 3, 15, 494520)], smoke.DEFAULT_OUTPUT_BOUND))
        self.assertIn("max_output_tokens 64", smoke.hold_problem([(1000, 64, 3, 15, 3960)], 32768))
        self.assertIn("max_cost 4500", smoke.hold_problem([(1000, 32768, 3, 15, 4500)], 32768))
        self.assertIn("0 reservations", smoke.hold_problem([], 32768))
        self.assertIn("2 reservations", smoke.hold_problem([(1, 64, 3, 15, 963)] * 2, 64))


class RunTests(unittest.TestCase):
    def quiet(self):
        return contextlib.redirect_stdout(io.StringIO())

    def test_call_keeps_the_hold_of_a_relayed_call_only(self):
        run = smoke.Run()
        with self.quiet():
            run.call("a", lambda: smoke.Response(200, CALL_ID, "application/json", b"{}"), 200,
                     outcome="completed", hold=32768)
            run.call("b", lambda: smoke.Response(200, OTHER_ID, "application/json", b"{}"), 200,
                     outcome="completed")
            run.call("c", lambda: smoke.Response(401, "req_11111111-1111-4111-8111-111111111111",
                                                 "application/json", b"{}"), 401, hold=64)
        self.assertEqual(0, run.failed)
        self.assertEqual([("a", CALL_ID, 32768)], run.holds)
        self.assertEqual(["a", "b"], [name for name, _, _ in run.charged])
        self.assertEqual(["c"], [name for name, _ in run.refused])

    def test_check_holds_reports_each_reservation(self):
        run = smoke.Run()
        run.holds = [("a", CALL_ID, 32768), ("b", OTHER_ID, 64)]
        rows = {CALL_ID: [(1000, 32768, 3, 15, 494520)], OTHER_ID: [(200, 32768, 3, 15, 492120)]}
        output = io.StringIO()
        with mock.patch.object(smoke, "query_holds", return_value=rows) as query, \
                contextlib.redirect_stdout(output):
            smoke.check_holds(run, "wt-1")
        query.assert_called_once_with("wt-1", [CALL_ID, OTHER_ID])
        self.assertEqual(1, run.failed)
        lines = output.getvalue().splitlines()
        self.assertEqual("PASS h. reservation: a", lines[0])
        self.assertTrue(lines[1].startswith("FAIL h. reservation: b: max_output_tokens 32768, want 64"))

    def test_check_holds_fails_every_hold_when_psql_fails(self):
        run = smoke.Run()
        run.holds = [("a", CALL_ID, 32768), ("b", OTHER_ID, 64)]
        with mock.patch.object(smoke, "query_holds", side_effect=RuntimeError("psql exited with 2")), \
                self.quiet():
            smoke.check_holds(run, None)
        self.assertEqual(2, run.failed)

    def test_check_holds_queries_nothing_without_holds(self):
        with mock.patch.object(smoke, "query_holds") as query:
            smoke.check_holds(smoke.Run(), None)
        query.assert_not_called()

    def test_query_holds_turns_a_bad_row_into_a_psql_failure(self):
        with mock.patch.object(smoke, "run_psql", return_value=f"{CALL_ID}|1|2\n"):
            with self.assertRaises(RuntimeError):
                smoke.query_holds(None, [CALL_ID])


if __name__ == "__main__":
    unittest.main()


class KubernetesCommandTests(unittest.TestCase):
    def test_explicit_kubernetes_psql(self):
        target=smoke.KubernetesDatabase('kind-gateflow71','gf','postgres-0')
        command=smoke.psql_command(target,'SELECT 1')
        self.assertEqual(command[:8],['kubectl','--context','kind-gateflow71','--namespace','gf','exec','postgres-0','--'])
        self.assertEqual(command[-1],'SELECT 1')
        self.assertIn('psql',command)

    def test_reject_mixed_targets(self):
        with self.assertRaises(SystemExit):
            smoke.main(['--project','other','--kube-context','kind-gf','--namespace','gf','--postgres-pod','postgres-0'])


DATABASE_URL = "postgres://metering:hunter2-must-not-leak@db.internal:5432/gateflow?sslmode=require"


class FakeKubectl:
    """Stands in for kubectl: records each command and fails the verbs in fail."""

    def __init__(self, fail=(), stderr=f"error: cannot connect to {DATABASE_URL}"):
        self.fail, self.stderr, self.commands, self.inputs = set(fail), stderr, [], []

    def __call__(self, command, **kwargs):
        self.commands.append(command)
        self.inputs.append(kwargs.get("input"))
        verb = command[5]
        if verb in self.fail:
            return smoke.subprocess.CompletedProcess(command, 1, "", self.stderr)
        return smoke.subprocess.CompletedProcess(command, 0, f"{CALL_ID}|completed\n" if verb == "exec" else "", "")

    def verbs(self):
        return [command[5] for command in self.commands]


class QueryPodTests(unittest.TestCase):
    def kubectl(self, fake):
        return mock.patch.object(smoke.subprocess, "run", fake)

    def test_db_secret_excludes_the_other_databases_and_needs_a_context(self):
        base = ["--kube-context", "kind-gf", "--namespace", "gf", "--db-secret", "gateflow-runtime"]
        for argv in [base + ["--postgres-pod", "postgres-0"], base + ["--project", "wt-1"],
                     ["--namespace", "gf", "--db-secret", "gateflow-runtime"],
                     ["--kube-context", "kind-gf", "--db-secret", "gateflow-runtime"],
                     ["--kube-context", "kind-gf", "--namespace", "gf", "--db-secret", ""],
                     ["--db-secret-key", "ACCESS_DATABASE_URL"], ["--query-image", "postgres:17.6"],
                     ["--kube-context", "kind-gf", "--namespace", "gf", "--postgres-pod", "postgres-0",
                      "--db-secret-key", "METERING_DATABASE_URL"]]:
            fake = FakeKubectl()
            with self.subTest(argv=argv), self.kubectl(fake), \
                    contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
                smoke.main(argv)
            self.assertEqual([], fake.commands)

    def test_pod_takes_the_url_from_the_secret_and_runs_restricted_on_a_services_node(self):
        spec = smoke.query_pod_spec("gateflow-smoke-db-0a1b2c3d", "gateflow-runtime")
        self.assertEqual(("v1", "Pod", "gateflow-smoke-db-0a1b2c3d"),
                         (spec["apiVersion"], spec["kind"], spec["metadata"]["name"]))
        self.assertEqual(smoke.QUERY_POD_LABELS, spec["metadata"]["labels"])
        pod = spec["spec"]
        self.assertEqual({"gateflow.io/workload": "services"}, pod["nodeSelector"])
        self.assertIs(False, pod["automountServiceAccountToken"])
        self.assertEqual("Never", pod["restartPolicy"])
        self.assertTrue(pod["securityContext"]["runAsNonRoot"])
        self.assertEqual(65532, pod["securityContext"]["runAsUser"])
        [container] = pod["containers"]
        self.assertEqual("postgres:17", container["image"])
        self.assertEqual("sleep", container["command"][0])
        self.assertEqual(smoke.QUERY_POD_LIFETIME, int(container["command"][1]))
        self.assertLessEqual(smoke.QUERY_POD_LIFETIME, 3600)
        self.assertEqual({"allowPrivilegeEscalation": False, "capabilities": {"drop": ["ALL"]}},
                         container["securityContext"])
        env = {variable["name"]: variable for variable in container["env"]}
        self.assertEqual({"DATABASE_URL", "HOME"}, set(env))
        self.assertEqual({"secretKeyRef": {"name": "gateflow-runtime", "key": "METERING_DATABASE_URL"}},
                         env["DATABASE_URL"]["valueFrom"])
        self.assertNotIn("value", env["DATABASE_URL"])
        self.assertEqual("/tmp", env["HOME"]["value"])
        text = json.dumps(spec)
        self.assertNotIn("postgres://", text)
        self.assertNotIn("sslmode", text)

    def test_pod_key_and_image_can_be_chosen(self):
        spec = smoke.query_pod_spec("p", "other-secret", "ACCESS_DATABASE_URL", "registry.local/postgres:17")
        [container] = spec["spec"]["containers"]
        self.assertEqual("registry.local/postgres:17", container["image"])
        self.assertEqual({"name": "other-secret", "key": "ACCESS_DATABASE_URL"},
                         container["env"][0]["valueFrom"]["secretKeyRef"])

    def test_pod_names_are_random_and_marked_as_smoke(self):
        names = {smoke.query_pod_name() for _ in range(5)}
        self.assertEqual(5, len(names))
        for name in names:
            self.assertRegex(name, r"^gateflow-smoke-db-[0-9a-f]{8}$")

    def test_exec_passes_the_sql_as_an_argument(self):
        target = smoke.QueryPod("kind-gf", "gf", "gateflow-smoke-db-0a1b2c3d")
        sql = smoke.ledger_sql([CALL_ID])
        command = smoke.psql_command(target, sql)
        self.assertEqual(["kubectl", "--context", "kind-gf", "--namespace", "gf", "exec",
                          "gateflow-smoke-db-0a1b2c3d", "--", "sh", "-c"], command[:10])
        script = command[10]
        self.assertEqual(sql, command[-1])
        self.assertEqual(13, len(command))
        self.assertNotIn(sql, script)
        self.assertNotIn("SELECT", script)
        self.assertIn('"$1"', script)
        self.assertIn('"$DATABASE_URL"', script)
        self.assertIn("psql -X -tAc", script)
        self.assertNotIn("-U", script)  # the role and sslmode come from the URL as it is

    def test_query_pod_is_created_then_used_then_deleted(self):
        fake = FakeKubectl()
        with self.kubectl(fake):
            with smoke.query_pod("kind-gf", "gf", "gateflow-runtime") as target:
                self.assertEqual({CALL_ID: ["completed"]}, smoke.query_ledger(target, [CALL_ID]))
        self.assertEqual(["create", "wait", "exec", "delete"], fake.verbs())
        for command in fake.commands:
            self.assertEqual(["kubectl", "--context", "kind-gf", "--namespace", "gf"], command[:5])
        created = json.loads(fake.inputs[0])
        self.assertEqual(target.pod, created["metadata"]["name"])
        self.assertIn(f"pod/{target.pod}", fake.commands[1])
        self.assertIn("--for=condition=Ready", fake.commands[1])
        self.assertEqual(["delete", "pod", target.pod, "--wait=false"], fake.commands[3][5:9])

    def test_query_pod_is_deleted_when_it_never_gets_ready(self):
        fake = FakeKubectl(fail={"wait"})
        with self.kubectl(fake), self.assertRaises(RuntimeError) as failure:
            with smoke.query_pod("kind-gf", "gf", "gateflow-runtime"):
                self.fail("the block must not run")
        self.assertEqual(["create", "wait", "delete"], fake.verbs())
        self.assertNotIn("hunter2", str(failure.exception))

    def test_query_pod_is_deleted_when_the_checks_fail_or_are_interrupted(self):
        for error in [RuntimeError("check failed"), KeyboardInterrupt()]:
            fake = FakeKubectl()
            with self.subTest(error=error), self.kubectl(fake), self.assertRaises(type(error)):
                with smoke.query_pod("kind-gf", "gf", "gateflow-runtime"):
                    raise error
            self.assertEqual(["create", "wait", "delete"], fake.verbs())

    def test_main_deletes_the_pod_when_checks_raise(self):
        fake = FakeKubectl()
        argv = ["--kube-context", "kind-gf", "--namespace", "gf", "--db-secret", "gateflow-runtime"]
        with self.kubectl(fake), mock.patch.object(smoke, "run_checks", side_effect=KeyboardInterrupt) as checks, \
                self.assertRaises(KeyboardInterrupt):
            smoke.main(argv)
        target = checks.call_args.args[2]
        self.assertIsInstance(target, smoke.QueryPod)
        self.assertEqual(["create", "wait", "delete"], fake.verbs())
        self.assertEqual(target.pod, fake.commands[-1][7])

    def test_main_passes_key_and_image_and_fails_setup_without_a_ready_pod(self):
        fake = FakeKubectl(fail={"create"})
        argv = ["--kube-context", "kind-gf", "--namespace", "gf", "--db-secret", "gateflow-runtime",
                "--db-secret-key", "ACCESS_DATABASE_URL", "--query-image", "postgres:17.6"]
        output = io.StringIO()
        with self.kubectl(fake), mock.patch.object(smoke, "run_checks") as checks, \
                contextlib.redirect_stdout(output), contextlib.redirect_stderr(io.StringIO()):
            self.assertEqual(1, smoke.main(argv))
        checks.assert_not_called()
        self.assertEqual(["create", "delete"], fake.verbs())
        container = json.loads(fake.inputs[0])["spec"]["containers"][0]
        self.assertEqual("postgres:17.6", container["image"])
        self.assertEqual("ACCESS_DATABASE_URL", container["env"][0]["valueFrom"]["secretKeyRef"]["key"])
        self.assertTrue(output.getvalue().startswith("FAIL setup: query Pod: kubectl create exited with 1"))
        self.assertNotIn("hunter2", output.getvalue())
        self.assertIn("postgres://<redacted>@", output.getvalue())

    def test_psql_errors_are_redacted(self):
        target = smoke.QueryPod("kind-gf", "gf", "gateflow-smoke-db-0a1b2c3d")
        stderr = (f'psql: error: connection to "{DATABASE_URL}" failed\n'
                  "also postgresql://metering:p%40ss@host/db and password=hunter2-must-not-leak&x=1")
        with self.kubectl(FakeKubectl(fail={"exec"}, stderr=stderr)), self.assertRaises(RuntimeError) as failure:
            smoke.query_ledger(target, [CALL_ID])
        message = str(failure.exception)
        self.assertTrue(message.startswith("psql exited with 1"), message)
        for secret in ["hunter2", "metering:", "p%40ss"]:
            self.assertNotIn(secret, message)
        self.assertIn("db.internal:5432/gateflow?sslmode=require", message)

    def test_kubectl_errors_and_timeouts_are_redacted(self):
        def timeout(command, **kwargs):
            raise smoke.subprocess.TimeoutExpired(command, 30, stderr=DATABASE_URL)
        with mock.patch.object(smoke.subprocess, "run", timeout), self.assertRaises(RuntimeError) as failure:
            smoke.run_kubectl(["kubectl", "--context", "c", "--namespace", "n", "wait", "pod/p"])
        self.assertIn("kubectl wait failed", str(failure.exception))
        self.assertEqual("postgres://<redacted>@db.internal/x password=<redacted>",
                         smoke.redact("postgres://u:p@w@db.internal/x password=secret"))
        self.assertEqual("no url here", smoke.redact("no url here"))

    def test_cleanup_failure_is_a_redacted_warning(self):
        fake = FakeKubectl(fail={"delete"})
        errors = io.StringIO()
        with self.kubectl(fake), contextlib.redirect_stderr(errors):
            with smoke.query_pod("kind-gf", "gf", "gateflow-runtime") as target:
                pass
        self.assertIn(f"WARNING could not delete query Pod {target.pod}", errors.getvalue())
        self.assertNotIn("hunter2", errors.getvalue())

    def test_compose_and_fixture_pod_targets_are_unchanged(self):
        with mock.patch.object(smoke, "run_checks", return_value=0) as checks:
            self.assertEqual(0, smoke.main(["--project", "wt-1"]))
            self.assertEqual(0, smoke.main([]))
            self.assertEqual(0, smoke.main(["--kube-context", "kind-gf", "--namespace", "gf",
                                            "--postgres-pod", "postgres-0"]))
        self.assertEqual(["wt-1", None, smoke.KubernetesDatabase("kind-gf", "gf", "postgres-0")],
                         [call.args[2] for call in checks.call_args_list])
