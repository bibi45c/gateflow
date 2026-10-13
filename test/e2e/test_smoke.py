"""Unit tests of the smoke test's helpers. They need no network and no Docker."""
import json
import re
import unittest

import smoke

CALL_ID = "req_3f1c2a9e-7b4d-4e8a-9c61-2d5b8f0a7e34"


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


if __name__ == "__main__":
    unittest.main()
