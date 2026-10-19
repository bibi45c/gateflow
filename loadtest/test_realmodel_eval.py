"""Unit tests of realmodel_eval.py, against fake streaming upstreams on localhost ports."""
import contextlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import io
import json
from pathlib import Path
import sys
import tempfile
import threading
import unittest

import realmodel_eval as ev

ITEMS = [
    {'id': 'gsm8k-0001', 'set': 'gsm8k', 'prompt': 'two plus two', 'answer': '4'},
    {'id': 'mmlu-00001', 'set': 'mmlu', 'prompt': 'pick B', 'answer': 'B'},
    {'id': 'humaneval-000', 'set': 'humaneval', 'prompt': 'write add', 'answer': None, 'entry_point': 'add',
     'code_prompt': 'import math\n\ndef add(a, b):\n    """Sum."""\n',
     'test': 'def check(candidate):\n    assert candidate(2, 3) == 5\n'},
]
REPLIES = {
    'two plus two': 'Two and two make four.\nAnswer: 4',
    'pick B': 'B',
    'write add': '```python\ndef add(a, b):\n    return a + b\n```',
}


class Upstream:
    """A fake model server. replies maps a prompt to its text; cut lists prompts whose stream stops early."""

    def __init__(self, replies, cut=(), status=200, chunk=5, request_ids=False):
        self.replies, self.cut, self.status, self.chunk = dict(replies), set(cut), status, chunk
        self.request_ids = request_ids
        self.requests = []
        owner = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def do_POST(self):
                body = json.loads(self.rfile.read(int(self.headers['content-length'])))
                owner.requests.append((self.path, dict(self.headers), body))
                if owner.status != 200:
                    self.send_response(owner.status)
                    self.end_headers()
                    self.wfile.write(b'{"type":"error"}')
                    return
                self.send_response(200)
                if owner.request_ids:
                    self.send_header('request-id', f'req_{len(owner.requests)}')
                self.send_header('content-type', 'text/event-stream')
                self.end_headers()
                prompt = body['messages'][0]['content'] if self.path == '/v1/messages' else body['input']
                text = owner.replies[prompt]
                parts = [text[i:i+owner.chunk] for i in range(0, len(text), owner.chunk)]
                events = owner.messages(parts) if self.path == '/v1/messages' else owner.responses(parts)
                if prompt in owner.cut:
                    events = events[:-1]
                for name, data in events:
                    self.wfile.write(f'event: {name}\ndata: {json.dumps(data)}\n\n'.encode('utf-8'))
                    self.wfile.flush()

        self.server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        self.url = f'http://127.0.0.1:{self.server.server_port}'
        threading.Thread(target=self.server.serve_forever, daemon=True).start()

    @staticmethod
    def messages(parts):
        return [('message_start', {'type': 'message_start', 'message': {'usage': {'input_tokens': 7, 'output_tokens': 0}}}),
                ('content_block_start', {'type': 'content_block_start', 'index': 0}),
                *[('content_block_delta', {'type': 'content_block_delta', 'delta': {'type': 'text_delta', 'text': p}})
                  for p in parts],
                ('content_block_stop', {'type': 'content_block_stop', 'index': 0}),
                ('message_delta', {'type': 'message_delta', 'delta': {'stop_reason': 'end_turn'},
                                   'usage': {'input_tokens': 7, 'output_tokens': len(parts)}}),
                ('message_stop', {'type': 'message_stop'})]

    @staticmethod
    def responses(parts):
        return [('response.created', {'type': 'response.created', 'response': {'status': 'in_progress'}}),
                *[('response.output_text.delta', {'type': 'response.output_text.delta', 'delta': p}) for p in parts],
                ('response.completed', {'type': 'response.completed', 'response': {
                    'status': 'completed', 'usage': {'input_tokens': 7, 'output_tokens': len(parts)}}})]

    def close(self):
        self.server.shutdown()
        self.server.server_close()


