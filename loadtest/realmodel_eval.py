"""Check that the gateway leaves a real model's answers unchanged (test T5), and score them.

Sends every prompt of a JSONL file (loadtest/prompts/realmodel.jsonl) as a streamed
call at temperature 0, first straight to the model server and then through the
gateway, with --concurrency calls in flight. Each pass saves every answer with its
timings and usage. The answers are then compared byte for byte, and scored:

  gsm8k      the final number ("Answer: <number>", else the last number) equals the reference
  mmlu       the single choice letter equals the reference
  humaneval  lite: the reply has Python code that parses and defines the entry point;
             with --exec-humaneval, the official tests also pass (see the warning below)

Greedy decoding is not batch-invariant: under concurrency the model server may give a
different answer to the same prompt when the batch around it differs. --control runs
the direct pass a second time, so that rate of direct-vs-direct differences, the
model's own noise floor, can be told apart from a change made by the gateway. Run
with --concurrency 1 for the strict check, where batching cannot differ.

Writes answers-<pass>.jsonl, summary.json and summary.md to --out-dir. Exit code 0:
every call completed on every pass and the direct and gateway answers are identical;
1: a call did not complete or an answer differs; 2: unusable input.

--exec-humaneval runs code written by the model in a subprocess with a timeout. That
is not a security sandbox: use it only in a disposable Pod (see the pod command).

  python loadtest/realmodel_eval.py run --direct-url http://<model host>:8000 --gateway-url http://gf-gateway:8080 --model qwen3-0.6b --out-dir /tmp/t5
  python loadtest/realmodel_eval.py pod --name t5 --out .local/realmodel/pod-t5.json
  python loadtest/realmodel_eval.py reconcile --answers <out-dir>/answers-gateway.jsonl --ledger <reset evidence>/ledger_entries.csv --reservations <reset evidence>/reservations.csv

reconcile checks the bill of every completed gateway call against the CSVs that
deploy/aws/reset_db.py exports before it empties the accounting tables.

Standard library only.
"""
import argparse
import ast
import base64
import csv
from concurrent.futures import ThreadPoolExecutor
import hashlib
import hmac
import http.client
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import time
import urllib.parse

HERE = Path(__file__).resolve().parent
PROMPTS = HERE/'prompts'/'realmodel.jsonl'
SETS = ('gsm8k', 'mmlu', 'humaneval')
PROTOCOLS = ('messages', 'responses')
DEFAULT_SEED_SECRET = 'seed-secret-local-only'  # the compose default, as messages.js
PYTHON_IMAGE = 'python:3.13-slim@sha256:70729b46c69b4f1e97c4822c1af3df53a1476cf5ddc6c087c0c10bc3a5678c2f'
WORKLOAD = {'gateflow.io/workload': 'load-generator'}
SECRET, SEED_KEY = 'gateflow-runtime', 'ACCESS_SEED_SECRET'
NAME = re.compile(r'[a-z0-9]([-a-z0-9]*[a-z0-9])?')
SNIPPET = 60  # characters shown around a first difference


class InputError(Exception):
    pass


def derive_key(secret, tenant):
    """The seeded gateway key of tenant, as `access seed` derives it."""
    digest = hmac.new(secret.encode('utf-8'), tenant.encode('utf-8'), hashlib.sha256).digest()
    return 'gf_' + base64.urlsafe_b64encode(digest).rstrip(b'=').decode('ascii')


def load_prompts(path, sets, limit):
    """The items of path in file order, limited to sets and to the first limit of each set."""
    items, ids, taken = [], set(), {}
    try:
        lines = path.read_text(encoding='utf-8').splitlines()
    except OSError as error:
        raise InputError(f'cannot read {path}: {error.strerror}') from None
    for number, line in enumerate(lines, 1):
        if not line.strip():
            continue
        try:
            item = json.loads(line)
        except json.JSONDecodeError as error:
            raise InputError(f'{path.name} line {number}: not JSON: {error.msg}') from None
        if not isinstance(item, dict) or not all(isinstance(item.get(k), str) and item[k] for k in ('id', 'set', 'prompt')):
            raise InputError(f'{path.name} line {number}: needs non-empty string id, set and prompt')
        if item['set'] not in SETS:
            raise InputError(f'{path.name} line {number}: unknown set {item["set"]!r}')
        if item['set'] == 'humaneval' and not isinstance(item.get('entry_point'), str):
            raise InputError(f'{path.name} line {number}: a humaneval item needs entry_point')
        if item['id'] in ids:
            raise InputError(f'{path.name} line {number}: id {item["id"]} repeats')
        ids.add(item['id'])
        if item['set'] in sets and (limit is None or taken.get(item['set'], 0) < limit):
            taken[item['set']] = taken.get(item['set'], 0) + 1
            items.append(item)
    if not items:
        raise InputError(f'{path.name} has no items of {", ".join(sets)}')
    return items


