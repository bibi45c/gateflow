"""Export the Prometheus series of a load-test run for the results report (#77).

Operator-run, after loadtest/kube_run.py. Opens kubectl port-forward to the
Prometheus Service (port 80) on a free local port, runs a fixed list of range
queries over the run window and writes each response, unchanged, to <name>.json,
with index.json listing the queries, the window, the step and the series returned.

The window is load_start to load_end of a kube_run.py run record (--record), or
--start to --end, plus --tail-seconds. When the end of that window is still to come,
the script first waits for it, so that the tail samples exist.

Exit codes: 0 when every query returned series, 2 when one returned none (the others
are still written), 1 on an HTTP, connection or port-forward error, 130 after Ctrl-C.
Requires kubectl on PATH. Never uses the default kubeconfig.
"""
import argparse
import contextlib
from datetime import datetime, timedelta
import http.client
import json
import math
from pathlib import Path
import re
import socket
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request

import kube_run

deploy = kube_run.deploy
PORT = 80  # the Service port of the prometheus chart's server
READY_S = 30  # seconds the port-forward has to answer /-/ready
QUERY_TIMEOUT_S = 60
STEP = re.compile(r'\d+(\.\d+)?(ms|s|m|h)?')
GATEWAY_MESSAGES = 'service="gateway",route="/v1/messages"'
INTERRUPTED = 'interrupted'
# Names for --skip are checked before the namespace and release are known.
NAMES = ('active', 'active_by_pod', 'arrivals', 'results', 'access_results', 'metering_results',
         'first_output_p95', 'duration_mean', 'gateway_duration_sum', 'gateway_duration_count', 'metering_p95',
         'cpu_by_pod', 'mem_by_pod', 'net_by_pod', 'replicas', 'hpa_desired', 'hpa_conditions')


def queries(namespace, release):
    """The fixed query list of the report template (Part 0.4), with the raw inputs of D.

    gateway_duration_sum and gateway_duration_count are the raw counters behind
    duration_mean, so that D can be recomputed over any hold window.
    """
    gateflow = f'namespace="{namespace}"'
    listed = (
        ('active', 'sum(gateflow_gateway_active_admitted_requests)'),
        ('active_by_pod', 'sum by (pod) (gateflow_gateway_active_admitted_requests)'),
        ('arrivals', 'sum(rate(gateflow_gateway_eligible_arrivals_total[1m]))'),
        ('results', 'sum by (result) (rate(gateflow_http_requests_total{service="gateway"}[1m]))'),
        ('access_results', 'sum by (route, result) (rate(gateflow_http_requests_total{service="access"}[1m]))'),
        ('metering_results', 'sum by (route, result) (rate(gateflow_http_requests_total{service="metering"}[1m]))'),
        ('first_output_p95',
         'histogram_quantile(0.95, sum by (le) (rate(gateflow_gateway_first_output_seconds_bucket[1m])))'),
        ('duration_mean', f'sum(rate(gateflow_http_request_duration_seconds_sum{{{GATEWAY_MESSAGES}}}[1m])) / '
                          f'sum(rate(gateflow_http_request_duration_seconds_count{{{GATEWAY_MESSAGES}}}[1m]))'),
        ('gateway_duration_sum', f'sum(gateflow_http_request_duration_seconds_sum{{{GATEWAY_MESSAGES}}})'),
        ('gateway_duration_count', f'sum(gateflow_http_request_duration_seconds_count{{{GATEWAY_MESSAGES}}})'),
        ('metering_p95', 'histogram_quantile(0.95, sum by (le, route) '
                         '(rate(gateflow_http_request_duration_seconds_bucket{service="metering"}[1m])))'),
        ('cpu_by_pod', f'sum by (pod) (rate(container_cpu_usage_seconds_total{{{gateflow},container!=""}}[1m]))'),
        ('mem_by_pod', f'sum by (pod) (container_memory_working_set_bytes{{{gateflow},container!=""}})'),
        ('net_by_pod', f'sum by (pod) (rate(container_network_transmit_bytes_total{{{gateflow}}}[1m]) + '
                       f'rate(container_network_receive_bytes_total{{{gateflow}}}[1m]))'),
        ('replicas', f'kube_deployment_status_replicas_ready{{{gateflow},deployment="{release}-gateway"}}'),
        ('hpa_desired', f'kube_horizontalpodautoscaler_status_desired_replicas{{{gateflow}}}'),
        ('hpa_conditions', f'kube_horizontalpodautoscaler_status_condition{{{gateflow}}}'),
    )
    assert tuple(name for name, _ in listed) == NAMES
    return listed


class ExportError(Exception):
    pass


def utc_time(text):
    """An RFC 3339 time with an explicit offset, so that SGT and UTC cannot be confused."""
    try:
        moment = datetime.fromisoformat(text.replace('Z', '+00:00'))
    except ValueError:
        raise argparse.ArgumentTypeError(f'not an RFC 3339 time: {text!r}') from None
    if moment.tzinfo is None:
        raise argparse.ArgumentTypeError(f'{text!r} has no offset; end it with Z for UTC')
    return moment