class Case(unittest.TestCase):
    def setUp(self):
        self.folder = Path(tempfile.mkdtemp(prefix='gateflow-eval-test-'))
        self.addCleanup(lambda: __import__('shutil').rmtree(self.folder, ignore_errors=True))
        self.prompts = self.folder/'prompts.jsonl'
        self.prompts.write_text(''.join(json.dumps(i) + '\n' for i in ITEMS), encoding='utf-8')

    def upstream(self, replies=REPLIES, **kwargs):
        server = Upstream(replies, **kwargs)
        self.addCleanup(server.close)
        return server

    def run_eval(self, direct, gateway, *extra):
        out = self.folder/'out'
        argv = ['run', '--direct-url', direct.url, '--gateway-url', gateway.url, '--model', 'qwen3-0.6b',
                '--out-dir', str(out), '--prompts', str(self.prompts), '--concurrency', '2', '--timeout', '10', *extra]
        with contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
            code = ev.main(argv)
        summary = json.loads((out/'summary.json').read_text(encoding='utf-8')) if (out/'summary.json').exists() else None
        return code, summary, out


class RunTest(Case):
    def test_identical_streams_pass_and_score(self):
        direct, gateway = self.upstream(), self.upstream()
        code, summary, out = self.run_eval(direct, gateway)
        self.assertEqual(code, 0)
        self.assertTrue(summary['pass'])
        self.assertEqual(summary['comparisons']['direct_vs_gateway'], {'compared': 3, 'identical': 3, 'usage_differs': 0,
                                                                       'mismatches': []})
        for name in ('direct', 'gateway'):
            sets = summary['passes'][name]['sets']
            self.assertEqual({s: sets[s]['accuracy'] for s in sets}, {'gsm8k': 1.0, 'mmlu': 1.0, 'humaneval': 1.0})
        answers = [json.loads(l) for l in (out/'answers-gateway.jsonl').read_text(encoding='utf-8').splitlines()]
        self.assertEqual([a['text'] for a in answers], [REPLIES[i['prompt']] for i in ITEMS])
        self.assertEqual(answers[0]['input_tokens'], 7)
        self.assertTrue((out/'summary.md').read_text(encoding='utf-8').count('PASS'))

    def test_requests_carry_temperature_zero_and_the_gateway_key(self):
        direct, gateway = self.upstream(), self.upstream()
        self.run_eval(direct, gateway)
        path, headers, body = gateway.requests[0]
        self.assertEqual(path, '/v1/messages')
        self.assertEqual((body['temperature'], body['stream'], body['max_tokens'], body['model']), (0, True, 256, 'qwen3-0.6b'))
        self.assertEqual(headers['x-api-key'], ev.derive_key(ev.DEFAULT_SEED_SECRET, 'tenant-01'))
        self.assertNotIn('x-api-key', {k.lower() for k in direct.requests[0][1]})

    def test_a_differing_answer_fails_with_its_offset(self):
        changed = dict(REPLIES, **{'two plus two': 'Two and two make five.\nAnswer: 5'})
        code, summary, _ = self.run_eval(self.upstream(), self.upstream(changed))
        self.assertEqual(code, 1)
        comparison = summary['comparisons']['direct_vs_gateway']
        self.assertEqual(comparison['identical'], 2)
        self.assertEqual(comparison['mismatches'][0]['id'], 'gsm8k-0001')
        self.assertEqual(comparison['mismatches'][0]['byte_offset'], len('Two and two make f'))
        self.assertEqual(summary['passes']['gateway']['sets']['gsm8k']['accuracy'], 0.0)

    def test_a_stream_without_its_end_is_failed_not_a_mismatch(self):
        code, summary, _ = self.run_eval(self.upstream(), self.upstream(cut={'pick B'}))
        self.assertEqual(code, 1)
        self.assertEqual(summary['passes']['gateway']['not_completed'], 1)
        self.assertEqual(summary['comparisons']['direct_vs_gateway']['compared'], 2)
        self.assertEqual(summary['comparisons']['direct_vs_gateway']['mismatches'], [])
        self.assertIn('end event', summary['passes']['gateway']['errors'][0]['error'])

    def test_an_error_status_is_recorded(self):
        code, summary, _ = self.run_eval(self.upstream(), self.upstream(status=503))
        self.assertEqual(code, 1)
        self.assertEqual(summary['passes']['gateway']['errors'][0]['http_status'], 503)

    def test_responses_protocol_and_control_pass(self):
        direct, gateway = self.upstream(), self.upstream()
        code, summary, out = self.run_eval(direct, gateway, '--protocol', 'responses', '--control')
        self.assertEqual(code, 0)
        self.assertEqual(gateway.requests[0][0], '/v1/responses')
        self.assertEqual(gateway.requests[0][2]['max_output_tokens'], 256)
        self.assertEqual(summary['comparisons']['direct_vs_direct_repeat']['identical'], 3)
        self.assertTrue((out/'answers-direct_repeat.jsonl').exists())
        self.assertEqual(len(direct.requests), 6)

    def test_unusable_prompts_exit_2(self):
        self.prompts.write_text('{"id": "x", "set": "gsm8k", "prompt": "p"}\nnot json\n', encoding='utf-8')
        code, summary, _ = self.run_eval(self.upstream(), self.upstream())
        self.assertEqual((code, summary), (2, None))

    def test_committed_prompts_load_with_their_references(self):
        items = ev.load_prompts(ev.PROMPTS, list(ev.SETS), None)
        counts = {s: sum(1 for i in items if i['set'] == s) for s in ev.SETS}
        self.assertEqual(counts, {'gsm8k': 200, 'mmlu': 200, 'humaneval': 164})
        self.assertTrue(all(i['answer'] in 'ABCD' for i in items if i['set'] == 'mmlu'))
        self.assertTrue(all(ev.number_value(i['answer']) is not None for i in items if i['set'] == 'gsm8k'))
        self.assertTrue(all(i['test'] and i['code_prompt'] for i in items if i['set'] == 'humaneval'))
        # The reference solutions of HumanEval pass its lite check once they are the reply.
        item = next(i for i in items if i['set'] == 'humaneval')
        self.assertEqual(ev.score(item, {'status': 'completed', 'text': item['code_prompt'] + '    pass\n'}), ('code', True))

    def test_sets_and_limit_select_items(self):
        items = ev.load_prompts(self.prompts, ['mmlu', 'humaneval'], 1)
        self.assertEqual([i['id'] for i in items], ['mmlu-00001', 'humaneval-000'])


