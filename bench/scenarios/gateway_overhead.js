// gateway_overhead.js
//
// The question this scenario exists to answer: how much latency does
// Penstock itself add to a request?
//
// WHY IT IS BUILT AS A DIFFERENCE
//
// You cannot answer that by timing requests through the gateway. That
// number is dominated by the upstream, and publishing it as "gateway
// latency" is the exact dishonesty this harness exists to avoid: it
// reports the speed of whatever was standing in for a provider. The
// only honest way to state gateway overhead is as a difference between
// two runs that are identical except for the gateway being in the path:
//
//   arm A   k6 -> llmsim
//   arm B   k6 -> penstock -> llmsim
//
// Everything below is in service of making those two arms comparable.
//
// WHY THE ARMS RUN ONE AFTER THE OTHER, NOT TOGETHER
//
// Running them concurrently would be better for cancelling out machine
// drift, but arm B's traffic also lands on llmsim, so a concurrent run
// would put twice the load on the simulator during the overlap and the
// arms would not be seeing the same upstream. Sequential arms keep the
// upstream's load identical; the cost is that the machine may drift
// between them, which is what the third arm below is for.
//
// WHY THERE IS A SECOND DIRECT ARM
//
// direct_b repeats arm A after arm B. If the two direct measurements
// disagree, the machine changed underneath the run (thermal throttling,
// a background process, a laptop switching power profiles) and the
// delta from arm B is not attributable to the gateway. It is cheaper to
// detect that than to argue about it later. Set DRIFT_CHECK=false to
// skip it and take a third off the wall clock.
//
// WHY TWO SEPARATE llmsim PROCESSES
//
// bench/run.sh starts two simulators with the same --seed: one behind
// the gateway, one for the direct arm. llmsim draws each request's
// simulated latency from the request's index and the seed, so request i
// in arm A and request i in arm B are served with the identical planned
// TTFT, inter-token latency and token count. The upstream therefore
// contributes the same latency distribution to both arms rather than
// two independent draws from it, and the difference between the arms is
// much closer to being only the gateway.
//
// WHY constant-arrival-rate AND NOT ramping-vus
//
// An open model. A closed model (constant-vus, ramping-vus) sends its
// next request only after the previous one comes back, so a slower
// system automatically receives less load. That is coordinated
// omission, and here it would actively hide the thing being measured:
// the gateway arm would quietly be given an easier workload precisely
// because it is slower. constant-arrival-rate offers both arms the same
// requests per second no matter how either one responds. When k6 cannot
// keep up it reports dropped_iterations, which the summary treats as
// invalidating the run rather than as a footnote.

import http from 'k6/http';
import { check } from 'k6';
import { Trend, Counter } from 'k6/metrics';
import {
  CFG, CHAT_PATH, TREND_STATS, RULE,
  chatBody, uniquePrompt, gatewayHeaders, directHeaders,
  envBool, toSeconds, stat, count, ms, pad, padLeft,
  header, loadWarnings, summaryOutputs,
} from './lib/common.js';

const driftCheck = envBool('DRIFT_CHECK', true);

const armSeconds = toSeconds(CFG.duration);
const gapSeconds = toSeconds(CFG.gap);
const armB = armSeconds + gapSeconds;
const armC = 2 * (armSeconds + gapSeconds);

const directLatency = new Trend('direct_latency', true);
const gatewayLatency = new Trend('gateway_latency', true);
const driftLatency = new Trend('direct_recheck_latency', true);
const shed = new Counter('gateway_shed_503');

function armScenario(exec, startTime) {
  return {
    executor: 'constant-arrival-rate',
    rate: CFG.rate,
    timeUnit: '1s',
    duration: CFG.duration,
    preAllocatedVUs: CFG.preAllocatedVUs,
    maxVUs: CFG.maxVUs,
    gracefulStop: CFG.gracefulStop,
    startTime: startTime + 's',
    exec: exec,
  };
}

const scenarios = {
  direct_a: armScenario('directArm', 0),
  via_gateway: armScenario('gatewayArm', armB),
};
if (driftCheck) {
  scenarios.direct_b = armScenario('driftArm', armC);
}

export const options = {
  scenarios: scenarios,
  summaryTrendStats: TREND_STATS,
  thresholds: {
    // A dropped iteration means the requested arrival rate was not
    // actually delivered, so the two arms were not given the same
    // workload. That does not degrade the result, it voids it.
    dropped_iterations: ['count == 0'],
    http_req_failed: ['rate == 0'],
  },
};

function post(url, headers, body, trend) {
  const res = http.post(url + CHAT_PATH, body, {
    headers: headers,
    // Tagging by arm keeps the raw JSON samples separable after the
    // fact, so a reader can recompute any statistic they like instead
    // of trusting the summary at the bottom of this file.
    tags: { arm: trend },
  });
  if (res.status === 503) {
    shed.add(1);
  }
  check(res, {
    'status 200': (r) => r.status === 200,
    'body is a completion': (r) =>
      typeof r.body === 'string' && r.body.indexOf('chat.completion') !== -1,
  });
  return res;
}

