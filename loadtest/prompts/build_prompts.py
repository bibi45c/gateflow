"""Build realmodel.jsonl: the benchmark prompts of the real-model tests.

Downloads three MIT-licensed benchmarks at pinned revisions, checks their SHA-256
where the source is a file, takes a seeded sample of each and writes one JSON object
per line:

  GSM8K      200 of the 1,319 test problems, openai/grade-school-math
  MMLU       200 of the 14,042 test questions, Hugging Face cais/mmlu (config all)
  HumanEval  all 164 problems, openai/human-eval

Each line has id, set, prompt (the user message sent to the model), answer (the
reference: a number, a letter, or null for HumanEval) and, for HumanEval, the
entry point, the original code prompt and the tests. SOURCES.md records what this
script pins. Rebuilding gives byte-identical output; the committed file is the one
the tests use, so this script is only needed to reproduce or change it.

Standard library only; needs network access:

  python loadtest/prompts/build_prompts.py
"""
import argparse
import gzip
import hashlib
import json
from pathlib import Path
import random
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

HERE = Path(__file__).resolve().parent
OUT = HERE/'realmodel.jsonl'
SEED = 5001
SAMPLE = 200
GSM8K_URL = ('https://raw.githubusercontent.com/openai/grade-school-math/'
             '3101c7d5072418e28b9008a6636bde82a006892c/grade_school_math/data/test.jsonl')
GSM8K_SHA256 = '3730d312f6e3440559ace48831e51066acaca737f6eabec99bccb9e4b3c39d14'
HUMANEVAL_URL = ('https://raw.githubusercontent.com/openai/human-eval/'
                 'b9453e8f1865df0a0057a1383767caa400acfbcf/data/HumanEval.jsonl.gz')
HUMANEVAL_SHA256 = 'b796127e635a67f93fb35c04f4cb03cf06f38c8072ee7cee8833d7bee06979ef'
MMLU_DATASET = 'cais/mmlu'
MMLU_REVISION = 'c30699e8356da336a370243923dbaf21066bb9fe'
MMLU_ROWS = 14042
PAGE = 100  # the most rows the datasets server returns per call
LETTERS = 'ABCD'

GSM8K_INSTRUCTION = ('Solve the following math problem. Reason briefly, then give the final answer '
                     'on the last line in the form "Answer: <number>".\n\n')
MMLU_INSTRUCTION = 'Answer the following multiple-choice question with the letter of the correct option only.\n\n'
HUMANEVAL_INSTRUCTION = ('Complete the following Python function. Reply with the complete function '
                         'in a single ```python code block and nothing else.\n\n')


def fetch(url, attempts=8):
    """The body of url; waits and retries while the server answers 429 or 5xx."""
    request = urllib.request.Request(url, headers={'User-Agent': 'gateflow-build-prompts'})
    for attempt in range(attempts):
        try:
            with urllib.request.urlopen(request, timeout=60) as response:
                return response.read()
        except urllib.error.HTTPError as error:
            if attempt == attempts - 1 or not (error.code == 429 or error.code >= 500):
                raise
            delay = error.headers.get('Retry-After', '')
            time.sleep(float(delay) if delay.isdigit() else 2 ** attempt)


def checked(url, sha256):
    data = fetch(url)
    digest = hashlib.sha256(data).hexdigest()
    if digest != sha256:
        raise SystemExit(f'{url}: SHA-256 {digest}, expected {sha256}')
    return data


def sample(total, count):
    return sorted(random.Random(SEED).sample(range(total), min(count, total)))


def gsm8k_answer(solution):
    """The number after the #### line of a GSM8K solution, without thousands separators."""
    return solution.rsplit('####', 1)[1].strip().replace(',', '')


def gsm8k(data):
    rows = [json.loads(line) for line in data.decode('utf-8').splitlines() if line.strip()]
    for i in sample(len(rows), SAMPLE):
        yield {'id': f'gsm8k-{i:04d}', 'set': 'gsm8k', 'prompt': GSM8K_INSTRUCTION + rows[i]['question'],
               'answer': gsm8k_answer(rows[i]['answer'])}


def mmlu_revision():
    info = json.loads(fetch(f'https://huggingface.co/api/datasets/{MMLU_DATASET}'))
    if info.get('sha') != MMLU_REVISION:
        raise SystemExit(f'{MMLU_DATASET} is at revision {info.get("sha")}, not the pinned {MMLU_REVISION}; '
                         'the datasets server only serves the latest revision, so review the change first')


def mmlu_rows(indices):
    pages = {}
    for page in sorted({i // PAGE for i in indices}):
        query = urllib.parse.urlencode({'dataset': MMLU_DATASET, 'config': 'all', 'split': 'test',
                                        'offset': page * PAGE, 'length': PAGE})
        body = json.loads(fetch(f'https://datasets-server.huggingface.co/rows?{query}'))
        if body.get('num_rows_total') != MMLU_ROWS:
            raise SystemExit(f'{MMLU_DATASET} has {body.get("num_rows_total")} test rows, expected {MMLU_ROWS}')
        pages.update({item['row_idx']: item['row'] for item in body['rows']})
        time.sleep(0.5)  # the datasets server limits the request rate
    return [(i, pages[i]) for i in indices]


def mmlu():
    mmlu_revision()
    for i, row in mmlu_rows(sample(MMLU_ROWS, SAMPLE)):
        options = '\n'.join(f'{letter}. {text}' for letter, text in zip(LETTERS, row['choices']))
        subject = row['subject'].replace('_', ' ')
        yield {'id': f'mmlu-{i:05d}', 'set': 'mmlu', 'subject': row['subject'],
               'prompt': f'{MMLU_INSTRUCTION}Subject: {subject}\n\n{row["question"]}\n\n{options}\n\nAnswer:',
               'answer': LETTERS[row['answer']]}


def humaneval(data):
    rows = [json.loads(line) for line in gzip.decompress(data).decode('utf-8').splitlines() if line.strip()]
    for row in rows:
        number = int(row['task_id'].split('/')[1])
        yield {'id': f'humaneval-{number:03d}', 'set': 'humaneval', 'task_id': row['task_id'],
               'prompt': HUMANEVAL_INSTRUCTION + '```python\n' + row['prompt'] + '```', 'answer': None,
               'entry_point': row['entry_point'], 'code_prompt': row['prompt'], 'test': row['test']}


def build():
    items = [*gsm8k(checked(GSM8K_URL, GSM8K_SHA256)), *mmlu(), *humaneval(checked(HUMANEVAL_URL, HUMANEVAL_SHA256))]
    return ''.join(json.dumps(item, ensure_ascii=False, sort_keys=True) + '\n' for item in items)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('--out', type=Path, default=OUT)
    args = parser.parse_args(argv)
    text = build()
    args.out.write_bytes(text.encode('utf-8'))
    print(f'{args.out.name}: {text.count(chr(10))} items, SHA-256 {hashlib.sha256(text.encode("utf-8")).hexdigest()}')
    return 0


if __name__ == '__main__':
    sys.exit(main())
