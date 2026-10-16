"""Unit tests of summarize.py. They need neither k6 nor a running stack."""
import contextlib
from datetime import datetime, timedelta, timezone
import io
import json
from pathlib import Path
import tempfile
import unittest

import summarize

START = datetime(2026, 10, 14, 6, 0, tzinfo=timezone.utc)


def at(seconds):
    """A k6 timestamp `seconds` after START, with nine fractional digits as k6 prints."""
    return (START + timedelta(seconds=seconds)).strftime("%Y-%m-%dT%H:%M:%S.%f") + "123Z"


def point(metric, seconds, value=1, **tags):
    return json.dumps({"type": "Point", "metric": metric,
                       "data": {"time": at(seconds), "value": value, "tags": tags}})


def plan(*phases):
    """gateflow_plan points for (phase, level, start, end), and the start point."""
    lines = [point("gateflow_plan", -0.5, phase=p, level=str(lv), start_s=str(s), end_s=str(e))
             for p, lv, s, e in phases]
    return lines + [point("gateflow_start", 0)]


def request(seconds, phase, level, status=200, result="completed", waiting=20.0, duration=1600.0,
            tenant=None):
    """The points k6 writes for one request: http_reqs, the timings and the result counter."""
    sent = {"phase": phase, "level": str(level), **({"tenant": tenant} if tenant else {})}
    tags = {**sent, "status": str(status)}
    http = {**tags, "path": "gateway"}
    return [point("gateflow_sent", seconds - 0.1, **sent),
            point("http_reqs", seconds, **http),
            point("http_req_waiting", seconds, waiting, **http),
            point("http_req_duration", seconds, duration, **http),
            point(f"gateflow_{result}", seconds, **tags)]


def direct(seconds, phase, level, status=200, waiting=5.0):
    """The http points of a paired call straight to the mock; it has no result counter."""
    tags = {"phase": phase, "level": str(level), "status": str(status), "path": "direct"}
    return [point("http_reqs", seconds, **tags),
            point("http_req_waiting", seconds, waiting, **tags),
            point("http_req_duration", seconds, 1500.0, **tags)]


TWO_PHASES = plan(("warmup", 2, 0, 10), ("hold", 2, 10, 20))


class ParseTests(unittest.TestCase):
    def test_parse_time_accepts_nanoseconds_and_z(self):
        parsed = summarize.parse_time("2026-10-14T06:00:01.123456789Z")
        self.assertEqual((parsed.second, parsed.microsecond), (1, 123456))
        self.assertEqual(parsed.utcoffset().total_seconds(), 0)

    def test_read_points_skips_metric_lines_and_blanks(self):
        lines = [json.dumps({"type": "Metric", "metric": "http_reqs", "data": {}}), "",
                 point("http_reqs", 1, status="200")]
        self.assertEqual([p[0] for p in summarize.read_points(lines)], ["http_reqs"])

    def test_read_points_names_the_bad_line(self):
        with self.assertRaisesRegex(ValueError, "line 2"):
            list(summarize.read_points([point("vus", 0), "{not json"]))


class PercentileTests(unittest.TestCase):
    def test_nearest_rank(self):
        result = summarize.percentiles([float(v) for v in range(100, 0, -1)])
        self.assertEqual(result, {"p50": 50.0, "p95": 95.0, "p99": 99.0, "max": 100.0})

    def test_single_value_and_empty(self):
        self.assertEqual(summarize.percentiles([7.0]), {"p50": 7.0, "p95": 7.0, "p99": 7.0, "max": 7.0})
        self.assertIsNone(summarize.percentiles([]))


