// compare_litellm.js
//
// The question this scenario exists to answer: how much latency does
// Penstock add to a request, and how much does LiteLLM add, measured
// the same way, on the same machine, against the same upstream, in the
// same run?
//
// It is gateway_overhead.js with a third arm. Everything that file
// argues for applies here unchanged, so the reasoning is not repeated;
// what follows is only what is different because a second gateway is
// in the picture.
//
// WHY BOTH GATEWAYS ARE REPORTED AS A DELTA OVER ONE SHARED BASELINE
//
//   arm 1   k6 -----------------------> llmsim      the baseline
//   arm 2   k6 ---> penstock ---------> llmsim      baseline + penstock
//   arm 3   k6 ---> litellm ----------> llmsim      baseline + litellm
//   arm 4   k6 -----------------------> llmsim      the drift check
//
// Neither gateway's absolute number means anything: both are dominated
// by the simulated upstream, and publishing either as "gateway latency"
// would be reporting the speed of the simulator. Only the two deltas
// are attributable, and they are only comparable to each other because
// they are differences over the SAME baseline measured in the SAME run.
//
// A comparison built any other way, in particular one that quotes a
// LiteLLM figure from somebody else's blog post next to a Penstock
// figure measured here, is not a comparison. It is two unrelated
// numbers printed near each other.
//
// WHY THE ARMS ARE SEQUENTIAL AND WHY THERE IS STILL A DRIFT ARM
//
// Same reasons as gateway_overhead.js: concurrent arms would double the
// load on a shared simulator, and sequential arms let the machine drift
// between them. With three measured arms instead of two the run is
// longer, so there is MORE room for drift, which makes the repeated
// direct arm at the end more important here rather than less. If
// direct_a and direct_b disagree, nothing in this run is attributable
// to either gateway and the summary says so.
//
// WHY THREE SIMULATORS
//
// bench/compare/run.sh starts three llmsim instances on the same --seed:
// one for the baseline, one behind Penstock, one behind LiteLLM. llmsim
// derives each request's simulated TTFT, inter-token latency and token
// count from the seed and the request index, so request i is served
// with identical planned timings in all three arms. Sharing one
// simulator would leave each arm seeing a different slice of the
// distribution, and the two deltas would then differ partly because
// the upstream happened to be slower during one of them.
//
// WHAT THIS SCENARIO DOES NOT DO
//
// It does not exercise a single feature LiteLLM has and Penstock does
// not, which is most of them. It sends one plain non-streaming chat
// completion to one model over one provider. That is the only workload
// where the two are doing comparable work, and it is therefore the only
// workload where subtracting their latencies means anything. See the
// section in docs/comparison.md on what this does not show.

import http from 'k6/http';
import { check } from 'k6';
import { Trend, Counter } from 'k6/metrics';
import {
  CFG, CHAT_PATH, TREND_STATS,
  chatBody, uniquePrompt, gatewayHeaders, directHeaders,
  envStr, envBool, toSeconds, stat, count, ms, pad, padLeft,
  header, loadWarnings, summaryOutputs,
} from './lib/common.js';

// LiteLLM's listener. Not in CFG because lib/common.js is shared with
// the scenarios that know nothing about a second gateway.
const litellmURL = envStr('LITELLM_URL', 'http://127.0.0.1:8081');
// LiteLLM routes by the model_name in its config's model_list. It is
// the same string Penstock routes so both gateways do an equivalent
// lookup and put identical bytes on the wire.
const litellmModel = envStr('LITELLM_MODEL', CFG.model);

const driftCheck = envBool('DRIFT_CHECK', true);

const armSeconds = toSeconds(CFG.duration);
const gapSeconds = toSeconds(CFG.gap);
const step = armSeconds + gapSeconds;

const directLatency = new Trend('direct_latency', true);
const penstockLatency = new Trend('penstock_latency', true);
const litellmLatency = new Trend('litellm_latency', true);
const driftLatency = new Trend('direct_recheck_latency', true);