# Requests and streams.

def request_body(protocol, model, prompt, max_tokens):
    if protocol == 'messages':
        return {'model': model, 'max_tokens': max_tokens, 'temperature': 0, 'stream': True,
                'messages': [{'role': 'user', 'content': prompt}]}
    return {'model': model, 'max_output_tokens': max_tokens, 'temperature': 0, 'stream': True, 'store': False,
            'input': prompt}


def request_headers(protocol, key, bearer):
    headers = {'content-type': 'application/json', 'accept': 'text/event-stream'}
    if protocol == 'messages':
        headers['anthropic-version'] = '2023-06-01'
    if key:
        headers['x-api-key'] = key
    if bearer:
        headers['authorization'] = f'Bearer {bearer}'
    return headers


def sse_events(lines):
    """(event, data) pairs of an SSE stream given as an iterator of byte lines."""
    event, data = None, []
    for raw in lines:
        line = raw.decode('utf-8').rstrip('\r\n')
        if not line:
            if data:
                yield event, '\n'.join(data)
            event, data = None, []
        elif line.startswith(':'):
            continue
        elif line.startswith('event:'):
            event = line[6:].strip()
        elif line.startswith('data:'):
            data.append(line[5:].lstrip(' '))
    if data:
        yield event, '\n'.join(data)


class Stream:
    """What one streamed reply said: its text, usage, end and first output time."""

    def __init__(self):
        self.text, self.thinking = [], []
        self.status, self.error, self.stop_reason = 'failed', 'stream ended without its end event', None
        self.input_tokens = self.output_tokens = None
        self.first_output = None
        self.output_times = []

    def output(self, text, clock, into=None):
        if text:
            now = clock()
            self.first_output = now if self.first_output is None else self.first_output
            self.output_times.append(now)
        (self.text if into is None else into).append(text)


def read_messages(events, stream, clock):
    for event, data in events:
        if data == '[DONE]':
            continue
        payload = json.loads(data)
        kind = payload.get('type') or event
        if kind == 'message_start':
            stream.input_tokens = ((payload.get('message') or {}).get('usage') or {}).get('input_tokens')
        elif kind == 'content_block_delta':
            delta = payload.get('delta') or {}
            if delta.get('type') == 'text_delta':
                stream.output(delta.get('text', ''), clock)
            elif delta.get('type') == 'thinking_delta':
                stream.output(delta.get('thinking', ''), clock, stream.thinking)
        elif kind == 'message_delta':
            usage = payload.get('usage') or {}
            stream.output_tokens = usage.get('output_tokens', stream.output_tokens)
            if usage.get('input_tokens'):
                stream.input_tokens = usage['input_tokens']
            stream.stop_reason = (payload.get('delta') or {}).get('stop_reason', stream.stop_reason)
        elif kind == 'error':
            stream.status, stream.error = 'failed', json.dumps(payload.get('error') or payload)[:300]
            return
        elif kind == 'message_stop':
            stream.status, stream.error = 'completed', None
            return


def read_responses(events, stream, clock):
    for event, data in events:
        if data == '[DONE]':
            continue
        payload = json.loads(data)
        kind = payload.get('type') or event
        if kind == 'response.output_text.delta':
            stream.output(payload.get('delta', ''), clock)
        elif kind in ('response.reasoning_text.delta', 'response.reasoning.delta'):
            stream.output(payload.get('delta', ''), clock, stream.thinking)
        elif kind in ('response.completed', 'response.incomplete', 'response.failed'):
            response = payload.get('response') or {}
            usage = response.get('usage') or {}
            stream.input_tokens, stream.output_tokens = usage.get('input_tokens'), usage.get('output_tokens')
            stream.stop_reason = (response.get('incomplete_details') or {}).get('reason') or response.get('status')
            if kind == 'response.completed':
                stream.status, stream.error = 'completed', None
            else:
                stream.status, stream.error = 'failed', f'{kind}: {json.dumps(response.get("error"))[:200]}'
            return
        elif kind == 'error':
            stream.status, stream.error = 'failed', json.dumps(payload)[:300]
            return


