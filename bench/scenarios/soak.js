// soak.js
//
// A long, deliberately gentle run whose purpose is not a latency
// number. It exists to answer one question: does this gateway still
// behave the same way after hours of traffic as it did in the first
// minute?
//
// WHY constant-vus, WHEN EVERY OTHER SCENARIO USES AN OPEN MODEL
//
// The other scenarios use constant-arrival-rate because a closed model
// hides latency differences. Here that reasoning inverts. A soak is not
// comparing two things, it is holding one thing steady for a long time,
// and under an open model a gateway that slowly degrades would build an
// ever growing backlog in the load generator until the run was
// measuring k6's queue instead of the gateway, or until k6 gave up and
// started dropping iterations. A closed model applies gentle back
// pressure, cannot run away, and will hold a low steady load for hours
// without the harness becoming the bottleneck. Low VU count plus a
// think time between iterations is the point, not a limitation.
//
// WHAT LEAK EVIDENCE THIS CAN AND CANNOT PRODUCE
//
// It compares the latency distribution of the first quarter of the run
// against the last quarter. A gateway leaking memory, goroutines, file
// descriptors or connections almost always shows it as a tail that
// creeps: identical work getting slower with nothing else changing. If
// the late p95 has moved and the early one has not, there is something
// to go find with a profiler.
//
// It CANNOT read heap or goroutine counts. internal/obs/metrics.go
// builds a private Prometheus registry and registers only Penstock's
// own instruments on it, so the standard go_memstats_* and go_goroutines
// series are not exposed on /metrics. Resident set size is therefore
// sampled out of band by bench/run.sh instead, into a .rss.csv beside
// the results, on a best effort basis. Growing RSS next to a flat
// latency curve is still a leak; this scenario only sees the second
// half of that picture.
//
// The admin listener is polled throughout as a liveness probe. An
// operator surface that stops answering, or that gets slower while the
// data path looks fine, is its own kind of evidence.

import http from 'k6/http';
import { check, sleep } from 'k6';
import { Trend, Counter } from 'k6/metrics';
import {
  CFG, CHAT_PATH, TREND_STATS, RULE,
  chatBody, uniquePrompt, gatewayHeaders,
  envInt, envStr, envFloat, toSeconds, stat, count, ms, pad, padLeft,
  header, loadWarnings, summaryOutputs,
} from './lib/common.js';

// A soak has its own duration knob, because reusing the 30s default the
// comparison scenarios use would make it a smoke test wearing a soak's
// name.
const soakDuration = envStr('SOAK_DURATION', envStr('DURATION', '30m'));
const soakSeconds = toSeconds(soakDuration);
const soakVUs = envInt('SOAK_VUS', envInt('VUS', 5));
const thinkTime = envFloat('SLEEP', 1.0);
const adminURL = envStr('ADMIN_URL', 'http://127.0.0.1:9090');
const watchInterval = envFloat('WATCH_INTERVAL', 15);

// The fraction of the run at each end that counts as early and late.
const edgeFraction = envFloat('EDGE_FRACTION', 0.25);

const soakLatency = new Trend('soak_latency', true);
const earlyLatency = new Trend('soak_latency_early', true);
const lateLatency = new Trend('soak_latency_late', true);
const adminLatency = new Trend('soak_admin_latency', true);
const adminFailures = new Counter('soak_admin_failures');
const requestFailures = new Counter('soak_request_failures');

export const options = {
  scenarios: {
    steady: {
      executor: 'constant-vus',
      vus: soakVUs,
      duration: soakDuration,
      gracefulStop: CFG.gracefulStop,
      exec: 'soakArm',
    },
    // One VU doing nothing but checking that the operator surface is
    // still there. It shares the run so its samples land in the same
    // raw JSON and can be lined up against the data path by timestamp.
    watch: {
      executor: 'constant-vus',
      vus: 1,
      duration: soakDuration,
      gracefulStop: '10s',
      exec: 'watchArm',
    },
  },
  summaryTrendStats: TREND_STATS,
  thresholds: {
    http_req_failed: ['rate == 0'],
    checks: ['rate == 1.0'],
  },
};

export function setup() {
  // Captured once and handed to every VU so all of them agree on when
  // the run began. Date.now() inside a VU would be that VU's first
  // iteration, which for a ramp would not be the same instant.
  return { startedAt: Date.now(), soakSeconds: soakSeconds };
}

export function soakArm(data) {
  const elapsed = (Date.now() - data.startedAt) / 1000;
  const edge = data.soakSeconds * edgeFraction;
  let phase = 'mid';
  if (elapsed <= edge) {
    phase = 'early';
  } else if (elapsed >= data.soakSeconds - edge) {
    phase = 'late';
  }

  const res = http.post(CFG.baseURL + CHAT_PATH,
    chatBody(CFG.model, uniquePrompt('soak'), { stream: false }), {
      headers: gatewayHeaders(),
      // The phase tag is on the raw samples too, so the early against
      // late comparison at the bottom of this file can be recomputed
      // from the committed JSON rather than taken on trust.
      tags: { arm: 'soak', phase: phase },
    });

  const ok = check(res, {
    'status 200': (r) => r.status === 200,
    'body is a completion': (r) =>
      typeof r.body === 'string' && r.body.indexOf('chat.completion') !== -1,
  });
  if (!ok) {
    requestFailures.add(1);
  }

  soakLatency.add(res.timings.duration);
  if (phase === 'early') {
    earlyLatency.add(res.timings.duration);
  } else if (phase === 'late') {
    lateLatency.add(res.timings.duration);
  }

  sleep(thinkTime);
}

