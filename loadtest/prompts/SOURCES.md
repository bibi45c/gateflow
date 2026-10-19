# Benchmark prompts

`realmodel.jsonl` holds the prompts of the real-model tests (issue
[#145](https://github.com/bibi45c/gateflow/issues/145)): 564 items, one JSON object
per line, SHA-256 `bd68144eadb41d2f3fbe2b4a7b12ac2c5bb6b7c64846a0123ae17439d1d0d698`.
`build_prompts.py` rebuilds it byte for byte from the sources below:

```text
python loadtest/prompts/build_prompts.py
```

| Set | Items | Source | Version | License |
|---|---|---|---|---|
| `gsm8k` | 200 of the 1,319 test problems | [openai/grade-school-math](https://github.com/openai/grade-school-math), `grade_school_math/data/test.jsonl` | commit `3101c7d5072418e28b9008a6636bde82a006892c`, file SHA-256 `3730d312…3c39d14` | MIT |
| `mmlu` | 200 of the 14,042 test questions (config `all`), 48 subjects | [cais/mmlu](https://huggingface.co/datasets/cais/mmlu) on Hugging Face, through its datasets server | revision `c30699e8356da336a370243923dbaf21066bb9fe` | MIT |
| `humaneval` | all 164 problems | [openai/human-eval](https://github.com/openai/human-eval), `data/HumanEval.jsonl.gz` | commit `b9453e8f1865df0a0057a1383767caa400acfbcf`, file SHA-256 `b796127e…6979ef` | MIT |

The GSM8K and MMLU items are a seeded sample (`random.Random(5001)`, kept in source
order). HumanEval has only 164 problems, so all are used. The script refuses a file
whose SHA-256 differs, and refuses MMLU if the dataset has moved past the pinned
revision, because the datasets server serves only the latest one.

Each item has `id`, `set`, `prompt` (the user message: an instruction plus the
problem), `answer` (the GSM8K number, the MMLU letter, or null) and, for HumanEval,
`task_id`, `entry_point`, `code_prompt` and `test`. The instructions ask for
`Answer: <number>`, a single letter, and one fenced Python block, so that
`loadtest/realmodel_eval.py` can score the replies of a small model.

## Notices

The three works are distributed under the MIT License. Their copyright notices:

- GSM8K: Copyright (c) 2021 OpenAI
- HumanEval: Copyright (c) OpenAI (https://openai.com)
- MMLU: Copyright (c) 2020 Dan Hendrycks

Permission is hereby granted, free of charge, to any person obtaining a copy of this
software and associated documentation files (the "Software"), to deal in the Software
without restriction, including without limitation the rights to use, copy, modify,
merge, publish, distribute, sublicense, and/or sell copies of the Software, and to
permit persons to whom the Software is furnished to do so, subject to the following
conditions:

The above copyright notice and this permission notice shall be included in all copies
or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR IMPLIED,
INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY, FITNESS FOR A
PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE AUTHORS OR COPYRIGHT
HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER LIABILITY, WHETHER IN AN ACTION OF
CONTRACT, TORT OR OTHERWISE, ARISING FROM, OUT OF OR IN CONNECTION WITH THE SOFTWARE
OR THE USE OR OTHER DEALINGS IN THE SOFTWARE.
