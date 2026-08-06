// cache_hit.js
//
// What a cache hit costs against what a miss costs, plus the hit rate
// actually achieved, with the exact tier warm.
//
// This is the one scenario where the gateway is expected to be FASTER
// than the upstream it fronts, so it is also the easiest one to cheat
// with. The guards are spelled out below.
//
// THE SEMANTIC TIER IS DELIBERATELY OFF
//
// bench/config/gateway-cache.yaml enables caching with cache.semantic
// left disabled, which is the shipped default. That is not an oversight
// and it is not tuning the benchmark for a nicer number: it is the
// opposite. Turning the semantic tier on would raise the hit rate
// printed at the bottom of this run, and the reason it stays off is
// that the measurements in docs/semantic-caching.md show cosine
// similarity ranking opposite-meaning questions ABOVE genuine
// paraphrases, so a higher hit rate bought that way includes hits that
// answer a question nobody asked. Read docs/semantic-caching.md before
// changing this. A hit rate published without its false hit rate beside
// it is not a result.
//
// WHAT MAKES A REQUEST CACHEABLE
//
// internal/cache/policy.go refuses to cache a request with no explicit
// temperature, with a seed, with tools, with logprobs, or asking for
// more than one completion. Every request below therefore carries
// "temperature": 0 explicitly. Entries are also keyed per tenant, so
// the warm phase and the measured phase must present the same bearer
// token, which they do because both use API_KEY.
//
// WHY THE WARM PHASE IS IN setup()
//
// setup() runs to completion before any scenario starts, so the hit arm
// cannot begin before the cache holds what it is about to ask for. The
// alternative, a warm scenario plus a startTime on the measured arm, is
// a race dressed up as a schedule. http.batch issues the warm requests
// concurrently so warming does not take CACHE_KEYS multiplied by the
// full simulated generation time.
//
// WHY constant-arrival-rate
//
// Open model, as everywhere else in this harness. It matters
// particularly here: a hit is roughly two orders of magnitude cheaper
// than a miss, so under a closed model the hit arm would fire far more
// requests per VU than the miss arm and the two arms would be measured
// under completely different concurrency. Fixing the arrival rate makes
// the only difference between the arms the thing being studied.

import http from 'k6/http';
import { check } from 'k6';
import { Trend, Rate, Counter } from 'k6/metrics';
import {
  CFG, CHAT_PATH, TREND_STATS, RULE,
  chatBody, uniquePrompt, gatewayHeaders,
  envInt, envStr, toSeconds, stat, count, ms, pad, padLeft,
  header, loadWarnings, summaryOutputs,
} from './lib/common.js';

// How many distinct questions the cache is warmed with. Larger is a
// more honest working set and a longer warm phase.
const cacheKeys = envInt('CACHE_KEYS', 100);
const warmBatch = envInt('WARM_BATCH', 50);

const armSeconds = toSeconds(CFG.duration);
const gapSeconds = toSeconds(CFG.gap);

const hitLatency = new Trend('cache_hit_latency', true);
const missLatency = new Trend('cache_miss_latency', true);
const warmLatency = new Trend('cache_warm_latency', true);
const hitRate = new Rate('cache_hit_rate');
const hitRateHitArm = new Rate('cache_hit_rate_hit_arm');
const hitRateMissArm = new Rate('cache_hit_rate_miss_arm');
const unexpectedHits = new Counter('cache_unexpected_hits');
const warmFailures = new Counter('cache_warm_failures');

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

export const options = {
  scenarios: {
    // Hits first, immediately after the warm phase, so nothing has had
    // a chance to expire out from under the measurement.
    hits: armScenario('hitArm', 0),
    misses: armScenario('missArm', armSeconds + gapSeconds),
  },
  summaryTrendStats: TREND_STATS,
  batch: warmBatch,
  batchPerHost: warmBatch,
  setupTimeout: envStr('SETUP_TIMEOUT', '300s'),
  thresholds: {
    dropped_iterations: ['count == 0'],
    http_req_failed: ['rate == 0'],
    // The hit arm asking for questions the cache was just given must
    // actually hit. Anything less means the run measured something
    // other than a warm cache, and the hit latency below is a blend.
    cache_hit_rate_hit_arm: ['rate > 0.99'],
    // The miss arm sends questions nobody has ever asked. A hit here
    // would mean the gateway answered a question it had not seen, which
    // is a correctness alarm, not a performance win.
    cache_hit_rate_miss_arm: ['rate == 0'],
  },
};

// cachePrompt is deterministic so the warm phase and the hit arm build
// byte identical requests. The cache key is a hash of the canonicalized
// body, so anything that varies here is a miss.
function cachePrompt(i) {
  return 'penstock cache probe number ' + i;
}

function cacheableBody(prompt) {
  return chatBody(CFG.model, prompt, { stream: false, temperature: 0 });
}

// cacheStatus reads the header the gateway sets on a served entry
// (internal/ingress/cached.go). Absent means the answer came from
// upstream. The lookup is case insensitive because header casing is not
// something a benchmark should be brittle about.
function cacheStatus(res) {
  const h = res.headers || {};
  for (const name of Object.keys(h)) {
    if (name.toLowerCase() === 'x-penstock-cache') {
      return h[name];
    }
  }
  return '';
}

