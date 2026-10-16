// Scheduled load on POST /v1/messages, after proposal section 5.6. See README.md.
//
// Requests arrive on schedule whether or not earlier replies have finished: a warm-up
// at the first level, then for each level a ramp (except the first) and a hold. Every
// request streams, carries an explicit max_tokens and uses the seeded tenants in
// turn. PAIRED=1 is a separate latency diagnostic: each request also goes straight to
// the mock, doubling its load. http_req_waiting measures TTFB, not the first model
// content delta required by proposal section 6.2. Each request is tagged with its
// phase, level, tenant and path, and setup() writes the plan into the output, so that
// summarize.py can build the results tables from the JSON output alone.
import http from 'k6/http';
import crypto from 'k6/crypto';
import encoding from 'k6/encoding';
import exec from 'k6/execution';
import { Counter, Trend } from 'k6/metrics';

const env = (name, fallback) => __ENV[name] || fallback;
const number = (name, fallback) => Number(env(name, fallback));

const BASE_URL = env('GATEWAY_URL', 'http://127.0.0.1:8080');
const MOCK_URL = env('MOCK_URL', 'http://127.0.0.1:8090');
const PAIRED = env('PAIRED', '0') === '1';
// mock-load streams MOCK_LOAD_EVENTS deltas (200 by default), one output token each;
// max_tokens covers them, so each call is charged its real usage, not its hold.
const MODEL = env('MODEL', 'mock-load');
const MAX_TOKENS = number('MAX_TOKENS', '256');
const INPUT_BYTES = number('INPUT_BYTES', '1024');
const TENANTS = number('TENANTS', '10');
// TENANTS=1 sends every request with tenant-01's key: the shared-wallet scenario.
if (!Number.isInteger(TENANTS) || TENANTS < 1) {
  throw new Error('TENANTS must be a whole number of at least 1');
}
const SEED_SECRET = env('ACCESS_SEED_SECRET', 'seed-secret-local-only');
const LEVELS = env('LEVELS', '5,25,50').split(',').map(Number);
const WARMUP_S = number('WARMUP_S', '60');
const RAMP_S = number('RAMP_S', '60');
const HOLD_S = number('HOLD_S', '180');
const TTFB_DEADLINE_MS = number('TTFB_DEADLINE_MS', '10000');
const TOTAL_DEADLINE_S = number('TOTAL_DEADLINE_S', '60');
if (!Number.isFinite(TOTAL_DEADLINE_S) || TOTAL_DEADLINE_S <= 0) {
  throw new Error('TOTAL_DEADLINE_S must be a positive number');
}
if (PAIRED && env('MOCK_FIRST_EVENT_DELAY_MS', '') !== '0') {
  throw new Error('PAIRED=1 requires MOCK_FIRST_EVENT_DELAY_MS=0 on both the mock and k6');
}
const MAX_VUS = number('MAX_VUS', '4000');
// Each request holds a VU for its whole stream, so the VUs in flight are the rate
// times the seconds a request lasts. Allocating them before the run keeps k6 from
// dropping arrivals while it starts more.
const STREAM_S = number('STREAM_S', '25');

// phases returns the run's plan as [phase, level, start, end], in seconds from the
// start of the scenario.
export function phases(levels, warmup, ramp, hold) {
  const plan = [['warmup', levels[0], 0, warmup]];
  let t = warmup;
  levels.forEach((level, i) => {
    if (i > 0) {
      plan.push(['ramp', level, t, t + ramp]);
      t += ramp;
    }
    plan.push(['hold', level, t, t + hold]);
    t += hold;
  });
  return plan;
}

const PLAN = phases(LEVELS, WARMUP_S, RAMP_S, HOLD_S);

export const options = {
  scenarios: {
    messages: {
      executor: 'ramping-arrival-rate',
      startRate: LEVELS[0],
      timeUnit: '1s',
      preAllocatedVUs: Math.min(MAX_VUS, Math.ceil(Math.max(...LEVELS) * STREAM_S)),
      maxVUs: MAX_VUS,
      // Let the last request reach its deadline and publish a terminal result.
      gracefulStop: `${TOTAL_DEADLINE_S + 5}s`,
      stages: PLAN.map(([, level, start, end]) => ({ target: level, duration: `${end - start}s` })),
    },
  },
};

