// R1 load check: keeps the provisioned 20-panel dashboard (fdb-load, 5 s
// refresh) open in headless Chromium and samples the plugin process, Grafana,
// fdbserver and FDB's own counters. Linux only (reads the host's /proc).
//
//   node tests/load/run-load.mjs [--minutes 5] [--idle 60] [--refresh 5s] [--grafana http://localhost:3000]
//        [--grafana-container birdayz-foundationdb-datasource] [--fdb-container gde2e-fdb-1]
import { execFileSync } from 'node:child_process';
import { readFileSync, writeFileSync } from 'node:fs';
import { chromium } from '@playwright/test';

const args = Object.fromEntries(
  process.argv.slice(2).reduce((acc, a, i, all) => (a.startsWith('--') ? [...acc, [a.slice(2), all[i + 1]]] : acc), [])
);
const minutes = Number(args.minutes ?? 5);
const idleSeconds = Number(args.idle ?? 60);
const refresh = args.refresh ?? '5s';
const grafana = args.grafana ?? 'http://localhost:3000';
const grafanaContainer = args['grafana-container'] ?? 'birdayz-foundationdb-datasource';
const fdbContainer = args['fdb-container'] ?? 'gde2e-fdb-1';
const SAMPLE_MS = 5000;
const CLK_TCK = Number(execFileSync('getconf', ['CLK_TCK']).toString().trim());

/** Host PID of the first process in container whose command line matches re. */
function hostPid(container, re) {
  const out = execFileSync('docker', ['top', container, '-eo', 'pid,args']).toString().split('\n').slice(1);
  const line = out.find((l) => re.test(l));
  if (!line) {
    throw new Error(`no process matching ${re} in ${container}`);
  }
  return Number(line.trim().split(/\s+/)[0]);
}

function procSample(pid) {
  const stat = readFileSync(`/proc/${pid}/stat`, 'utf8');
  const fields = stat.slice(stat.lastIndexOf(')') + 2).split(' ');
  const ticks = Number(fields[11]) + Number(fields[12]); // utime + stime
  const rssKiB = Number(/VmRSS:\s+(\d+)/.exec(readFileSync(`/proc/${pid}/status`, 'utf8'))[1]);
  return { t: Date.now(), ticks, rssMiB: rssKiB / 1024 };
}

function fdbStatus() {
  const raw = execFileSync('docker', [
    'exec',
    fdbContainer,
    'fdbcli',
    '--exec',
    'status json',
    '--timeout',
    '10',
  ]).toString();
  const c = JSON.parse(raw).cluster;
  const procs = Object.values(c.processes ?? {});
  // Cumulative counters: rates come from their deltas over a phase, because the
  // hz fields average a few seconds and miss most of a 30 s refresh's bursts.
  const w = c.workload ?? {};
  return {
    t: Date.now(),
    txnStarted: w.transactions?.started?.counter ?? 0,
    reads: w.operations?.reads?.counter ?? 0,
    keysRead: w.keys?.read?.counter ?? 0,
    bytesRead: w.bytes?.read?.counter ?? 0,
    limitedBy: c.qos?.performance_limited_by?.name ?? '',
  };
}

const pids = {
  plugin: hostPid(grafanaContainer, /gpx_foundationdb/),
  grafana: hostPid(grafanaContainer, /grafana server|grafana-server/),
  // The entrypoint's shell names fdbserver too; the server is the process it execs.
  fdbserver: hostPid(fdbContainer, /^\s*\d+\s+fdbserver /),
};

const samples = { idle: [], load: [] };
const fdb = { idle: [], load: [] };
let phase = 'idle';
const sampler = setInterval(() => {
  samples[phase].push(Object.fromEntries(Object.entries(pids).map(([k, pid]) => [k, procSample(pid)])));
}, SAMPLE_MS);
const fdbSampler = setInterval(() => {
  try {
    fdb[phase].push(fdbStatus());
  } catch (e) {
    console.error('status json:', e.message);
  }
}, 15000);

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
console.log(`pids ${JSON.stringify(pids)}; idle ${idleSeconds}s, then the dashboard for ${minutes} min`);
await sleep(idleSeconds * 1000);