class SummarizeTests(unittest.TestCase):
    def test_counts_rates_and_timings_per_phase(self):
        lines = TWO_PHASES + request(1, "warmup", 2) + request(11, "hold", 2, waiting=30.0)
        lines += request(12, "hold", 2, status=503, result="rejected", waiting=5.0)
        lines += request(13, "hold", 2, status=0, result="timed_out")
        warmup, hold = summarize.summarize(lines)["phases"]
        self.assertEqual((warmup["phase"], warmup["scheduled"], warmup["completed"]), ("warmup", 1, 1))
        self.assertEqual(hold["scheduled"], 3)
        self.assertEqual(hold["admitted"], 1)
        self.assertEqual((hold["completed"], hold["rejected"], hold["timed_out"], hold["failed"]), (1, 1, 1, 0))
        self.assertEqual(hold["completion_rate"], round(1 / 3, 4))
        self.assertEqual(hold["throughput_rps"], 0.1)
        self.assertEqual(hold["statuses"], {"0": 1, "200": 1, "503": 1})
        # Only HTTP 200 responses count for TTFB percentiles.
        self.assertEqual(hold["ttfb_ms"]["max"], 30.0)

    def test_dropped_iterations_and_vus_are_placed_by_time(self):
        lines = TWO_PHASES + request(11, "hold", 2)
        lines += [point("dropped_iterations", 12, 3), point("dropped_iterations", 25, 2),
                  point("vus", 5, 4), point("vus", 15, 9), point("vus", 16, 6)]
        summary = summarize.summarize(lines)
        warmup, hold = summary["phases"]
        self.assertEqual((hold["dropped"], hold["scheduled"]), (3, 4))
        self.assertEqual((warmup["max_active_vus"], hold["max_active_vus"]), (4, 9))
        self.assertEqual(summary["checks"]["dropped_iterations"], 5)
        self.assertEqual(summary["checks"]["dropped_outside_plan"], 2)

    def test_checks_count_empty_wallets_and_bad_keys(self):
        lines = TWO_PHASES + request(11, "hold", 2, status=402, result="failed")
        lines += request(12, "hold", 2, status=401, result="failed")
        lines += request(13, "hold", 2, status=403, result="failed")
        checks = summarize.summarize(lines)["checks"]
        self.assertEqual((checks["wallet_empty_responses"], checks["invalid_key_responses"]), (1, 2))

    def test_direct_calls_stay_out_of_the_gateway_table(self):
        lines = TWO_PHASES + request(11, "hold", 2) + direct(11, "hold", 2, status=500)
        hold = summarize.summarize(lines)["phases"][1]
        self.assertEqual((hold["scheduled"], hold["admitted"], hold["statuses"]), (1, 1, {"200": 1}))
        self.assertEqual(hold["ttfb_ms"]["max"], 20.0)
        self.assertIsNone(hold["direct_ttfb_ms"])  # the direct call answered 500
        self.assertIsNone(hold["direct_duration_ms"])

    def test_direct_timings_show_the_mock(self):
        lines = TWO_PHASES + request(11, "hold", 2) + direct(11, "hold", 2, waiting=4.0)
        hold = summarize.summarize(lines)["phases"][1]
        self.assertEqual(hold["direct_ttfb_ms"]["max"], 4.0)
        self.assertEqual(hold["direct_duration_ms"]["p95"], 1500.0)

    def test_pairs_give_the_added_ttfb(self):
        lines = TWO_PHASES + request(11, "hold", 2) + direct(11, "hold", 2)
        lines += [point("gateflow_added_ttfb_ms", 11, v, phase="hold", level="2") for v in (3.0, 9.0, 4.0)]
        lines += [point("gateflow_pair_failed", 12, phase="hold", level="2")]
        warmup, hold = summarize.summarize(lines)["phases"]
        self.assertEqual((hold["pairs"], hold["failed_pairs"]), (3, 1))
        self.assertEqual(hold["added_ttfb_ms"], {"p50": 4.0, "p95": 9.0, "p99": 9.0, "max": 9.0})
        self.assertEqual((warmup["pairs"], warmup["added_ttfb_ms"]), (0, None))
        self.assertIn("| 9.0, 3 |", summarize.markdown(summarize.summarize(lines)))

    def test_interrupted_iteration_without_http_metrics_is_not_lost(self):
        lines = TWO_PHASES + request(11, "hold", 2)
        lines += [point("gateflow_sent", 12, phase="hold", level="2")]
        summary = summarize.summarize(lines)
        hold = summary["phases"][1]
        self.assertEqual((hold["sent"], hold["completed"], hold["unclassified"]), (2, 1, 1))
        self.assertEqual(hold["completion_rate"], 0.5)
        self.assertEqual(summary["checks"]["unclassified_requests"], 1)
        self.assertEqual(summary["sent_source"], "gateflow_sent")

    def test_missing_terminal_counter_after_http_return_is_reported(self):
        lines = TWO_PHASES + request(11, "hold", 2)[:-1]
        summary = summarize.summarize(lines)
        self.assertEqual(summary["phases"][1]["unclassified"], 1)

    def test_duplicate_terminal_results_fail_accounting(self):
        lines = TWO_PHASES + request(11, "hold", 2)
        lines += [point("gateflow_failed", 11, phase="hold", level="2")]
        summary = summarize.summarize(lines)
        self.assertEqual(summary["phases"][1]["unclassified"], -1)
        self.assertEqual(summary["checks"]["excess_results"], 1)

    def test_last_phase_results_after_the_arrival_window_still_count(self):
        lines = TWO_PHASES + request(24, "hold", 2)
        summary = summarize.summarize(lines)
        hold = summary["phases"][1]
        self.assertEqual((hold["completed"], hold["unclassified"]), (1, 0))

    def test_legacy_recordings_are_read_with_accurate_timing_labels(self):
        lines = TWO_PHASES + request(11, "hold", 2)
        lines = [line for line in lines if json.loads(line)["metric"] != "gateflow_sent"]
        lines += [point("gateflow_added_first_output_ms", 11, 15.0, phase="hold", level="2")]
        summary = summarize.summarize(lines)
        self.assertEqual(summary["sent_source"], "http_reqs (legacy)")
        self.assertEqual(summary["phases"][1]["added_ttfb_ms"]["p95"], 15.0)
        self.assertNotIn("First output", summarize.markdown(summary))

    def test_repeated_phase_and_level_is_rejected_instead_of_merging_rows(self):
        lines = plan(("hold", 2, 0, 10), ("hold", 2, 10, 20))
        with self.assertRaisesRegex(ValueError, "repeated phase/level"):
            summarize.summarize(lines)

    def test_plan_is_sorted_by_start(self):
        lines = plan(("hold", 2, 10, 20), ("warmup", 2, 0, 10)) + request(1, "warmup", 2)
        self.assertEqual([r["phase"] for r in summarize.summarize(lines)["phases"]], ["warmup", "hold"])

    def test_output_without_plan_or_start_is_refused(self):
        with self.assertRaisesRegex(ValueError, "gateflow_plan"):
            summarize.summarize(request(1, "warmup", 2))
        with self.assertRaisesRegex(ValueError, "gateflow_start"):
            summarize.summarize(TWO_PHASES[:-1])


