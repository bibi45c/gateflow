"""Builds the results table of a k6 run of loadtest/messages.js.

Run k6 with JSON output, then, from the repository root:

    python loadtest/summarize.py <k6 JSON output> [--out <results.json>]

It prints one Markdown row per phase of the plan and writes the same results as JSON
(default: the input path with .results.json). The plan and the time zero come from
the gateflow_plan and gateflow_start points that the script writes, so the output
file alone is enough. Percentiles use the nearest-rank method. Only the standard
library is used.
"""
import argparse
from collections import Counter, defaultdict
from datetime import datetime
import json
import math
from pathlib import Path
import re
import sys

RESULTS = ("completed", "rejected", "timed_out", "failed")
PERCENTILES = (50, 95, 99)
# Statuses that mean the run itself was set up wrong, not that the gateway was slow.
INVALID_KEY = {"401", "403"}
WALLET_EMPTY = "402"
FRACTION = re.compile(r"(\.\d{6})\d+")


def parse_time(value):
    """A k6 timestamp as a datetime; k6 may print more than six fractional digits."""
    return datetime.fromisoformat(FRACTION.sub(r"\1", value.replace("Z", "+00:00")))


def read_points(lines):
    """Yields (metric, time, value, tags) for each Point line of the k6 output."""
    for number, line in enumerate(lines, 1):
        line = line.strip()
        if not line:
            continue
        try:
            record = json.loads(line)
        except json.JSONDecodeError as err:
            raise ValueError(f"line {number}: not JSON: {err}") from None
        if record.get("type") != "Point":
            continue
        data = record["data"]
        yield record["metric"], parse_time(data["time"]), data["value"], data.get("tags") or {}


def percentiles(values):
    """Nearest-rank percentiles and the maximum, in milliseconds, or None if empty."""
    if not values:
        return None
    ordered = sorted(values)
    out = {f"p{p}": round(ordered[max(0, math.ceil(p / 100 * len(ordered)) - 1)], 1)
           for p in PERCENTILES}
    out["max"] = round(ordered[-1], 1)
    return out


class Run:
    """The points of one run, grouped by phase."""

    def __init__(self):
        self.plan = []
        self.start = None
        self.sent = Counter()
        self.http_sent = Counter()  # fallback for recordings predating gateflow_sent
        self.has_sent_counter = False
        self.statuses = defaultdict(Counter)
        self.results = defaultdict(Counter)
        self.waiting = defaultdict(list)
        self.duration = defaultdict(list)
        self.added = defaultdict(list)  # gateway minus direct TTFB, per pair
        self.pair_failed = Counter()
        self.direct_waiting = defaultdict(list)
        self.direct_duration = defaultdict(list)
        self.timed = []  # (metric, time, value) of untagged points: dropped iterations, vus

    def add(self, metric, time, value, tags):
        key = (tags.get("phase"), tags.get("level"))
        if tags.get("path") == "direct":
            # The paired calls straight to the mock count only through their pair, and
            # their timings show whether the mock kept up.
            if tags.get("status") == "200" and metric in ("http_req_waiting", "http_req_duration"):
                target = self.direct_waiting if metric == "http_req_waiting" else self.direct_duration
                target[key].append(value)
            return
        if metric == "gateflow_plan":
            self.plan.append((tags["phase"], tags["level"], float(tags["start_s"]), float(tags["end_s"])))
        elif metric == "gateflow_start":
            self.start = time if self.start is None else min(self.start, time)
        elif metric == "http_reqs":
            self.http_sent[key] += int(value)
            self.statuses[key][tags.get("status", "")] += int(value)
        elif metric == "gateflow_sent":
            self.has_sent_counter = True
            self.sent[key] += int(value)
        elif metric.startswith("gateflow_") and metric[len("gateflow_"):] in RESULTS:
            self.results[key][metric[len("gateflow_"):]] += int(value)
        elif metric in ("gateflow_added_ttfb_ms", "gateflow_added_first_output_ms"):
            self.added[key].append(value)
        elif metric == "gateflow_pair_failed":
            self.pair_failed[key] += int(value)
        elif metric in ("http_req_waiting", "http_req_duration") and tags.get("status") == "200":
            (self.waiting if metric == "http_req_waiting" else self.duration)[key].append(value)
        elif metric in ("dropped_iterations", "vus"):
            self.timed.append((metric, time, value))

    def phase_at(self, time):
        """The (phase, level) whose interval holds time, or None."""
        offset = (time - self.start).total_seconds()
        for phase, level, start, end in self.plan:
            if start <= offset < end:
                return phase, level
        return None

    def timed_by_phase(self):
        dropped, vus = Counter(), defaultdict(int)
        for metric, time, value in self.timed:
            key = self.phase_at(time) if self.start else None
            if metric == "dropped_iterations":
                dropped[key] += int(value)
            elif key:
                vus[key] = max(vus[key], int(value))
        return dropped, vus