export function setup() {
  const headers = gatewayHeaders();
  let ok = 0;
  let failed = 0;

  for (let start = 0; start < cacheKeys; start += warmBatch) {
    const reqs = [];
    for (let i = start; i < Math.min(start + warmBatch, cacheKeys); i++) {
      reqs.push({
        method: 'POST',
        url: CFG.baseURL + CHAT_PATH,
        body: cacheableBody(cachePrompt(i)),
        params: { headers: headers, tags: { arm: 'warm' } },
      });
    }
    const responses = http.batch(reqs);
    for (const res of responses) {
      if (res.status === 200) {
        ok++;
        warmLatency.add(res.timings.duration);
      } else {
        failed++;
        warmFailures.add(1);
      }
    }
  }

  if (ok === 0) {
    throw new Error(
      'cache warm phase stored nothing: every request to ' + CFG.baseURL +
      ' failed. Check that the gateway is up and that bench/config/gateway-cache.yaml ' +
      'is the config it was started with.'
    );
  }
  return { warmed: ok, failed: failed };
}

export function hitArm(setupData) {
  // Every VU cycles through the same warmed questions. Which one it
  // picks does not matter as long as it is one of the warmed set.
  const i = (__VU * 7919 + __ITER) % cacheKeys;
  const res = http.post(CFG.baseURL + CHAT_PATH, cacheableBody(cachePrompt(i)), {
    headers: gatewayHeaders(),
    tags: { arm: 'hit' },
  });

  const status = cacheStatus(res);
  const isHit = status !== '';
  check(res, {
    'status 200': (r) => r.status === 200,
    'served from cache': () => isHit,
    'served by the exact tier': () => status === '' || status === 'hit-exact',
  });

  hitRate.add(isHit);
  hitRateHitArm.add(isHit);
  if (isHit) {
    hitLatency.add(res.timings.duration);
  }
  return setupData;
}

export function missArm() {
  // A question nobody has asked. This is the control: it is what the
  // same gateway costs when the cache cannot help, and it is the number
  // the hit column has to be read against.
  const res = http.post(CFG.baseURL + CHAT_PATH, cacheableBody(uniquePrompt('cache-miss')), {
    headers: gatewayHeaders(),
    tags: { arm: 'miss' },
  });

  const status = cacheStatus(res);
  const isHit = status !== '';
  if (isHit) {
    unexpectedHits.add(1);
  }
  check(res, {
    'status 200': (r) => r.status === 200,
    'not served from cache': () => !isHit,
  });

  hitRate.add(isHit);
  hitRateMissArm.add(isHit);
  if (!isHit) {
    missLatency.add(res.timings.duration);
  }
}

function row(label, hit, miss) {
  let ratio = '     n/a';
  if (hit !== null && miss !== null && hit > 0) {
    ratio = padLeft((miss / hit).toFixed(1) + 'x', 8);
  }
  return pad(label, 10) + padLeft(ms(hit), 12) + padLeft(ms(miss), 14) + padLeft(ratio, 12);
}

export function handleSummary(data) {
  const lines = [];
  lines.push(header('Penstock exact cache: hit against miss', {
    'cache keys': cacheKeys,
    'semantic': 'OFF on purpose, see docs/semantic-caching.md',
  }));
  lines.push(loadWarnings(data));

  lines.push(pad('quantile', 10) + padLeft('hit', 12) + padLeft('miss', 14) + padLeft('speedup', 12));
  lines.push(RULE);
  lines.push(row('p50', stat(data, 'cache_hit_latency', 'p(50)'), stat(data, 'cache_miss_latency', 'p(50)')));
  lines.push(row('p95', stat(data, 'cache_hit_latency', 'p(95)'), stat(data, 'cache_miss_latency', 'p(95)')));
  lines.push(row('p99', stat(data, 'cache_hit_latency', 'p(99)'), stat(data, 'cache_miss_latency', 'p(99)')));
  lines.push(row('mean', stat(data, 'cache_hit_latency', 'avg'), stat(data, 'cache_miss_latency', 'avg')));
  lines.push(RULE);
  lines.push('samples   ' + padLeft(count(data, 'cache_hit_latency'), 12) +
    padLeft(count(data, 'cache_miss_latency'), 14));
  lines.push('');

  const overall = stat(data, 'cache_hit_rate', 'rate');
  const hitArm = stat(data, 'cache_hit_rate_hit_arm', 'rate');
  const missArm = stat(data, 'cache_hit_rate_miss_arm', 'rate');
  const pct = (v) => (v === null ? 'n/a' : (v * 100).toFixed(2) + '%');

  lines.push('Hit rate');
  lines.push(RULE);
  lines.push('warm entries stored : ' + count(data, 'cache_warm_latency'));
  lines.push('hit arm             : ' + pct(hitArm) + '   (asking only warmed questions)');
  lines.push('miss arm            : ' + pct(missArm) + '   (asking only new questions)');
  lines.push('across both arms    : ' + pct(overall));
  lines.push('');
  lines.push('That combined figure is an artifact of how this test splits its');
  lines.push('traffic, not a property of the cache. A hit rate only means');
  lines.push('something next to the request mix that produced it, and the mix');
  lines.push('here was chosen to measure both sides, not to look good.');
  lines.push('');

  const bad = count(data, 'cache_unexpected_hits');
  if (bad > 0) {
    lines.push('ALARM: ' + bad + ' requests carrying a question never asked before were');
    lines.push('answered from cache. That is a correctness failure, not a fast');
    lines.push('result. Do not publish this run.');
    lines.push('');
  }

  lines.push('What this does and does not show');
  lines.push(RULE);
  lines.push('A hit costs a hash and a map lookup and never leaves the process,');
  lines.push('so the ratio above is mostly a statement about how slow the');
  lines.push('simulated upstream is. Against a faster upstream the ratio shrinks;');
  lines.push('against a real provider on a bad day it grows. The transferable');
  lines.push('number is the absolute hit latency, not the speedup.');
  lines.push('');

  return summaryOutputs(lines.join('\n'), data);
}