class ReconcileTest(Case):
    LEDGER = 'ledger_entry_id,reservation_id,call_id,attempt_id,tenant_id,amount,input_tokens,output_tokens,outcome,created_at\n'
    HOLDS = 'reservation_id,call_id,status,input_price,output_price\n'

    def bill(self, out, amount=None, held=False, outcome='completed', drop=None):
        answers = [json.loads(l) for l in (out/'answers-gateway.jsonl').read_text(encoding='utf-8').splitlines()]
        ledger, holds = [self.LEDGER], [self.HOLDS]
        for i, a in enumerate(answers):
            if a['id'] == drop:
                continue
            charge = 3 * a['input_tokens'] + 15 * a['output_tokens'] if amount is None else amount
            ledger.append(f"le{i},r{i},{a['request_id']},1,tenant-01,{charge},{a['input_tokens']},{a['output_tokens']},{outcome},t\n")
            holds.append(f"r{i},{a['request_id']},settled,3,15\n")
        ledger.append('le9,r9,req_other,1,tenant-02,0,1,1,completed,t\n')
        holds.append(f"r9,req_other,{'reserved' if held else 'settled'},3,15\n")
        (self.folder/'ledger_entries.csv').write_text(''.join(ledger), encoding='utf-8')
        (self.folder/'reservations.csv').write_text(''.join(holds), encoding='utf-8')
        output = io.StringIO()
        with contextlib.redirect_stdout(output), contextlib.redirect_stderr(io.StringIO()):
            code = ev.main(['reconcile', '--answers', str(out/'answers-gateway.jsonl'),
                            '--ledger', str(self.folder/'ledger_entries.csv'),
                            '--reservations', str(self.folder/'reservations.csv')])
        return code, output.getvalue()

    def gateway_run(self):
        code, summary, out = self.run_eval(self.upstream(), self.upstream(request_ids=True))
        self.assertEqual(code, 0)
        self.assertEqual(summary['passes']['gateway']['missing_usage'], 0)
        return out

    def test_matching_bill_passes(self):
        code, text = self.bill(self.gateway_run())
        self.assertEqual(code, 0, text)
        self.assertIn('"reconciled": 3', text)
        self.assertIn('"other_ledger_entries": 1', text)

    def test_wrong_amount_missing_entry_bad_outcome_and_held_reservation_fail(self):
        out = self.gateway_run()
        for kwargs, expected in (({'amount': 1}, 'is not the price'), ({'drop': 'mmlu-00001'}, '0 ledger entries'),
                                 ({'outcome': 'upstream_error'}, 'outcome upstream_error'),
                                 ({'held': True}, 'still reserved')):
            with self.subTest(**kwargs):
                code, text = self.bill(out, **kwargs)
                self.assertEqual(code, 1)
                self.assertIn(expected, text)

    def test_direct_answers_cannot_be_reconciled(self):
        code, summary, out = self.run_eval(self.upstream(), self.upstream())
        (self.folder/'ledger_entries.csv').write_text(self.LEDGER, encoding='utf-8')
        (self.folder/'reservations.csv').write_text(self.HOLDS, encoding='utf-8')
        with contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
            self.assertEqual(ev.main(['reconcile', '--answers', str(out/'answers-direct.jsonl'), '--ledger',
                                      str(self.folder/'ledger_entries.csv'), '--reservations',
                                      str(self.folder/'reservations.csv')]), 2)