class TenantTests(unittest.TestCase):
    def test_rows_per_phase_and_tenant(self):
        lines = TWO_PHASES + request(1, "warmup", 2, tenant="tenant-02")
        lines += request(11, "hold", 2, waiting=30.0, tenant="tenant-02")
        lines += request(12, "hold", 2, status=429, result="rejected", tenant="tenant-02")
        lines += request(13, "hold", 2, status=0, result="timed_out", tenant="tenant-01")
        lines += request(14, "hold", 2, status=500, result="failed", tenant="tenant-01")
        lines += request(15, "hold", 2, waiting=10.0, tenant="tenant-01")
        rows = summarize.summarize(lines)["tenants"]
        self.assertEqual([(r["phase"], r["tenant"]) for r in rows],
                         [("warmup", "tenant-02"), ("hold", "tenant-01"), ("hold", "tenant-02")])
        one, two = rows[1], rows[2]
        self.assertEqual((one["sent"], one["completed"], one["rejected"], one["timed_out"], one["failed"]),
                         (3, 1, 0, 1, 1))
        self.assertEqual((two["sent"], two["completed"], two["rejected"]), (2, 1, 1))
        self.assertEqual((one["completion_rate"], two["completion_rate"]), (round(1 / 3, 4), 0.5))
        # Only HTTP 200 responses count for TTFB, as in the phase table.
        self.assertEqual((one["ttfb_ms"]["p95"], two["ttfb_ms"]["p95"]), (10.0, 30.0))

    def test_tenant_rows_add_up_to_the_phase_row(self):
        lines = TWO_PHASES
        for i in range(6):
            lines = lines + request(11 + i, "hold", 2, tenant=f"tenant-0{i % 3 + 1}")
        summary = summarize.summarize(lines)
        hold = [r for r in summary["tenants"] if r["phase"] == "hold"]
        self.assertEqual(sum(r["sent"] for r in hold), summary["phases"][1]["sent"])
        self.assertEqual([r["sent"] for r in hold], [2, 2, 2])

    def test_direct_calls_and_untagged_points_stay_out_of_tenants(self):
        lines = TWO_PHASES + request(11, "hold", 2, tenant="tenant-01")
        lines += [point("http_reqs", 11, phase="hold", level="2", status="200", path="direct",
                        tenant="tenant-01")]
        lines += request(12, "hold", 2)
        rows = summarize.summarize(lines)["tenants"]
        self.assertEqual([(r["tenant"], r["sent"]) for r in rows], [("tenant-01", 1)])

    def test_spread_gives_min_max_and_ratio(self):
        lines = TWO_PHASES + request(11, "hold", 2, waiting=10.0, tenant="tenant-01")
        lines += request(12, "hold", 2, waiting=40.0, tenant="tenant-02")
        lines += request(13, "hold", 2, status=503, result="rejected", tenant="tenant-02")
        (spread,) = summarize.summarize(lines)["tenant_spread"]
        self.assertEqual(spread, {
            "phase": "hold", "level_rps": 2.0, "tenants": 2, "min_sent": 1, "max_sent": 2,
            "min_completion_rate": 0.5, "max_completion_rate": 1.0,
            "min_ttfb_p95_ms": 10.0, "max_ttfb_p95_ms": 40.0, "ttfb_p95_ratio": 4.0,
        })

    def test_spread_without_ttfb_has_no_ratio(self):
        lines = TWO_PHASES + request(11, "hold", 2, status=503, result="rejected", tenant="tenant-01")
        (spread,) = summarize.summarize(lines)["tenant_spread"]
        self.assertEqual((spread["min_ttfb_p95_ms"], spread["ttfb_p95_ratio"]), (None, None))
        self.assertEqual(spread["max_completion_rate"], 0.0)

    def test_single_shared_tenant_has_one_row_per_phase(self):
        lines = TWO_PHASES + request(1, "warmup", 2, tenant="tenant-01")
        lines += request(11, "hold", 2, tenant="tenant-01")
        summary = summarize.summarize(lines)
        self.assertEqual([r["tenant"] for r in summary["tenants"]], ["tenant-01", "tenant-01"])
        self.assertEqual([s["tenants"] for s in summary["tenant_spread"]], [1, 1])
        self.assertEqual(summary["tenant_spread"][1]["ttfb_p95_ratio"], 1.0)

    def test_legacy_recordings_count_tenants_from_http_reqs(self):
        lines = TWO_PHASES + request(11, "hold", 2, tenant="tenant-01")
        lines = [line for line in lines if json.loads(line)["metric"] != "gateflow_sent"]
        (row,) = summarize.summarize(lines)["tenants"]
        self.assertEqual((row["sent"], row["completion_rate"]), (1, 1.0))

    def test_markdown_tenant_tables_only_with_tenant_tags(self):
        self.assertNotIn("Per tenant", summarize.markdown(
            summarize.summarize(TWO_PHASES + request(11, "hold", 2))))
        lines = TWO_PHASES + request(11, "hold", 2, tenant="tenant-01")
        lines += request(12, "hold", 2, waiting=40.0, tenant="tenant-02")
        text = summarize.markdown(summarize.summarize(lines))
        self.assertIn("| hold | 2 | tenant-01 | 1 | 1 | 0 | 0 | 0 | 100.00% | 20.0 / 20.0 |", text)
        self.assertIn("| hold | 2 | 2 | 1 / 1 | 100.00% / 100.00% | 20.0 / 40.0 | 2.0 |", text)
        for block in text.split("\n\n"):
            rows = [line for line in block.splitlines() if line.startswith("|")]
            self.assertLessEqual(len({row.count("|") for row in rows}), 1)

    def test_tenant_results_do_not_change_the_exit_code(self):
        with tempfile.TemporaryDirectory() as tmp:
            source = Path(tmp) / "run.json"
            lines = TWO_PHASES + request(11, "hold", 2, status=503, result="rejected", tenant="tenant-01")
            source.write_text("\n".join(lines) + "\n", encoding="utf-8")
            with contextlib.redirect_stdout(io.StringIO()):
                self.assertEqual(summarize.main([str(source)]), 0)


