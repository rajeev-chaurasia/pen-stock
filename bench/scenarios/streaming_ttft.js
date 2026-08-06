// streaming_ttft.js
//
// Time to first token on a streamed completion, direct against llmsim
// and through the gateway, reported as both arms and their difference.
//
// TTFT is the number that decides whether a chat UI feels alive, and it
// is the one place a gateway can do real damage: everything after the
// first token is the provider's pace, but the wait before it is
// whatever the provider takes plus whatever the gateway adds on the way
// in. So this measures the same difference gateway_overhead.js does,
// against the part of the request a user actually feels.
//
// HOW THE FIRST FRAME IS TIMED, AND THE LIMIT OF IT
//
// Stock k6 has no incremental reader for an HTTP response body: http.post
// returns once the whole response has been consumed. There is no way in
// a k6 script to be woken at the instant the first `data:` frame lands,
// so this does the next thing and says so plainly.
//
// The measurement used is res.timings.waiting, k6's time to first
// response byte. For these two servers that is the same instant as the
// first `data:` frame, and that equality is a property of their code
// rather than an assumption:
//
//   - llmsim (internal/llmsim/server.go, streamChat) sets its SSE
//     headers, then sleeps the simulated TTFT, then writes and flushes
//     the first event. Go does not put headers on the wire until that
//     first flush, so the first byte the client sees IS the first frame.
//
//   - the gateway (internal/ingress/handlers.go, serveStream) calls
//     ChatStream first and only writes its own 200 once the upstream's
//     response headers have arrived. It does not answer early, so its
//     first byte is also gated on the upstream's first frame.
//
// The residual bias is named rather than hidden: the gateway flushes
// its headers on receiving the upstream's headers, one Recv and one
// write before it emits its own first `data:` frame. So the gateway arm
// understates true time to first frame by that small amount, which
// makes the reported gateway overhead slightly optimistic. Every
// iteration still parses the buffered body and asserts the first frame
// really is a `data:` frame and that the stream reached [DONE], so a
// run where that stopped being true fails its checks instead of quietly
// reporting a number about something else.
//
// WHY constant-arrival-rate
//
// Open model, for the same reason as gateway_overhead.js: a closed
// model would send less load to whichever arm is slower and hide the
// difference. Note that streamed requests hold a connection for the
// whole generation, so the VU pool has to cover RATE multiplied by the
// mean stream duration. MAX_VUS too low shows up as dropped iterations,
// which the summary treats as voiding the run.

import http from 'k6/http';
import { check } from 'k6';
import { Trend } from 'k6/metrics';
import {
  CFG, CHAT_PATH, TREND_STATS, RULE,
  chatBody, uniquePrompt, gatewayHeaders, directHeaders,
  toSeconds, stat, count, ms, pad, padLeft,
  header, loadWarnings, summaryOutputs,
} from './lib/common.js';

const armSeconds = toSeconds(CFG.duration);
const gapSeconds = toSeconds(CFG.gap);

const directTTFT = new Trend('direct_ttft', true);
const gatewayTTFT = new Trend('gateway_ttft', true);
// Time spent receiving the rest of the stream after the first byte.
// This is the provider's generation pace and it is reported so nobody
// mistakes the total for the gateway's contribution.
const directGen = new Trend('direct_generation', true);
const gatewayGen = new Trend('gateway_generation', true);
const frames = new Trend('stream_frames');

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
    direct_stream: armScenario('directArm', 0),
    gateway_stream: armScenario('gatewayArm', armSeconds + gapSeconds),
  },
  summaryTrendStats: TREND_STATS,
  thresholds: {
    dropped_iterations: ['count == 0'],
    http_req_failed: ['rate == 0'],
    // A stream that never reached [DONE] was truncated, and its TTFT is
    // still real but its presence means the run hit an error path.
    checks: ['rate == 1.0'],
  },
};

// countFrames reports how many `data:` frames the buffered body holds,
// and whether the stream terminated the way a complete one does.
function inspect(body) {
  if (typeof body !== 'string' || body.length === 0) {
    return { first: false, done: false, n: 0 };
  }
  const parts = body.split('\n\n');
  let n = 0;
  for (const p of parts) {
    if (p.indexOf('data:') === 0) {
      n++;
    }
  }
  return {
    first: body.indexOf('data:') === 0,
    done: body.indexOf('data: [DONE]') !== -1,
    n: n,
  };
}