const penstockShed = new Counter('penstock_shed_503');
const litellmShed = new Counter('litellm_shed_503');
const penstockErrors = new Counter('penstock_non_200');
const litellmErrors = new Counter('litellm_non_200');

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

// Order matters only in that the drift arm has to be last. The baseline
// runs first so that both gateway arms are measured against a baseline
// taken before either of them has warmed the machine up.
const scenarios = {
  direct_a: armScenario('directArm', 0),
  via_penstock: armScenario('penstockArm', step),
  via_litellm: armScenario('litellmArm', 2 * step),
};
if (driftCheck) {
  scenarios.direct_b = armScenario('driftArm', 3 * step);
}

export const options = {
  scenarios: scenarios,
  summaryTrendStats: TREND_STATS,
  thresholds: {
    // A dropped iteration means the requested arrival rate was not
    // delivered, so the arms did not receive equal load. That voids the
    // comparison rather than degrading it.
    dropped_iterations: ['count == 0'],
    http_req_failed: ['rate == 0'],
  },
};

function post(url, headers, body, armTag, shedCounter, errCounter) {
  const res = http.post(url + CHAT_PATH, body, {
    headers: headers,
    tags: { arm: armTag },
  });
  if (res.status === 503 && shedCounter) {
    shedCounter.add(1);
  }
  if (res.status !== 200 && errCounter) {
    errCounter.add(1);
  }
  // The body check is not decoration. A gateway that answers 200 with an
  // error envelope, or that fast-fails an unrouted model, would look
  // dramatically cheaper than one doing the work. bench/README.md
  // records that exact accident happening on this harness once already.
  check(res, {
    'status 200': (r) => r.status === 200,
    'body is a completion': (r) =>
      typeof r.body === 'string' && r.body.indexOf('chat.completion') !== -1,
  });
  return res;
}

export function directArm() {
  const body = chatBody(CFG.directModel, uniquePrompt('cmp'), { stream: false });
  const res = post(CFG.directURL, directHeaders(), body, 'direct', null, null);
  directLatency.add(res.timings.duration);
}

export function penstockArm() {
  const body = chatBody(CFG.model, uniquePrompt('cmp'), { stream: false });
  const res = post(CFG.baseURL, gatewayHeaders(), body, 'penstock', penstockShed, penstockErrors);
  penstockLatency.add(res.timings.duration);
}

export function litellmArm() {
  const body = chatBody(litellmModel, uniquePrompt('cmp'), { stream: false });
  // Same header set as the Penstock arm: both gateways authenticate a
  // bearer token, so both are doing the same work and the request
  // bytes are identical.
  const res = post(litellmURL, gatewayHeaders(), body, 'litellm', litellmShed, litellmErrors);
  litellmLatency.add(res.timings.duration);
}

export function driftArm() {
  const body = chatBody(CFG.directModel, uniquePrompt('cmp'), { stream: false });
  const res = post(CFG.directURL, directHeaders(), body, 'drift', null, null);
  driftLatency.add(res.timings.duration);
}

// The table is wider than the other scenarios' because it carries two
// gateways and two deltas.
const WIDE = '-'.repeat(78);

function row(label, d, p, l) {
  const dp = d === null || p === null ? null : p - d;
  const dl = d === null || l === null ? null : l - d;
  return pad(label, 8) +
    padLeft(ms(d), 13) +
    padLeft(ms(p), 13) + padLeft(ms(dp), 13) +
    padLeft(ms(l), 13) + padLeft(ms(dl), 13);
}

