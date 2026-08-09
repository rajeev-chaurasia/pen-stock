// Shared helpers for the Penstock benchmark scenarios.
//
// Nothing here is imported from a remote URL on purpose. A benchmark
// that fetches its own formatting code over the network at run time is
// one network outage away from being unreproducible, and the whole
// claim these scripts back is reproducibility.

// ---------------------------------------------------------------------
// Environment knobs
//
// Every scenario takes its configuration from environment variables so a
// run can be described completely by the command that started it. The
// defaults below are the loopback ports bench/run.sh brings up.
// ---------------------------------------------------------------------

export function envStr(name, fallback) {
  const v = __ENV[name];
  return v === undefined || v === '' ? fallback : v;
}

export function envInt(name, fallback) {
  const v = __ENV[name];
  if (v === undefined || v === '') {
    return fallback;
  }
  const n = parseInt(v, 10);
  if (isNaN(n)) {
    throw new Error(name + ' must be an integer, got ' + JSON.stringify(v));
  }
  return n;
}

export function envFloat(name, fallback) {
  const v = __ENV[name];
  if (v === undefined || v === '') {
    return fallback;
  }
  const n = parseFloat(v);
  if (isNaN(n)) {
    throw new Error(name + ' must be a number, got ' + JSON.stringify(v));
  }
  return n;
}

export function envBool(name, fallback) {
  const v = __ENV[name];
  if (v === undefined || v === '') {
    return fallback;
  }
  return v === '1' || v.toLowerCase() === 'true' || v.toLowerCase() === 'yes';
}

// Common knobs, resolved once so every scenario reports the same values
// in its summary header.
export const CFG = {
  // The gateway under test.
  baseURL: envStr('BASE_URL', 'http://127.0.0.1:8080'),
  // The simulated upstream, reached with the gateway cut out of the
  // path. This is a second llmsim process, not the one the gateway
  // talks to: see the note on paired upstreams in bench/README.md.
  directURL: envStr('DIRECT_URL', 'http://127.0.0.1:8090'),
  // Bearer token the gateway requires. The default matches the literal
  // key in bench/config/*.yaml, which is a fixed loopback bench
  // constant and not a secret.
  apiKey: envStr('API_KEY', 'benchbenchbenchbenchbench'),
  // Model name the gateway routes. llmsim answers to any name, so the
  // direct arm can use the same one.
  model: envStr('MODEL', 'llmsim-small'),
  directModel: envStr('DIRECT_MODEL', envStr('MODEL', 'llmsim-small')),

  duration: envStr('DURATION', '30s'),
  // Arrival rate in requests per second for the open model scenarios.
  rate: envInt('RATE', 20),
  preAllocatedVUs: envInt('PRE_ALLOCATED_VUS', 50),
  maxVUs: envInt('MAX_VUS', 200),
  // Idle gap between sequential arms, so one arm's connections and
  // goroutines are fully torn down before the next arm is measured.
  gap: envStr('ARM_GAP', '5s'),
  gracefulStop: envStr('GRACEFUL_STOP', '30s'),
};

export const CHAT_PATH = '/v1/chat/completions';

// toSeconds parses a k6 duration string. Scenarios that run their arms
// one after another have to compute each arm's startTime themselves,
// and k6 gives them no arithmetic over its own duration format.
export function toSeconds(d) {
  const m = /^(\d+(?:\.\d+)?)(ms|s|m|h)$/.exec(String(d).trim());
  if (!m) {
    throw new Error('duration must look like 30s, 5m or 500ms, got ' + JSON.stringify(d));
  }
  const n = parseFloat(m[1]);
  switch (m[2]) {
    case 'ms':
      return n / 1000;
    case 's':
      return n;
    case 'm':
      return n * 60;
    default:
      return n * 3600;
  }
}

export function gatewayHeaders() {
  return {
    'Content-Type': 'application/json',
    Authorization: 'Bearer ' + CFG.apiKey,
  };
}

// llmsim does not check credentials. The header is sent anyway so both
// arms put the same bytes on the wire: a request that differs in size
// between arms would show up as a difference the gateway did not cause.
export function directHeaders() {
  return {
    'Content-Type': 'application/json',
    Authorization: 'Bearer ' + CFG.apiKey,
  };
}

// ---------------------------------------------------------------------
// Request bodies
// ---------------------------------------------------------------------

// chatBody builds an OpenAI-shaped chat request.
//
// opts.unique appends a marker to the prompt. Uniqueness matters more
// than it looks: an identical body is exactly what the gateway's exact
// cache is built to answer without calling upstream, so a scenario that
// is not measuring the cache must never send the same body twice or it
// will time a cache hit and report it as gateway overhead.
export function chatBody(model, prompt, opts) {
  const o = opts || {};
  const body = {
    model: model,
    messages: [{ role: 'user', content: prompt }],
    stream: o.stream === true,
  };
  // Temperature is only set when a scenario wants the request to be
  // cache eligible. internal/cache/policy.go refuses to cache a request
  // with no temperature, because an absent temperature means the
  // provider default, which is not reproducible.
  if (o.temperature !== undefined) {
    body.temperature = o.temperature;
  }
  return JSON.stringify(body);
}