function streamOnce(url, headers, model, arm, ttft, gen) {
  const body = chatBody(model, uniquePrompt('ttft'), { stream: true });
  const res = http.post(url + CHAT_PATH, body, {
    headers: headers,
    tags: { arm: arm },
  });

  const s = inspect(res.body);
  check(res, {
    'status 200': (r) => r.status === 200,
    'first byte is a data: frame': () => s.first,
    'stream reached [DONE]': () => s.done,
  });

  if (res.status === 200 && s.first) {
    ttft.add(res.timings.waiting);
    gen.add(res.timings.receiving);
    frames.add(s.n);
  }
}

export function directArm() {
  streamOnce(CFG.directURL, directHeaders(), CFG.directModel, 'direct', directTTFT, directGen);
}

export function gatewayArm() {
  streamOnce(CFG.baseURL, gatewayHeaders(), CFG.model, 'gateway', gatewayTTFT, gatewayGen);
}

function row(label, d, g) {
  const delta = d === null || g === null ? null : g - d;
  return pad(label, 10) + padLeft(ms(d), 12) + padLeft(ms(g), 14) + padLeft(ms(delta), 14);
}

export function handleSummary(data) {
  const lines = [];
  lines.push(header('Penstock streaming TTFT', {
    'arrival rate': CFG.rate + ' req/s per arm',
    'measured as': 'time to first response byte (see file header)',
  }));
  lines.push(loadWarnings(data));

  lines.push('Time to first token');
  lines.push(pad('quantile', 10) + padLeft('direct', 12) + padLeft('gateway', 14) + padLeft('delta', 14));
  lines.push(RULE);
  lines.push(row('p50', stat(data, 'direct_ttft', 'p(50)'), stat(data, 'gateway_ttft', 'p(50)')));
  lines.push(row('p95', stat(data, 'direct_ttft', 'p(95)'), stat(data, 'gateway_ttft', 'p(95)')));
  lines.push(row('p99', stat(data, 'direct_ttft', 'p(99)'), stat(data, 'gateway_ttft', 'p(99)')));
  lines.push(row('mean', stat(data, 'direct_ttft', 'avg'), stat(data, 'gateway_ttft', 'avg')));
  lines.push(RULE);
  lines.push('samples   ' + padLeft(count(data, 'direct_ttft'), 12) +
    padLeft(count(data, 'gateway_ttft'), 14));
  lines.push('');

  lines.push('Generation time after the first byte (this is llmsim, not the gateway)');
  lines.push(pad('quantile', 10) + padLeft('direct', 12) + padLeft('gateway', 14) + padLeft('delta', 14));
  lines.push(RULE);
  lines.push(row('p50', stat(data, 'direct_generation', 'p(50)'), stat(data, 'gateway_generation', 'p(50)')));
  lines.push(row('p95', stat(data, 'direct_generation', 'p(95)'), stat(data, 'gateway_generation', 'p(95)')));
  lines.push(row('mean', stat(data, 'direct_generation', 'avg'), stat(data, 'gateway_generation', 'avg')));
  lines.push(RULE);
  lines.push('mean frames per stream : ' + (stat(data, 'stream_frames', 'avg') || 0).toFixed(1));
  lines.push('');

  lines.push('Reading this');
  lines.push(RULE);
  lines.push('The absolute TTFT figures are a property of the profile llmsim is');
  lines.push('replaying. They are NOT any provider\'s real time to first token,');
  lines.push('and quoting them as such would be a lie about a service that was');
  lines.push('never called. The delta column is the part that is about Penstock.');
  lines.push('');
  lines.push('The mean delta subtracts exactly. The per quantile deltas are');
  lines.push('differences of quantiles and do not describe any single request:');
  lines.push('see the same note in gateway_overhead.js.');
  lines.push('');
  lines.push('At TIME_SCALE=1 the simulated TTFT is hundreds of milliseconds and');
  lines.push('the gateway\'s contribution sits near the timer noise floor. Rerun');
  lines.push('with a small TIME_SCALE to resolve it; the absolute numbers then');
  lines.push('stop being profile realistic, which is the trade being made.');
  lines.push('');

  return summaryOutputs(lines.join('\n'), data);
}