export function directArm() {
  const body = chatBody(CFG.directModel, uniquePrompt('overhead'), { stream: false });
  const res = post(CFG.directURL, directHeaders(), body, 'direct');
  directLatency.add(res.timings.duration);
}

export function gatewayArm() {
  const body = chatBody(CFG.model, uniquePrompt('overhead'), { stream: false });
  const res = post(CFG.baseURL, gatewayHeaders(), body, 'gateway');
  gatewayLatency.add(res.timings.duration);
}

export function driftArm() {
  const body = chatBody(CFG.directModel, uniquePrompt('overhead'), { stream: false });
  const res = post(CFG.directURL, directHeaders(), body, 'drift');
  driftLatency.add(res.timings.duration);
}

function row(label, d, g) {
  const delta = d === null || g === null ? null : g - d;
  return pad(label, 10) + padLeft(ms(d), 12) + padLeft(ms(g), 14) + padLeft(ms(delta), 14);
}

export function handleSummary(data) {
  const lines = [];
  lines.push(header('Penstock gateway overhead', {
    'arrival rate': CFG.rate + ' req/s per arm',
    'drift check': driftCheck ? 'on' : 'off',
  }));
  lines.push(loadWarnings(data));

  const dAvg = stat(data, 'direct_latency', 'avg');
  const gAvg = stat(data, 'gateway_latency', 'avg');

  lines.push(pad('quantile', 10) + padLeft('direct', 12) + padLeft('gateway', 14) + padLeft('delta', 14));
  lines.push(RULE);
  lines.push(row('p50', stat(data, 'direct_latency', 'p(50)'), stat(data, 'gateway_latency', 'p(50)')));
  lines.push(row('p95', stat(data, 'direct_latency', 'p(95)'), stat(data, 'gateway_latency', 'p(95)')));
  lines.push(row('p99', stat(data, 'direct_latency', 'p(99)'), stat(data, 'gateway_latency', 'p(99)')));
  lines.push(row('mean', dAvg, gAvg));
  lines.push(RULE);
  lines.push('samples   ' + padLeft(count(data, 'direct_latency'), 12) +
    padLeft(count(data, 'gateway_latency'), 14));
  lines.push('');

  // The single most abused number in gateway benchmarking gets its
  // caveat printed next to it, every run, rather than in a footnote
  // nobody reaches.
  lines.push('How to read the delta column');
  lines.push(RULE);
  lines.push('mean : the mean delta IS the mean per request overhead. Means');
  lines.push('       subtract exactly, so E[gateway] - E[direct] = E[overhead]');
  lines.push('       even though no request was measured both ways.');
  lines.push('');
  lines.push('p50/p95/p99 : these are DIFFERENCES OF QUANTILES, not quantiles');
  lines.push('       of the per request overhead. Quantiles do not subtract. The');
  lines.push('       p95 row says how far the gateway moved the 95th percentile');
  lines.push('       of end to end latency, which is a fair statement about the');
  lines.push('       tail of the system. It does NOT say that 95% of requests');
  lines.push('       paid less than that in gateway cost. Nothing in this harness');
  lines.push('       can say that, because measuring one request both ways at');
  lines.push('       once is not possible.');
  lines.push('');

  if (driftCheck) {
    const a = stat(data, 'direct_latency', 'p(50)');
    const b = stat(data, 'direct_recheck_latency', 'p(50)');
    lines.push('Drift check');
    lines.push(RULE);
    lines.push('direct p50 before gateway arm : ' + ms(a));
    lines.push('direct p50 after  gateway arm : ' + ms(b));
    if (a !== null && b !== null && a > 0) {
      const pctDrift = Math.abs(b - a) / a * 100;
      lines.push('drift                         : ' + pctDrift.toFixed(1) + '%');
      if (pctDrift > 10) {
        lines.push('');
        lines.push('WARNING: the two direct measurements differ by more than 10%.');
        lines.push('The machine did not hold still across this run, so the delta');
        lines.push('above cannot be attributed to the gateway. Close whatever else');
        lines.push('is running and repeat.');
      }
    }
    lines.push('');
  }

  const shedCount = count(data, 'gateway_shed_503');
  if (shedCount > 0) {
    lines.push('NOTE: the gateway shed ' + shedCount + ' requests with 503. That is its');
    lines.push('in flight ceiling working, not a defect, but this run is measuring');
    lines.push('load shedding rather than overhead. Lower RATE or raise');
    lines.push('server.max_inflight in the bench config.');
    lines.push('');
  }

  return summaryOutputs(lines.join('\n'), data);
}