phase = 'load';
const browser = await chromium.launch();
const page = await browser.newPage({ viewport: { width: 1920, height: 1200 } });
const queries = [];
const started = new Map();
page.on('request', (r) => r.url().includes('/api/ds/query') && started.set(r, Date.now()));
page.on('requestfinished', async (r) => {
  if (!started.has(r)) {
    return;
  }
  const resp = await r.response();
  queries.push({ ms: Date.now() - started.get(r), status: resp?.status() ?? 0 });
});
// A panel query still running when the next refresh starts is aborted by the browser.
page.on('requestfailed', (r) => started.has(r) && queries.push({ ms: Date.now() - started.get(r), status: 'aborted' }));
await page.goto(`${grafana}/d/fdb-load?orgId=1&refresh=${refresh}&from=now-6h&to=now`);
await sleep(minutes * 60 * 1000);
await browser.close();
clearInterval(sampler);
clearInterval(fdbSampler);

// CPU between consecutive samples, in percent of one core.
function cpuSeries(list, key) {
  const out = [];
  for (let i = 1; i < list.length; i++) {
    const a = list[i - 1][key];
    const b = list[i][key];
    out.push((((b.ticks - a.ticks) / CLK_TCK) * 1000 * 100) / (b.t - a.t));
  }
  return out;
}
const pct = (xs, p) => {
  const s = [...xs].sort((a, b) => a - b);
  return s.length ? s[Math.min(s.length - 1, Math.floor((p / 100) * s.length))] : NaN;
};
const mean = (xs) => (xs.length ? xs.reduce((a, b) => a + b, 0) / xs.length : NaN);
const r1 = (x) => Math.round(x * 10) / 10;

const summary = { setup: { minutes, idleSeconds, sampleMs: SAMPLE_MS, refresh, panels: 20 }, phases: {} };
for (const ph of ['idle', 'load']) {
  const s = samples[ph];
  const out = {};
  for (const key of Object.keys(pids)) {
    const cpu = cpuSeries(s, key);
    const rss = s.map((x) => x[key].rssMiB);
    out[key] = {
      cpuPctMean: r1(mean(cpu)),
      cpuPctP95: r1(pct(cpu, 95)),
      rssMiBStart: r1(rss[0]),
      rssMiBEnd: r1(rss.at(-1)),
      rssMiBMax: r1(Math.max(...rss)),
    };
  }
  const f = fdb[ph];
  const rate = (key, scale = 1) => {
    if (f.length < 2) {
      return NaN;
    }
    return r1((f.at(-1)[key] - f[0][key]) / scale / ((f.at(-1).t - f[0].t) / 1000));
  };
  out.fdb = {
    samples: f.length,
    txnStartedPerS: rate('txnStarted'),
    readsPerS: rate('reads'),
    keysReadPerS: rate('keysRead'),
    MiBReadPerS: rate('bytesRead', 1048576),
    limitedBy: [...new Set(f.map((x) => x.limitedBy))].join(','),
  };
  summary.phases[ph] = out;
}
const ms = queries.filter((q) => q.status === 200).map((q) => q.ms);
const byStatus = {};
for (const q of queries) {
  byStatus[q.status] = (byStatus[q.status] ?? 0) + 1;
}
summary.queries = {
  requests: queries.length,
  perMinute: r1(queries.length / minutes),
  byStatus,
  // Latency of the completed (200) requests.
  msP50: pct(ms, 50),
  msP95: pct(ms, 95),
  msMax: Math.max(...ms),
};
const out = args.out ?? 'test-results/load-summary.json';
writeFileSync(out, JSON.stringify(summary, null, 2) + '\n');
console.log(JSON.stringify(summary, null, 2));