def connect(url, timeout):
    parts = urllib.parse.urlsplit(url)
    kind = http.client.HTTPSConnection if parts.scheme == 'https' else http.client.HTTPConnection
    return kind(parts.hostname, parts.port, timeout=timeout), parts.path.rstrip('/')


def call(target, protocol, item, settings, clock=time.monotonic):
    """One streamed call of item to target; returns its record."""
    body = json.dumps(request_body(protocol, settings.model, item['prompt'], settings.max_tokens)).encode('utf-8')
    started = clock()
    record = {'id': item['id'], 'set': item['set'], 'status': 'failed', 'http_status': None, 'error': None,
              'text': '', 'ttfb_ms': None, 'first_output_ms': None, 'duration_ms': None,
              'input_tokens': None, 'output_tokens': None, 'stop_reason': None, 'request_id': None}
    stream = Stream()
    connection = None
    try:
        connection, prefix = connect(target['url'], settings.timeout)
        connection.request('POST', f'{prefix}/v1/{protocol}', body,
                           request_headers(protocol, target.get('key'), target.get('bearer')))
        response = connection.getresponse()
        record['ttfb_ms'] = round((clock() - started) * 1000, 1)
        record['http_status'] = response.status
        # The gateway's call ID, which metering records as call_id; a model server sends none.
        record['request_id'] = response.getheader('request-id')
        if response.status != 200:
            stream.error = response.read(500).decode('utf-8', 'replace')
        else:
            reader = read_messages if protocol == 'messages' else read_responses
            reader(sse_events(iter(response.readline, b'')), stream, clock)
    except (OSError, http.client.HTTPException, ValueError) as error:
        stream.error = f'{type(error).__name__}: {error}'
    finally:
        if connection is not None:
            connection.close()
    record.update(status=stream.status, error=stream.error, text=''.join(stream.text),
                  input_tokens=stream.input_tokens, output_tokens=stream.output_tokens, stop_reason=stream.stop_reason,
                  duration_ms=round((clock() - started) * 1000, 1))
    if stream.thinking:
        record['thinking'] = ''.join(stream.thinking)
    if stream.first_output is not None:
        record['first_output_ms'] = round((stream.first_output - started) * 1000, 1)
    if settings.record_deltas:
        # Arrival of each output delta, in ms from the request: the token gaps of test T3.
        record['delta_ms'] = [round((t - started) * 1000, 1) for t in stream.output_times]
    return record


def run_pass(target, protocol, items, settings):
    with ThreadPoolExecutor(max_workers=settings.concurrency) as pool:
        return list(pool.map(lambda item: call(target, protocol, item, settings), items))


# Scoring.

NUMBER = r'-?\$?\d[\d,]*(?:\.\d+)?'


def number_value(text):
    cleaned = text.replace('$', '').replace(',', '')
    try:
        value = float(cleaned)
    except ValueError:
        return None
    return int(value) if value == int(value) else value


def gsm8k_prediction(text):
    """The number after the last "Answer:", else the last number of text, or None."""
    marked = re.findall(r'answer\s*[:：]\s*\**\s*(' + NUMBER + ')', text, flags=re.IGNORECASE)
    found = marked or re.findall(NUMBER, text)
    return number_value(found[-1]) if found else None


