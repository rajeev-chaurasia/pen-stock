// calibrate_load.js
//
// A deliberately minimal k6 script used only by
// bench/compare/calibrate-litellm.sh to find the worker count LiteLLM
// is FASTEST at on this machine.
//
// It is not a benchmark and its numbers are not published. It exists so
// that the --num_workers value in bench/compare/start-litellm.sh is a
// measured choice rather than a guess, because "we picked a bad worker
// count" is the most obvious way a comparison like this gets rigged,
// and the cheapest way to answer that objection is to sweep it and
// commit the sweep.
//
// Same executor as the real scenario, for the same reason: an open
// model, so a slower configuration is not quietly handed less work.

import http from 'k6/http';
import { check } from 'k6';
import { Trend } from 'k6/metrics';

const URL = __ENV.TARGET_URL || 'http://127.0.0.1:8081';
const MODEL = __ENV.MODEL || 'llmsim-small';
const KEY = __ENV.API_KEY || 'penstock-bench-key-0123456789abcdef';

const lat = new Trend('cal_latency', true);

export const options = {
  scenarios: {
    cal: {
      executor: 'constant-arrival-rate',
      rate: parseInt(__ENV.RATE || '20', 10),
      timeUnit: '1s',
      duration: __ENV.DURATION || '20s',
      preAllocatedVUs: parseInt(__ENV.PRE_ALLOCATED_VUS || '50', 10),
      maxVUs: parseInt(__ENV.MAX_VUS || '200', 10),
      gracefulStop: '30s',
    },
  },
  summaryTrendStats: ['avg', 'p(50)', 'p(95)', 'p(99)', 'count'],
};

export default function () {
  const body = JSON.stringify({
    model: MODEL,
    messages: [{ role: 'user', content: 'calibrate vu=' + __VU + ' iter=' + __ITER }],
    stream: false,
  });
  const res = http.post(URL + '/v1/chat/completions', body, {
    headers: { 'Content-Type': 'application/json', Authorization: 'Bearer ' + KEY },
  });
  check(res, { 'status 200': (r) => r.status === 200 });
  lat.add(res.timings.duration);
}

export function handleSummary(data) {
  const m = data.metrics.cal_latency;
  const v = m && m.values ? m.values : {};
  const dropped = data.metrics.dropped_iterations
    ? data.metrics.dropped_iterations.values.count : 0;
  const failed = data.metrics.http_req_failed
    ? data.metrics.http_req_failed.values.rate : 0;
  const num = (x) => (x === undefined ? 'n/a' : x.toFixed(2));
  // One machine readable line, because the sweep script parses it.
  const line = 'CALIBRATION' +
    ' workers=' + (__ENV.LABEL || '?') +
    ' p50=' + num(v['p(50)']) +
    ' p95=' + num(v['p(95)']) +
    ' p99=' + num(v['p(99)']) +
    ' mean=' + num(v.avg) +
    ' samples=' + (v.count === undefined ? 0 : v.count) +
    ' dropped=' + dropped +
    ' failed_rate=' + failed.toFixed(4);
  return { stdout: '\n' + line + '\n' };
}