def phase_row(run, phase, level, start, end, dropped, vus):
    key = (phase, level)
    results = run.results[key]
    sent = (run.sent if run.has_sent_counter else run.http_sent)[key]
    lost = dropped[key]
    scheduled = sent + lost
    statuses = dict(sorted(run.statuses[key].items()))
    return {
        "phase": phase, "level_rps": float(level), "start_s": start, "end_s": end,
        "scheduled": scheduled, "sent": sent, "dropped": lost,
        "admitted": statuses.get("200", 0),
        **{name: results[name] for name in RESULTS},
        "unclassified": sent - sum(results[name] for name in RESULTS),
        "completion_rate": round(results["completed"] / scheduled, 4) if scheduled else None,
        "throughput_rps": round(results["completed"] / (end - start), 2) if end > start else None,
        "ttfb_ms": percentiles(run.waiting[key]),
        "duration_ms": percentiles(run.duration[key]),
        "pairs": len(run.added[key]), "failed_pairs": run.pair_failed[key],
        "added_ttfb_ms": percentiles(run.added[key]),
        "direct_ttfb_ms": percentiles(run.direct_waiting[key]),
        "direct_duration_ms": percentiles(run.direct_duration[key]),
        "max_active_vus": vus.get(key, 0),
        "statuses": statuses,
    }


def summarize(lines):
    """The results of a run, as a dict ready for JSON."""
    run = Run()
    for point in read_points(lines):
        run.add(*point)
    if not run.plan:
        raise ValueError("no gateflow_plan points: was this output written by loadtest/messages.js?")
    if run.start is None:
        raise ValueError("no gateflow_start point: the run sent no request")
    run.plan.sort(key=lambda p: p[2])
    if len({(p[0], p[1]) for p in run.plan}) != len(run.plan):
        raise ValueError("repeated phase/level in plan: use distinct LEVELS")
    dropped, vus = run.timed_by_phase()
    rows = [phase_row(run, *plan, dropped, vus) for plan in run.plan]
    statuses = Counter()
    for row in rows:
        statuses.update(row["statuses"])
    return {
        "phases": rows,
        "checks": {
            "wallet_empty_responses": statuses[WALLET_EMPTY],
            "invalid_key_responses": sum(statuses[s] for s in INVALID_KEY),
            "dropped_iterations": sum(dropped.values()),
            "dropped_outside_plan": dropped[None],
            "unclassified_requests": sum(max(0, row["unclassified"]) for row in rows),
            "excess_results": sum(max(0, -row["unclassified"]) for row in rows),
        },
        "sent_source": "gateflow_sent" if run.has_sent_counter else "http_reqs (legacy)",
    }


def markdown(summary):
    """The results as a Markdown table, then one line per check."""
    head = ("| Phase | Level (req/s) | Scheduled | HTTP 200 | Completed | 429/503/529 | Timed out "
            "| Failed | Unclassified | Completion | Throughput (req/s) | TTFB p50 / p95 / p99 (ms) "
            "| Added TTFB p95 (ms), pairs | Failed pairs | Direct duration p95 (ms) | Max VUs |")
    lines = [head, "|" + "---|" * (head.count("|") - 1)]
    for row in summary["phases"]:
        first = row["ttfb_ms"]
        first = " / ".join(str(first[f"p{p}"]) for p in PERCENTILES) if first else "-"
        rate = f"{row['completion_rate']:.2%}" if row["completion_rate"] is not None else "-"
        paired = (f"{row['added_ttfb_ms']['p95']}, {row['pairs']}"
                  if row["added_ttfb_ms"] else "-")
        direct = row["direct_duration_ms"]["p95"] if row["direct_duration_ms"] else "-"
        lines.append(f"| {row['phase']} | {row['level_rps']:g} | {row['scheduled']} | {row['admitted']} "
                     f"| {row['completed']} | {row['rejected']} | {row['timed_out']} | {row['failed']} "
                     f"| {row['unclassified']} | {rate} | {row['throughput_rps']} | {first} "
                     f"| {paired} | {row['failed_pairs']} | {direct} | {row['max_active_vus']} |")
    lines.append("")
    lines += [f"- {name}: {value}" for name, value in summary["checks"].items()]
    return "\n".join(lines)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__.split("\n", 1)[0])
    parser.add_argument("output", type=Path, help="k6 JSON output (k6 run --out json=<file>)")
    parser.add_argument("--out", type=Path, help="results file (default: <output>.results.json)")
    args = parser.parse_args(argv)
    try:
        with args.output.open(encoding="utf-8") as lines:
            summary = summarize(lines)
    except (OSError, ValueError, KeyError) as err:
        print(f"error: {args.output}: {err}", file=sys.stderr)
        return 1
    out = args.out or args.output.with_suffix(".results.json")
    out.write_text(json.dumps(summary, indent=2) + "\n", encoding="utf-8", newline="\n")
    print(markdown(summary))
    print(f"\nWrote {out}")
    return 2 if any(summary["checks"].values()) else 0


if __name__ == "__main__":
    sys.exit(main())