def mmlu_prediction(text):
    """The choice letter of text: a leading letter, an "answer is X", else a lone letter; None if unclear."""
    stripped = text.strip()
    # A leading letter, but not the article of a sentence such as "A cell is ...".
    lead = re.match(r'^\(?\**([ABCD])\**\)?(?:[.:)]|\s*$|\s*\n|\s+(?![a-z]))', stripped)
    if lead:
        return lead.group(1)
    stated = re.findall(r'answer(?:\s+is)?\s*[:：]?\s*\(?\**([ABCD])\b', stripped, flags=re.IGNORECASE)
    if stated:
        return stated[-1].upper()
    # Leave out the article that starts a sentence: "A cell is ...".
    words = re.sub(r'(?:^|(?<=[.!?]\s))A(?=\s+[a-z])', '', stripped)
    letters = set(re.findall(r'\b([ABCD])\b', words))
    return letters.pop() if len(letters) == 1 else None


def extract_code(text):
    """The first ```python (or bare ```) block of text, else text itself if it has a def; or None."""
    block = re.search(r'```(?:python|py)?[ \t]*\n(.*?)```', text, flags=re.DOTALL)
    if block:
        return block.group(1)
    return text if re.search(r'^\s*def ', text, flags=re.MULTILINE) else None


def defines(code, name):
    try:
        tree = ast.parse(code)
    except (SyntaxError, ValueError):
        return False
    return any(isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)) and node.name == name for node in ast.walk(tree))


def humaneval_program(item, code):
    """The completion with the imports of the original prompt, its tests and the call of check."""
    imports = [line for line in item.get('code_prompt', '').splitlines() if line.startswith(('import ', 'from '))]
    return '\n'.join([*imports, code, item.get('test', ''), f'check({item["entry_point"]})', ''])


def run_humaneval(item, code, timeout=10):
    """True if the official tests pass in a subprocess. Not a sandbox."""
    with tempfile.TemporaryDirectory(prefix='gateflow-humaneval-') as folder:
        program = Path(folder)/'program.py'
        program.write_text(humaneval_program(item, code), encoding='utf-8')
        try:
            result = subprocess.run([sys.executable, '-I', str(program)], cwd=folder, capture_output=True,
                                    timeout=timeout, env={'PATH': os.environ.get('PATH', '')})
        except subprocess.TimeoutExpired:
            return False
    return result.returncode == 0


def score(item, record, execute=False):
    """(prediction, correct) of record; correct is None when the call did not complete."""
    if record['status'] != 'completed':
        return None, None
    text = record['text']
    if item['set'] == 'gsm8k':
        prediction = gsm8k_prediction(text)
        return prediction, prediction is not None and prediction == number_value(item['answer'])
    if item['set'] == 'mmlu':
        prediction = mmlu_prediction(text)
        return prediction, prediction == item['answer']
    code = extract_code(text)
    lite = code is not None and defines(code, item['entry_point'])
    if not execute:
        return ('code' if lite else None), lite
    return ('code' if lite else None), lite and run_humaneval(item, code)


# Comparison and summary.

def first_difference(a, b):
    a_bytes, b_bytes = a.encode('utf-8'), b.encode('utf-8')
    for offset, (x, y) in enumerate(zip(a_bytes, b_bytes)):
        if x != y:
            return offset
    return min(len(a_bytes), len(b_bytes))