def read_record(path):
    """load_start, load_end, namespace and release of a kube_run.py run record."""
    record = json.loads(path.read_text(encoding='utf-8'))
    if not record.get('load_start') or not record.get('load_end'):
        problem = record.get('timeline_problem') or 'it predates the timeline fields'
        raise ValueError(f'the record has no load_start and load_end ({problem}); pass --start and --end')
    return (utc_time(record['load_start']), utc_time(record['load_end']), record.get('namespace'),
            record.get('release'), record.get('name'))


def parse_args(argv):
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('--kubeconfig', type=Path, required=True)
    parser.add_argument('--kube-context', required=True)
    parser.add_argument('--monitoring-namespace', default='monitoring')
    parser.add_argument('--service', default='prometheus-server', help='Prometheus Service, port 80')
    parser.add_argument('--record', type=Path, help='run-record.json of kube_run.py: its load_start and load_end')
    parser.add_argument('--start', type=utc_time, help='window start, RFC 3339 (instead of --record)')
    parser.add_argument('--end', type=utc_time, help='window end, RFC 3339 (instead of --record)')
    parser.add_argument('--tail-seconds', type=int, default=120, help='seconds exported after the end (default: 120)')
    parser.add_argument('--step', default='15s', help='query resolution, like 15s or 30 (default: 15s)')
    parser.add_argument('--out-dir', type=Path, help='output folder (default: prom/ next to --record)')
    parser.add_argument('--namespace', help='GateFlow namespace (default: from --record, else gateflow)')
    parser.add_argument('--release', help='Helm release (default: from --record, else gf)')
    parser.add_argument('--skip', action='append', default=[], choices=NAMES, metavar='NAME',
                        help='leave out a query, repeatable; e.g. hpa_desired and hpa_conditions without autoscaling')
    parser.add_argument('--overwrite', action='store_true', help='replace an earlier export in --out-dir')
    args = parser.parse_args(argv)
    args.run = None
    if args.record is not None:
        if args.start or args.end:
            parser.error('use --record or --start and --end, not both')
        try:
            args.start, args.end, namespace, release, args.run = read_record(args.record)
        except (OSError, ValueError, KeyError, argparse.ArgumentTypeError) as error:
            parser.error(f'cannot read --record: {kube_run.error_text(error)}')
        args.namespace = args.namespace or namespace
        args.release = args.release or release
        args.out_dir = args.out_dir or args.record.parent/'prom'
    elif not (args.start and args.end):
        parser.error('give --record, or both --start and --end')
    elif args.out_dir is None:
        parser.error('--out-dir is required with --start and --end')
    args.namespace, args.release = args.namespace or 'gateflow', args.release or 'gf'
    if args.end <= args.start:
        parser.error('the window ends before it starts')
    if args.tail_seconds < 0:
        parser.error('--tail-seconds must not be negative')
    if not STEP.fullmatch(args.step) or not float(re.sub(r'[a-z]+$', '', args.step)) > 0:
        parser.error('--step is a positive duration like 15s, 1m or 30')
    for option in ('monitoring_namespace', 'service', 'namespace', 'release'):
        if not kube_run.NAME.fullmatch(getattr(args, option)):
            parser.error(f'--{option.replace("_", "-")} must be lowercase letters, digits and dashes')
    return args


def free_port():
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as probe:
        probe.bind(('127.0.0.1', 0))
        return probe.getsockname()[1]


# Never through a proxy: the port-forward listens on this machine only.
OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}))


def fetch(url, timeout):
    with OPENER.open(url, timeout=timeout) as response:
        return response.read()


def stop(process):
    """Ends the port-forward, killing it if it does not stop within five seconds."""
    if process.poll() is None:
        process.terminate()
    try:
        process.wait(timeout=5)
    except subprocess.TimeoutExpired:
        process.kill()
        process.wait()


@contextlib.contextmanager
def port_forward(args, port, popen, fetch, clock, sleep):
    """Yields the base URL of Prometheus through kubectl port-forward, which is always stopped."""
    command = ['kubectl', '--kubeconfig', str(args.kubeconfig.resolve()), '--context', args.kube_context,
               '-n', args.monitoring_namespace, 'port-forward', f'svc/{args.service}', f'{port}:{PORT}',
               '--address', '127.0.0.1']
    base = f'http://127.0.0.1:{port}'
    with tempfile.TemporaryFile() as log:
        process = popen(command, stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=log)
        try:
            deadline = clock() + READY_S
            while True:
                code = process.poll()
                if code is not None:
                    log.seek(0)
                    text = '\n'.join(log.read().decode('utf-8', 'replace').strip().splitlines()[-5:])
                    raise ExportError(f'kubectl port-forward exited with {code}: {text}')
                try:
                    fetch(f'{base}/-/ready', 2)
                    break
                except (OSError, http.client.HTTPException):
                    if clock() >= deadline:
                        raise ExportError(f'Prometheus did not answer through the port-forward within {READY_S} s')
                    sleep(0.5)
            yield base
        finally:
            stop(process)