export function watchArm() {
  const res = http.get(adminURL + '/metrics', { tags: { arm: 'watch' } });
  const ok = check(res, {
    'admin listener answers': (r) => r.status === 200,
    'metrics are being served': (r) =>
      typeof r.body === 'string' && r.body.indexOf('penstock_requests_total') !== -1,
  });
  if (!ok) {
    adminFailures.add(1);
  }
  adminLatency.add(res.timings.duration);
  sleep(watchInterval);
}

function row(label, early, late) {
  let drift = '     n/a';
  if (early !== null && late !== null && early > 0) {
    const d = (late - early) / early * 100;
    drift = padLeft((d >= 0 ? '+' : '') + d.toFixed(1) + '%', 10);
  }
  return pad(label, 10) + padLeft(ms(early), 12) + padLeft(ms(late), 14) + padLeft(drift, 12);
}

export function handleSummary(data) {
  const lines = [];
  lines.push(header('Penstock soak', {
    'soak duration': soakDuration,
    'vus': soakVUs,
    'think time': thinkTime + 's',
    'edge window': (edgeFraction * 100).toFixed(0) + '% at each end',
  }, { armed: false }));
  lines.push(loadWarnings(data));

  lines.push('Latency drift across the run');
  lines.push(pad('quantile', 10) + padLeft('early', 12) + padLeft('late', 14) + padLeft('drift', 12));
  lines.push(RULE);
  const e95 = stat(data, 'soak_latency_early', 'p(95)');
  const l95 = stat(data, 'soak_latency_late', 'p(95)');
  lines.push(row('p50', stat(data, 'soak_latency_early', 'p(50)'), stat(data, 'soak_latency_late', 'p(50)')));
  lines.push(row('p95', e95, l95));
  lines.push(row('p99', stat(data, 'soak_latency_early', 'p(99)'), stat(data, 'soak_latency_late', 'p(99)')));
  lines.push(row('mean', stat(data, 'soak_latency_early', 'avg'), stat(data, 'soak_latency_late', 'avg')));
  lines.push(RULE);
  lines.push('samples   ' + padLeft(count(data, 'soak_latency_early'), 12) +
    padLeft(count(data, 'soak_latency_late'), 14));
  lines.push('total requests : ' + count(data, 'soak_latency'));
  lines.push('failed requests: ' + count(data, 'soak_request_failures'));
  lines.push('');

  // A p95 computed over a few dozen samples moves tens of percent on
  // noise alone, so below this bar the comparison cannot support either
  // conclusion and says so instead of picking one. This is not a
  // formality: the first test run of this scenario, over 40 seconds,
  // reported a 28% "leak signal" that was entirely sampling noise.
  const MIN_EDGE_SAMPLES = 500;
  const nEarly = count(data, 'soak_latency_early');
  const nLate = count(data, 'soak_latency_late');

  if (e95 === null || l95 === null || e95 <= 0) {
    lines.push('Not enough samples in one of the edge windows to compare.');
    lines.push('');
  } else if (nEarly < MIN_EDGE_SAMPLES || nLate < MIN_EDGE_SAMPLES) {
    lines.push('UNDER POWERED: ' + nEarly + ' early and ' + nLate + ' late samples, against ' +
      MIN_EDGE_SAMPLES + ' needed');
    lines.push('before a p95 comparison means anything. A p95 over this few');
    lines.push('samples swings by tens of percent on noise alone, so this run');
    lines.push('is evidence of nothing either way. Raise SOAK_DURATION, or lower');
    lines.push('SLEEP, until each edge window holds at least ' + MIN_EDGE_SAMPLES + ' samples.');
    lines.push('');
  } else {
    const drift = (l95 - e95) / e95 * 100;
    if (drift > 20) {
      lines.push('SIGNAL: the late p95 is ' + drift.toFixed(0) + '% above the early p95 under');
      lines.push('identical work. That is what a leak looks like from the outside.');
      lines.push('Check the .rss.csv beside this run for resident memory, and');
      lines.push('take a pprof heap profile before and after a repeat run.');
      lines.push('');
    } else {
      lines.push('No latency drift beyond 20% between the first and last ' +
        (edgeFraction * 100).toFixed(0) + '% of');
      lines.push('the run. That is evidence against a leak of the kind that shows');
      lines.push('up as degradation, and it is not evidence against a leak that');
      lines.push('only grows memory. Read the .rss.csv beside this run.');
      lines.push('');
    }
  }

  lines.push('Admin listener');
  lines.push(RULE);
  lines.push('scrapes        : ' + count(data, 'soak_admin_latency'));
  lines.push('failures       : ' + count(data, 'soak_admin_failures'));
  lines.push('p95 latency    : ' + ms(stat(data, 'soak_admin_latency', 'p(95)')));
  lines.push('');
  lines.push('Go runtime series (go_memstats_*, go_goroutines) are not on this');
  lines.push('registry, so heap and goroutine growth cannot be read here. See');
  lines.push('the note at the top of this file.');
  lines.push('');

  return summaryOutputs(lines.join('\n'), data);
}