export function handleSummary(data) {
  const lines = [];
  lines.push(header('Penstock against LiteLLM: gateway overhead', {
    'litellm': litellmURL,
    'arrival rate': CFG.rate + ' req/s per arm',
    'drift check': driftCheck ? 'on' : 'off',
  }));
  lines.push(loadWarnings(data));

  lines.push(pad('', 8) + padLeft('direct', 13) +
    padLeft('penstock', 13) + padLeft('delta', 13) +
    padLeft('litellm', 13) + padLeft('delta', 13));
  lines.push(WIDE);
  for (const q of ['p(50)', 'p(95)', 'p(99)']) {
    lines.push(row(q.replace('(', '').replace(')', ''),
      stat(data, 'direct_latency', q),
      stat(data, 'penstock_latency', q),
      stat(data, 'litellm_latency', q)));
  }
  lines.push(row('mean',
    stat(data, 'direct_latency', 'avg'),
    stat(data, 'penstock_latency', 'avg'),
    stat(data, 'litellm_latency', 'avg')));
  lines.push(WIDE);
  lines.push(pad('samples', 8) +
    padLeft(count(data, 'direct_latency'), 13) +
    padLeft(count(data, 'penstock_latency'), 13) + padLeft('', 13) +
    padLeft(count(data, 'litellm_latency'), 13));
  lines.push('');

  // Same caveat gateway_overhead.js prints, for the same reason: it is
  // the most abused number in gateway benchmarking and it belongs next
  // to the table rather than in a footnote nobody reaches.
  lines.push('How to read the delta columns');
  lines.push(WIDE);
  lines.push('mean : the mean delta IS the mean per request overhead. Means subtract');
  lines.push('       exactly, so E[gateway] - E[direct] = E[overhead], even though no');
  lines.push('       request was ever measured both ways.');
  lines.push('');
  lines.push('p50/p95/p99 : DIFFERENCES OF QUANTILES, not quantiles of the per request');
  lines.push('       overhead. Quantiles do not subtract. The p95 row says how far each');
  lines.push('       gateway moved the 95th percentile of end to end latency, which is a');
  lines.push('       fair statement about the tail of the system. It does NOT say 95% of');
  lines.push('       requests paid less than that. Nothing here can say that, because');
  lines.push('       measuring one request both with and without a gateway at the same');
  lines.push('       time is not possible.');
  lines.push('');
  lines.push('Comparing the two delta columns to each other is the only comparison this');
  lines.push('run supports, and only for the one workload it sent: a single non');
  lines.push('streaming chat completion to one model on one provider. LiteLLM does far');
  lines.push('more than that. See docs/comparison.md.');
  lines.push('');

  if (driftCheck) {
    const a = stat(data, 'direct_latency', 'p(50)');
    const b = stat(data, 'direct_recheck_latency', 'p(50)');
    lines.push('Drift check');
    lines.push(WIDE);
    lines.push('direct p50 before the gateway arms : ' + ms(a));
    lines.push('direct p50 after  the gateway arms : ' + ms(b));
    if (a !== null && b !== null && a > 0) {
      const pctDrift = Math.abs(b - a) / a * 100;
      lines.push('drift                              : ' + pctDrift.toFixed(1) + '%');
      if (pctDrift > 10) {
        lines.push('');
        lines.push('WARNING: the two direct measurements differ by more than 10%. The');
        lines.push('machine did not hold still, so NEITHER delta above is attributable');
        lines.push('to its gateway, and comparing them to each other is worse still:');
        lines.push('the arms ran at different times, so drift lands unevenly on them.');
        lines.push('Close whatever else is running and repeat.');
      }
    }
    lines.push('');
  }

  const ps = count(data, 'penstock_shed_503');
  const ls = count(data, 'litellm_shed_503');
  const pe = count(data, 'penstock_non_200');
  const le = count(data, 'litellm_non_200');
  if (ps > 0 || ls > 0 || pe > 0 || le > 0) {
    lines.push('Non-200 responses');
    lines.push(WIDE);
    lines.push('penstock : ' + pe + ' non-200, of which ' + ps + ' were 503 shed');
    lines.push('litellm  : ' + le + ' non-200, of which ' + ls + ' were 503 shed');
    lines.push('');
    lines.push('A failed request is usually CHEAPER than a successful one, so any arm');
    lines.push('with errors has an understated latency and an unfairly flattering');
    lines.push('delta. Do not compare the columns until this is zero on both sides.');
    lines.push('');
  }

  return summaryOutputs(lines.join('\n'), data);
}