def query_range(fetch, base, name, query, start, end, step):
    """The raw body and the series of one range query."""
    url = f'{base}/api/v1/query_range?' + urllib.parse.urlencode(
        {'query': query, 'start': kube_run.iso(start), 'end': kube_run.iso(end), 'step': step})
    try:
        body = fetch(url, QUERY_TIMEOUT_S)
    except urllib.error.HTTPError as error:
        detail = error.read().decode('utf-8', 'replace')[:300]
        raise ExportError(f'{name}: HTTP {error.code}: {detail}') from None
    except (OSError, http.client.HTTPException) as error:
        raise ExportError(f'{name}: {type(error).__name__}: {error}') from None
    try:
        document = json.loads(body)
        if document.get('status') != 'success':
            raise ExportError(f"{name}: Prometheus answered {document.get('status')}: {document.get('error')}")
        return body, document['data']['result']
    except (ValueError, KeyError, TypeError, AttributeError) as error:
        raise ExportError(f'{name}: not a Prometheus answer: {error}') from None


def earlier_files(out, names):
    return [file for file in ('index.json', *(f'{name}.json' for name in names)) if (out/file).exists()]


def export(args, index, popen, fetch, clock, sleep, wall, port):
    """Waits for the tail, then runs every query; fills index and returns the empty ones."""
    end = args.end + timedelta(seconds=args.tail_seconds)
    wait = (end - wall()).total_seconds()
    if wait > 0:
        print(f'Waiting {math.ceil(wait)} s until {kube_run.iso(end)} so that the tail samples exist')
        sleep(math.ceil(wait))
    selected = [(name, query) for name, query in queries(args.namespace, args.release) if name not in args.skip]
    empty = []
    with port_forward(args, port, popen, fetch, clock, sleep) as base:
        for name, query in selected:
            body, series = query_range(fetch, base, name, query, args.start, end, args.step)
            (args.out_dir/f'{name}.json').write_bytes(body)
            entry = {'name': name, 'query': query, 'file': f'{name}.json', 'series': len(series),
                     'samples': sum(len(item.get('values') or []) for item in series), 'empty': not series}
            index['queries'].append(entry)
            if not series:
                empty.append(name)
            print(f"{name}: {entry['series']} series, {entry['samples']} samples" + (' EMPTY' if not series else ''))
    index['exported_at'] = kube_run.iso(wall())
    return empty


def main(argv=None, popen=subprocess.Popen, fetch=fetch, clock=time.monotonic, sleep=time.sleep,
         wall=kube_run.utc_now, port=None):
    args = parse_args(argv)
    names = [name for name in NAMES if name not in args.skip]
    present = earlier_files(args.out_dir, names)
    if present and not args.overwrite:
        print(f'FAIL: {args.out_dir} already has {", ".join(present)}; use --overwrite or another --out-dir',
              file=sys.stderr)
        return 1
    end = args.end + timedelta(seconds=args.tail_seconds)
    index = {'run': args.run, 'source': 'record' if args.record else 'arguments', 'context': args.kube_context,
             'prometheus': {'namespace': args.monitoring_namespace, 'service': args.service, 'port': PORT},
             'namespace': args.namespace, 'release': args.release,
             'window': {'start': kube_run.iso(args.start), 'end': kube_run.iso(args.end),
                        'tail_seconds': args.tail_seconds, 'query_end': kube_run.iso(end), 'step': args.step},
             'queries': [], 'skipped': list(args.skip), 'empty': [], 'exported_at': None, 'error': None}
    code = 0
    try:
        args.out_dir.mkdir(parents=True, exist_ok=True)
        index['empty'] = export(args, index, popen, fetch, clock, sleep, wall, port or free_port())
        code = 2 if index['empty'] else 0
    except ExportError as error:
        index['error'], code = str(error), 1
    except OSError as error:
        index['error'], code = kube_run.error_text(error), 1
    except KeyboardInterrupt:
        index['error'], code = INTERRUPTED, 130
    paths = deploy.private_paths(args.kubeconfig)
    if index['error']:
        index['error'] = deploy.scrub(index['error'], paths)
    with contextlib.suppress(OSError):
        (args.out_dir/'index.json').write_text(json.dumps(index, indent=2) + '\n', encoding='utf-8', newline='\n')
    if index['error']:
        print(f"FAIL Prometheus export: {index['error']}", file=sys.stderr)
    elif index['empty']:
        print(f"FAIL Prometheus export: no series for {', '.join(index['empty'])}", file=sys.stderr)
    else:
        print(f"Exported {len(index['queries'])} queries to {args.out_dir}")
    return code


if __name__ == '__main__': sys.exit(main())