// uniquePrompt produces a prompt no other iteration in this run will
// send. llmsim's simulated latency is drawn from the request index
// rather than the prompt text, so varying the text costs nothing in
// comparability.
export function uniquePrompt(tag) {
  return 'bench ' + tag + ' vu=' + __VU + ' iter=' + __ITER;
}

// ---------------------------------------------------------------------
// Summary formatting
//
// k6 reports trend statistics under the keys named in
// options.summaryTrendStats, so every scenario sets TREND_STATS below
// and reads the percentiles back by the same names.
// ---------------------------------------------------------------------

export const TREND_STATS = ['avg', 'min', 'med', 'p(50)', 'p(95)', 'p(99)', 'max', 'count'];

// stat pulls one statistic out of a trend, or null when the metric was
// never recorded. A missing metric is normal: an arm can be switched
// off by an environment variable, and printing a zero for it would read
// as a measurement rather than an absence.
export function stat(data, metric, key) {
  const m = data.metrics[metric];
  if (!m || !m.values || m.values[key] === undefined) {
    return null;
  }
  return m.values[key];
}

export function count(data, metric) {
  const c = stat(data, metric, 'count');
  return c === null ? 0 : c;
}

export function ms(v) {
  if (v === null || v === undefined) {
    return '     n/a';
  }
  return v.toFixed(2) + ' ms';
}

export function pad(s, width) {
  let out = String(s);
  while (out.length < width) {
    out = out + ' ';
  }
  return out;
}

export function padLeft(s, width) {
  let out = String(s);
  while (out.length < width) {
    out = ' ' + out;
  }
  return out;
}

export const RULE = '-'.repeat(72);

// header prints the run's identity. A benchmark number is only
// interpretable next to the conditions that produced it, so the same
// conditions are printed above every result and written into the JSON.
// opts.armed is false for a scenario that has no arms to compare, in
// which case the per arm duration is not a fact about it and printing
// one would just be noise a reader has to learn to ignore.
export function header(title, extra, opts) {
  const o = opts || {};
  const lines = [
    '',
    RULE,
    title,
    RULE,
    'gateway      : ' + CFG.baseURL,
    'direct       : ' + CFG.directURL,
    'model        : ' + CFG.model,
  ];
  if (o.armed !== false) {
    lines.push('duration/arm : ' + CFG.duration);
  }
  if (extra) {
    for (const k of Object.keys(extra)) {
      lines.push(pad(k, 13) + ': ' + extra[k]);
    }
  }
  // The operator, not this script, knows what machine this ran on.
  // bench/run.sh records it beside the results; the reminder is here
  // because a number quoted without it is not a benchmark.
  lines.push('');
  lines.push('Hardware and OS are recorded by bench/run.sh in the .meta.json');
  lines.push('beside this run. A latency figure without that stanza is unusable.');
  lines.push('');
  return lines.join('\n');
}

// loadWarnings surfaces the conditions that void a comparison. They are
// printed at the top of every summary rather than buried, because a
// reader who skips them will quote a number that does not mean what
// they think it means.
export function loadWarnings(data) {
  const out = [];
  const dropped = count(data, 'dropped_iterations');
  if (dropped > 0) {
    out.push(
      'WARNING: k6 dropped ' + dropped + ' iterations. The load generator ' +
      'could not\n         deliver the requested arrival rate, so the arms did ' +
      'not receive\n         equal load and any delta below is void. Lower RATE ' +
      'or raise\n         MAX_VUS and run again.'
    );
  }
  const failed = stat(data, 'http_req_failed', 'rate');
  if (failed !== null && failed > 0) {
    out.push(
      'WARNING: ' + (failed * 100).toFixed(2) + '% of requests failed. Failed ' +
      'requests are usually\n         faster than successful ones, so a delta ' +
      'computed over them\n         understates the real cost.'
    );
  }
  if (out.length === 0) {
    return '';
  }
  return out.join('\n\n') + '\n\n' + RULE + '\n\n';
}

// summaryOutputs routes a rendered summary to stdout and, when run.sh
// asked for them, to files. The raw per sample JSON is written by k6
// itself through --out json=..., which is the artifact that gets
// committed; these are conveniences layered on top of it.
export function summaryOutputs(text, data) {
  const out = { stdout: text };
  const txtPath = __ENV.SUMMARY_TXT;
  if (txtPath) {
    out[txtPath] = text;
  }
  const jsonPath = __ENV.SUMMARY_JSON;
  if (jsonPath) {
    out[jsonPath] = JSON.stringify(data, null, 2);
  }
  return out;
}