def compare(left, right):
    """Byte comparison of two passes over the items both completed."""
    pairs = [(a, b) for a, b in zip(left, right) if a['status'] == 'completed' and b['status'] == 'completed']
    mismatches = []
    for a, b in pairs:
        if a['text'] != b['text']:
            offset = first_difference(a['text'], b['text'])
            mismatches.append({'id': a['id'], 'byte_offset': offset,
                               'left': a['text'].encode('utf-8')[max(0, offset-SNIPPET//2):offset+SNIPPET].decode('utf-8', 'replace'),
                               'right': b['text'].encode('utf-8')[max(0, offset-SNIPPET//2):offset+SNIPPET].decode('utf-8', 'replace'),
                               'output_tokens': [a['output_tokens'], b['output_tokens']]})
    usage = sum(1 for a, b in pairs if (a['input_tokens'], a['output_tokens']) != (b['input_tokens'], b['output_tokens']))
    return {'compared': len(pairs), 'identical': len(pairs) - len(mismatches), 'usage_differs': usage,
            'mismatches': mismatches}


def percentile(values, q):
    values = sorted(v for v in values if v is not None)
    if not values:
        return None
    return values[min(len(values) - 1, max(0, round(q / 100 * len(values) + 0.5) - 1))]


def pass_summary(items, records, execute):
    sets = {}
    for item, record in zip(items, records):
        prediction, correct = score(item, record, execute)
        record['prediction'], record['correct'] = prediction, correct
        entry = sets.setdefault(item['set'], {'items': 0, 'completed': 0, 'correct': 0})
        entry['items'] += 1
        entry['completed'] += record['status'] == 'completed'
        entry['correct'] += bool(correct)
    for entry in sets.values():
        entry['accuracy'] = round(entry['correct'] / entry['items'], 4)
    done = [r for r in records if r['status'] == 'completed']
    return {'items': len(records), 'completed': len(done), 'not_completed': len(records) - len(done),
            'with_thinking': sum(1 for r in records if r.get('thinking')),
            'missing_usage': sum(1 for r in done if r['input_tokens'] is None or r['output_tokens'] is None),
            'ttfb_ms': {f'p{q}': percentile([r['ttfb_ms'] for r in done], q) for q in (50, 95, 99)},
            'first_output_ms': {f'p{q}': percentile([r['first_output_ms'] for r in done], q) for q in (50, 95, 99)},
            'duration_ms': {f'p{q}': percentile([r['duration_ms'] for r in done], q) for q in (50, 95, 99)},
            'output_tokens': {f'p{q}': percentile([r['output_tokens'] for r in done], q) for q in (50, 95, 99)},
            'stop_reasons': dict(sorted(_count(r['stop_reason'] for r in done).items())),
            'errors': [{'id': r['id'], 'http_status': r['http_status'], 'error': r['error']}
                       for r in records if r['status'] != 'completed'][:20],
            'sets': dict(sorted(sets.items()))}


def _count(values):
    counts = {}
    for value in values:
        counts[str(value)] = counts.get(str(value), 0) + 1
    return counts


def verdict(summary):
    """Every call completed, and, when both passes ran, every gateway answer equals the direct one."""
    complete = all(p['not_completed'] == 0 for p in summary['passes'].values())
    gate = summary['comparisons'].get('direct_vs_gateway')
    if gate is None:
        return complete
    return complete and gate['compared'] == summary['items'] and gate['identical'] == gate['compared']


def markdown(summary):
    lines = [f"# Real-model answer check ({summary['protocol']})", '',
             f"Model `{summary['model']}`, {summary['items']} items, concurrency {summary['concurrency']}, "
             f"max tokens {summary['max_tokens']}, temperature 0. Verdict: **{'PASS' if summary['pass'] else 'FAIL'}**.", '',
             '| Pass | Completed | Not completed | Missing usage | TTFB p50 / p95 (ms) | Duration p50 / p95 (ms) | Output tokens p50 / p95 |'
             + ''.join(f' {name} accuracy |' for name in summary['sets']),
             '|---|---|---|---|---|---|---|' + '---|' * len(summary['sets'])]
    for name, p in summary['passes'].items():
        accuracy = ''.join(f" {p['sets'].get(s, {}).get('accuracy', '-')} |" for s in summary['sets'])
        lines.append(f"| {name} | {p['completed']} | {p['not_completed']} | {p['missing_usage']} | {p['ttfb_ms']['p50']} / {p['ttfb_ms']['p95']} | "
                     f"{p['duration_ms']['p50']} / {p['duration_ms']['p95']} | "
                     f"{p['output_tokens']['p50']} / {p['output_tokens']['p95']} |{accuracy}")
    lines += ['', '| Comparison | Compared | Identical | Usage differs |', '|---|---|---|---|']
    for name, c in summary['comparisons'].items():
        lines.append(f"| {name} | {c['compared']} | {c['identical']} | {c['usage_differs']} |")
    for name, c in summary['comparisons'].items():
        if c['mismatches']:
            lines += ['', f'First differences, {name} (up to 10):', '']
            lines += [f"- `{m['id']}` at byte {m['byte_offset']}: {json.dumps(m['left'])} vs {json.dumps(m['right'])}"
                      for m in c['mismatches'][:10]]
    lines += ['', 'HumanEval is scored ' + ('by running its tests.' if summary['exec_humaneval']
                                             else 'lite: the code parses and defines the entry point.')]
    return '\n'.join(lines) + '\n'


def write_jsonl(path, records):
    path.write_text(''.join(json.dumps(r, ensure_ascii=False, sort_keys=True) + '\n' for r in records), encoding='utf-8')


def run(args):
    items = load_prompts(args.prompts, args.sets, args.limit)
    secret = os.environ.get(SEED_KEY, DEFAULT_SEED_SECRET)
    direct = {'url': args.direct_url, 'bearer': os.environ.get(args.direct_api_key_env) if args.direct_api_key_env else None}
    gateway = {'url': args.gateway_url, 'key': derive_key(secret, args.tenant)}
    order = [(name, {'direct': direct, 'gateway': gateway}[name]) for name in args.passes]
    if args.control:
        order.append(('direct_repeat', direct))
    args.out_dir.mkdir(parents=True, exist_ok=True)
    records = {}
    for name, target in order:
        started = time.monotonic()
        records[name] = run_pass(target, args.protocol, items, args)
        print(f'{name}: {sum(r["status"] == "completed" for r in records[name])}/{len(items)} completed '
              f'in {time.monotonic() - started:.0f} s', file=sys.stderr)
    summary = {'protocol': args.protocol, 'model': args.model, 'items': len(items), 'concurrency': args.concurrency,
               'max_tokens': args.max_tokens, 'exec_humaneval': args.exec_humaneval, 'tenant': args.tenant,
               'prompts_sha256': hashlib.sha256(args.prompts.read_bytes()).hexdigest(),
               'sets': sorted({i['set'] for i in items}, key=SETS.index),
               'passes': {name: pass_summary(items, recs, args.exec_humaneval) for name, recs in records.items()},
               'comparisons': {}}
    if 'direct' in records and 'gateway' in records:
        summary['comparisons']['direct_vs_gateway'] = compare(records['direct'], records['gateway'])
    if args.control:
        summary['comparisons']['direct_vs_direct_repeat'] = compare(records['direct'], records['direct_repeat'])
    summary['pass'] = verdict(summary)
    for name, recs in records.items():
        write_jsonl(args.out_dir/f'answers-{name}.jsonl', recs)
    (args.out_dir/'summary.json').write_text(json.dumps(summary, indent=2, ensure_ascii=False) + '\n', encoding='utf-8')
    (args.out_dir/'summary.md').write_text(markdown(summary), encoding='utf-8')
    print(markdown(summary))
    return 0 if summary['pass'] else 1


def read_csv(path):
    try:
        with path.open(encoding='utf-8', newline='') as handle:
            return list(csv.DictReader(handle))
    except OSError as error:
        raise InputError(f'cannot read {path}: {error.strerror}') from None


def whole(value):
    return None if value in (None, '') else int(value)


def reconcile_calls(calls, ledger, reservations):
    """Problems found matching gateway calls to the metering ledger, and the counts checked.

    Every completed call must have exactly one ledger entry with its call_id, outcome
    completed, the token counts the client saw and an amount of input_price x input
    tokens + output_price x output tokens from its reservation. No reservation may stay
    reserved. Ledger entries of other calls are counted, not judged.
    """
    by_call = {}
    for entry in ledger:
        by_call.setdefault(entry['call_id'], []).append(entry)
    holds = {r['reservation_id']: r for r in reservations}
    problems = []
    checked = 0
    for call in calls:
        if call['status'] != 'completed':
            continue
        entries = by_call.get(call.get('request_id') or '', [])
        if len(entries) != 1:
            problems.append(f"{call['id']}: {len(entries)} ledger entries for call {call.get('request_id')}")
            continue
        entry = entries[0]
        tokens = (whole(entry['input_tokens']), whole(entry['output_tokens']))
        hold = holds.get(entry['reservation_id'])
        if entry['outcome'] != 'completed':
            problems.append(f"{call['id']}: ledger outcome {entry['outcome']}")
        elif tokens != (call['input_tokens'], call['output_tokens']):
            problems.append(f"{call['id']}: ledger tokens {tokens}, client saw {(call['input_tokens'], call['output_tokens'])}")
        elif hold is None:
            problems.append(f"{call['id']}: no reservation {entry['reservation_id']}")
        elif int(entry['amount']) != int(hold['input_price']) * tokens[0] + int(hold['output_price']) * tokens[1]:
            problems.append(f"{call['id']}: amount {entry['amount']} is not the price times the tokens")
        else:
            checked += 1
    for entry in ledger:
        hold = holds.get(entry['reservation_id'])
        tokens = (whole(entry['input_tokens']) or 0, whole(entry['output_tokens']) or 0)
        if hold is None:
            problems.append(f"ledger entry {entry['ledger_entry_id']}: no reservation {entry['reservation_id']}")
        elif int(entry['amount']) != int(hold['input_price']) * tokens[0] + int(hold['output_price']) * tokens[1]:
            problems.append(f"ledger entry {entry['ledger_entry_id']}: amount {entry['amount']} is not the price times the tokens")
    held = [r['reservation_id'] for r in reservations if r['status'] == 'reserved']
    if held:
        problems.append(f'{len(held)} reservations still reserved, first {held[0]}')
    ours = {c.get('request_id') for c in calls}
    return problems, {'calls_completed': sum(1 for c in calls if c['status'] == 'completed'), 'reconciled': checked,
                      'ledger_entries': len(ledger), 'other_ledger_entries': sum(1 for e in ledger if e['call_id'] not in ours),
                      'reservations_reserved': len(held),
                      'outcomes': dict(sorted(_count(e['outcome'] for e in ledger).items()))}


def reconcile(args):
    calls = []
    for path in args.answers or []:
        try:
            calls += [json.loads(line) for line in path.read_text(encoding='utf-8').splitlines() if line.strip()]
        except (OSError, json.JSONDecodeError) as error:
            raise InputError(f'cannot read {path}: {error}') from None
    if args.answers and not any(c.get('request_id') for c in calls):
        raise InputError('no call has a request_id: pass the answers-gateway.jsonl of a gateway pass')
    problems, counts = reconcile_calls(calls, read_csv(args.ledger), read_csv(args.reservations))
    print(json.dumps(counts))
    for problem in problems[:50]:
        print(f'MISMATCH {problem}')
    print('PASS billing matches the calls' if not problems else f'FAIL {len(problems)} problems')
    return 0 if not problems else 1


def pod(args):
    name = f'gateflow-eval-{args.name}'
    container = {'name': 'eval', 'image': args.image, 'command': ['sleep', str(args.lifetime)],
                 'env': [{'name': SEED_KEY, 'valueFrom': {'secretKeyRef': {'name': SECRET, 'key': SEED_KEY}}}],
                 'resources': {'requests': {'cpu': '1', 'memory': '1Gi'}, 'limits': {'cpu': '1', 'memory': '1Gi'}},
                 'securityContext': {'allowPrivilegeEscalation': False, 'capabilities': {'drop': ['ALL']}},
                 'workingDir': '/tmp'}
    document = {'apiVersion': 'v1', 'kind': 'Pod',
                'metadata': {'name': name, 'labels': {'app.kubernetes.io/name': 'realmodel-eval',
                                                      'app.kubernetes.io/component': 'load-generator'}},
                'spec': {'restartPolicy': 'Never', 'automountServiceAccountToken': False,
                         'activeDeadlineSeconds': args.lifetime, 'terminationGracePeriodSeconds': 5,
                         'securityContext': {'runAsNonRoot': True, 'runAsUser': 65534, 'runAsGroup': 65534,
                                             'seccompProfile': {'type': 'RuntimeDefault'}},
                         'nodeSelector': dict(WORKLOAD),
                         'tolerations': [{'key': k, 'operator': 'Equal', 'value': v, 'effect': 'NoSchedule'}
                                         for k, v in WORKLOAD.items()],
                         'containers': [container]}}
    text = json.dumps(document, indent=2) + '\n'
    if args.out:
        args.out.parent.mkdir(parents=True, exist_ok=True)
        args.out.write_text(text, encoding='utf-8')
        print(f'wrote {args.out}: Pod {name}', file=sys.stderr)
    else:
        sys.stdout.write(text)
    return 0


def positive(text):
    try:
        number = int(text)
    except ValueError:
        raise argparse.ArgumentTypeError(f'expected a whole number, got {text!r}') from None
    if number < 1:
        raise argparse.ArgumentTypeError(f'must be at least 1, got {number}')
    return number


def pass_list(text):
    passes = [p for p in text.split(',') if p]
    if not passes or any(p not in ('direct', 'gateway') for p in passes) or len(set(passes)) != len(passes):
        raise argparse.ArgumentTypeError('passes are direct, gateway or both, comma-separated')
    return passes


def set_list(text):
    sets = [s for s in text.split(',') if s]
    unknown = [s for s in sets if s not in SETS]
    if unknown or not sets:
        raise argparse.ArgumentTypeError(f'sets are a comma list of {", ".join(SETS)}')
    return sets


def parse_args(argv):
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    commands = parser.add_subparsers(dest='command', required=True)
    r = commands.add_parser('run', help='send every prompt directly and through the gateway, compare and score')
    r.add_argument('--direct-url', required=True, help='model server base URL, such as http://10.40.2.15:8000')
    r.add_argument('--gateway-url', required=True, help='gateway base URL, such as http://gf-gateway:8080')
    r.add_argument('--model', required=True, help="the model name both sides serve (vLLM's --served-model-name)")
    r.add_argument('--out-dir', type=Path, required=True)
    r.add_argument('--prompts', type=Path, default=PROMPTS)
    r.add_argument('--protocol', choices=PROTOCOLS, default='messages')
    r.add_argument('--concurrency', type=positive, default=32)
    r.add_argument('--max-tokens', type=positive, default=256)
    r.add_argument('--timeout', type=positive, default=120, help='seconds a call may wait for data (default: 120)')
    r.add_argument('--sets', type=set_list, default=list(SETS), help='comma list (default: all three)')
    r.add_argument('--limit', type=positive, help='first N items of each set only')
    r.add_argument('--tenant', default='tenant-01', help=f'seeded tenant whose key is derived from {SEED_KEY}')
    r.add_argument('--direct-api-key-env', metavar='NAME', help='variable holding a key vLLM checks as a bearer token')
    r.add_argument('--passes', type=pass_list, default=['direct', 'gateway'],
                   help='comma list of direct and gateway, in order (default: direct,gateway); one alone is not compared')
    r.add_argument('--record-deltas', action='store_true', help='keep the arrival time of every output delta (T3)')
    r.add_argument('--control', action='store_true', help='repeat the direct pass to measure the noise floor')
    r.add_argument('--exec-humaneval', action='store_true', help='run the HumanEval tests; disposable Pod only')
    c = commands.add_parser('reconcile', help='match gateway calls to the ledger and reservations CSVs of reset_db.py')
    c.add_argument('--answers', type=Path, action='append', metavar='JSONL',
                   help='answers-gateway.jsonl of a run (repeatable); without it only the ledger itself is checked')
    c.add_argument('--ledger', type=Path, required=True, help='ledger_entries.csv')
    c.add_argument('--reservations', type=Path, required=True, help='reservations.csv')
    p = commands.add_parser('pod', help='write a Pod manifest that runs this script on the load-generator node')
    p.add_argument('--name', required=True, help='suffix of the Pod name, lowercase letters, digits and dashes')
    p.add_argument('--out', type=Path, help='file to write (default: standard output)')
    p.add_argument('--image', default=PYTHON_IMAGE)
    p.add_argument('--lifetime', type=positive, default=14400, help='seconds before the Pod stops (default: 4 h)')
    args = parser.parse_args(argv)
    if args.command == 'run':
        if args.control and 'direct' not in args.passes:
            parser.error('--control repeats the direct pass, so --passes must include direct')
        for flag, url in (('--direct-url', args.direct_url), ('--gateway-url', args.gateway_url)):
            parts = urllib.parse.urlsplit(url)
            if parts.scheme not in ('http', 'https') or not parts.hostname:
                parser.error(f'{flag} must be an http(s) base URL, got {url!r}')
    if args.command == 'pod' and (not NAME.fullmatch(args.name) or len(args.name) > 40):
        parser.error('--name must be lowercase letters, digits and dashes, at most 40 characters')
    return args


def main(argv=None):
    args = parse_args(argv)
    try:
        return {'run': run, 'reconcile': reconcile, 'pod': pod}[args.command](args)
    except InputError as error:
        print(f'ERROR {error}', file=sys.stderr)
        return 2


if __name__ == '__main__':
    sys.exit(main())