class ScoreTest(unittest.TestCase):
    def test_gsm8k(self):
        cases = {'So 3 + 4 = 7.\nAnswer: 7': 7, 'Answer: **1,234**': 1234, 'Answer: $18.00': 18,
                 'It costs 5 then 12': 12, 'no number here': None, 'Answer: -3.5': -3.5}
        for text, expected in cases.items():
            self.assertEqual(ev.gsm8k_prediction(text), expected, text)

    def test_mmlu(self):
        cases = {'B': 'B', 'C.': 'C', '(D) Paris': 'D', '**A**': 'A', 'The answer is C.': 'C',
                 'A cell is the unit of life, so B': 'B', 'either A or D': None, 'none of them': None}
        for text, expected in cases.items():
            self.assertEqual(ev.mmlu_prediction(text), expected, text)

    def test_humaneval_lite(self):
        item = ITEMS[2]
        record = {'status': 'completed', 'text': REPLIES['write add']}
        self.assertEqual(ev.score(item, record), ('code', True))
        self.assertEqual(ev.score(item, dict(record, text='I cannot help.')), (None, False))
        self.assertEqual(ev.score(item, dict(record, text='```python\ndef add(a, b) return\n```')), (None, False))
        self.assertEqual(ev.score(item, dict(record, text='```python\ndef other():\n    pass\n```')), (None, False))
        self.assertEqual(ev.score(item, {'status': 'failed', 'text': ''}), (None, None))

    def test_humaneval_execution(self):
        item = ITEMS[2]
        self.assertEqual(ev.score(item, {'status': 'completed', 'text': REPLIES['write add']}, execute=True), ('code', True))
        wrong = '```python\ndef add(a, b):\n    return a - b\n```'
        self.assertEqual(ev.score(item, {'status': 'completed', 'text': wrong}, execute=True), ('code', False))

    def test_key_matches_the_seed(self):
        # Same derivation as messages.js and access seed: HMAC-SHA256, base64url without padding.
        self.assertRegex(ev.derive_key('s', 'tenant-01'), r'^gf_[A-Za-z0-9_-]{43}$')


class PodTest(unittest.TestCase):
    def test_pod_runs_on_the_generator_with_the_seed_secret(self):
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            self.assertEqual(ev.main(['pod', '--name', 't5']), 0)
        document = json.loads(out.getvalue())
        spec = document['spec']
        self.assertEqual(document['metadata']['name'], 'gateflow-eval-t5')
        self.assertEqual(spec['nodeSelector'], {'gateflow.io/workload': 'load-generator'})
        self.assertIn('@sha256:', spec['containers'][0]['image'])
        self.assertEqual(spec['containers'][0]['env'][0]['valueFrom']['secretKeyRef'],
                         {'name': 'gateflow-runtime', 'key': 'ACCESS_SEED_SECRET'})
        self.assertFalse(spec['automountServiceAccountToken'])

    def test_bad_name_is_refused(self):
        with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
            ev.main(['pod', '--name', 'Bad_Name'])

    def test_bad_url_is_refused(self):
        with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
            ev.main(['run', '--direct-url', 'ftp://x', '--gateway-url', 'http://g', '--model', 'm', '--out-dir', 'o'])


if __name__ == '__main__':
    sys.exit(unittest.main())