class OutputTests(unittest.TestCase):
    def test_markdown_rows_match_the_header(self):
        text = summarize.markdown(summarize.summarize(TWO_PHASES + request(11, "hold", 2)))
        rows = [line for line in text.splitlines() if line.startswith("|")]
        self.assertEqual(len({row.count("|") for row in rows}), 1)
        self.assertIn("| hold | 2 | 1 | 1 | 1 | 0 | 0 | 0 | 0 | 100.00% | 0.1 | 20.0 / 20.0 / 20.0 |", text)
        self.assertIn("Failed pairs", text)
        self.assertIn("Direct duration", text)

    def test_main_writes_the_results_file(self):
        with tempfile.TemporaryDirectory() as tmp:
            source = Path(tmp) / "run.json"
            source.write_text("\n".join(TWO_PHASES + request(11, "hold", 2)) + "\n", encoding="utf-8")
            with contextlib.redirect_stdout(io.StringIO()):
                self.assertEqual(summarize.main([str(source)]), 0)
            written = json.loads((Path(tmp) / "run.results.json").read_text(encoding="utf-8"))
            self.assertEqual(written["phases"][1]["completed"], 1)

    def test_main_reports_unusable_input(self):
        with tempfile.TemporaryDirectory() as tmp:
            source = Path(tmp) / "run.json"
            source.write_text(point("vus", 0) + "\n", encoding="utf-8")
            with contextlib.redirect_stderr(io.StringIO()) as err:
                self.assertEqual(summarize.main([str(source)]), 1)
            self.assertIn("gateflow_plan", err.getvalue())

    def test_main_returns_nonzero_but_writes_incomplete_results(self):
        with tempfile.TemporaryDirectory() as tmp:
            source = Path(tmp) / "run.json"
            lines = TWO_PHASES + [point("gateflow_sent", 11, phase="hold", level="2")]
            source.write_text("\n".join(lines) + "\n", encoding="utf-8")
            with contextlib.redirect_stdout(io.StringIO()):
                self.assertEqual(summarize.main([str(source)]), 2)
            written = json.loads(source.with_suffix(".results.json").read_text(encoding="utf-8"))
            self.assertEqual(written["checks"]["unclassified_requests"], 1)


if __name__ == "__main__":
    unittest.main()