const planned = new Counter('gateflow_plan');
// One point when the first iteration starts: the time zero of the plan, which
// summarize.py needs to place dropped iterations and the vus gauge in a phase.
const started = new Counter('gateflow_start');
// Count before starting HTTP, so an interrupted iteration cannot disappear merely
// because k6 never returned its response to the script.
const sent = new Counter('gateflow_sent');
const results = {
  completed: new Counter('gateflow_completed'),
  rejected: new Counter('gateflow_rejected'),
  timed_out: new Counter('gateflow_timed_out'),
  failed: new Counter('gateflow_failed'),
};
const added = new Trend('gateflow_added_ttfb_ms');
const pairFailed = new Counter('gateflow_pair_failed');

// deriveKey returns the seeded gateway key of tenant, as `access seed` derives it.
export function deriveKey(secret, tenant) {
  return 'gf_' + encoding.b64encode(crypto.hmac('sha256', secret, tenant, 'binary'), 'rawurl');
}

const KEYS = Array.from({ length: TENANTS }, (_, i) => {
  const tenant = `tenant-${String(i + 1).padStart(2, '0')}`;
  return [tenant, deriveKey(SEED_SECRET, tenant)];
});

const BODY = JSON.stringify({
  model: MODEL,
  max_tokens: MAX_TOKENS,
  stream: true,
  messages: [{ role: 'user', content: 'x'.repeat(INPUT_BYTES) }],
});
const HEADERS = { 'content-type': 'application/json', 'anthropic-version': '2023-06-01' };

// classify names the result of one request. A request completes only when its first
// byte came within the deadline and its stream ended with message_stop and no error
// event. 429, 503 and 529 are rejections; any other answer is a failure.
export function classify(status, errorCode, waitingMs, body) {
  if (errorCode === 1050) return 'timed_out';
  if (status === 429 || status === 503 || status === 529) return 'rejected';
  if (status !== 200) return 'failed';
  if (waitingMs > TTFB_DEADLINE_MS) return 'timed_out';
  const text = body || '';
  if (text.includes('event: error')) return 'failed';
  return text.includes('event: message_stop') ? 'completed' : 'failed';
}

function phaseAt(elapsed) {
  const found = PLAN.find(([, , , end]) => elapsed < end) || PLAN[PLAN.length - 1];
  return [found[0], found[1]];
}

export function setup() {
  for (const [phase, level, start, end] of PLAN) {
    planned.add(1, { phase, level: String(level), start_s: String(start), end_s: String(end) });
  }
}

function request(url, headers, tags) {
  return ['POST', url, BODY, { headers, timeout: `${TOTAL_DEADLINE_S}s`, tags }];
}

export default function () {
  if (exec.scenario.iterationInTest === 0) started.add(1);
  const [tenant, key] = KEYS[exec.scenario.iterationInTest % KEYS.length];
  const [phase, level] = phaseAt((Date.now() - exec.scenario.startTime) / 1000);
  const tags = { phase, level: String(level), tenant };
  const gateway = request(`${BASE_URL}/v1/messages`, { ...HEADERS, 'x-api-key': key }, { ...tags, path: 'gateway' });
  sent.add(1, tags);
  const [res, direct] = PAIRED
    ? http.batch([gateway, request(`${MOCK_URL}/v1/messages`, HEADERS, { ...tags, path: 'direct' })])
    : [http.request(...gateway), null];
  const result = classify(res.status, res.error_code, res.timings.waiting, res.body);
  results[result].add(1, { ...tags, status: String(res.status) });
  if (!direct) return;
  if (result === 'completed' && classify(direct.status, direct.error_code, direct.timings.waiting, direct.body) === 'completed') {
    added.add(res.timings.waiting - direct.timings.waiting, tags);
  } else {
    pairFailed.add(1, tags);
  }
}
