// Ramp + broadcast benchmark. Opens connections in steps, holds, then publishes once per second over the
// Pusher HTTP API and measures receive latency on a 10 % sample. Container stats come from the Docker API.
//
// env: SERVER (compose service / host), PORT, LABEL, STEPS (comma list), RATE (conn/s), INFLIGHT,
//      WORKERS, HOLD (s), BROADCASTS (count), PROJECT (compose project name),
//      EXTRA_CONTAINERS (comma list of full container names whose memory/CPU is summed with the main one,
//      e.g. a TLS proxy and the server behind it)
import { fork } from 'node:child_process';
import http from 'node:http';
import fs from 'node:fs';
import { createRequire } from 'node:module';

const require = createRequire('/deps/package.json');
const PusherServer = require('pusher');

const env = (k, d) => process.env[k] ?? d;
const SERVER = env('SERVER', 'reverb');
const PORT = Number(env('PORT', SERVER === 'reverb' ? 8080 : 6001));
const LABEL = env('LABEL', SERVER);
const STEPS = env('STEPS', '1000,5000,10000,20000,30000,40000,50000,60000').split(',').map(Number);
const RATE = Number(env('RATE', 500));
const INFLIGHT = Number(env('INFLIGHT', 200));
const WORKERS = Number(env('WORKERS', 8));
const HOLD = Number(env('HOLD', 10));
const BROADCASTS = Number(env('BROADCASTS', 15));
const CONTAINER = `${env('PROJECT', 'arco-ws-bench')}-${SERVER}-1`;
const EXTRA = env('EXTRA_CONTAINERS', '').split(',').map((x) => x.trim()).filter(Boolean);
const APP = { appId: 'bench', key: 'bench-key', secret: 'bench-secret' };

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const log = (...a) => console.log(new Date().toISOString().slice(11, 19), ...a);

function docker(path) {
  return new Promise((resolve, reject) => {
    http.get({ socketPath: '/var/run/docker.sock', path }, (res) => {
      let body = '';
      res.on('data', (c) => (body += c));
      res.on('end', () => {
        try { resolve(JSON.parse(body)); } catch (e) { reject(e); }
      });
    }).on('error', reject);
  });
}

// Same formulas as `docker stats`: CPU % of one core, memory = usage - inactive_file. anon ≈ process RSS.
async function statOne(name) {
  const s = await docker(`/containers/${name}/stats?stream=false`);
  const cpuDelta = s.cpu_stats.cpu_usage.total_usage - s.precpu_stats.cpu_usage.total_usage;
  const sysDelta = s.cpu_stats.system_cpu_usage - s.precpu_stats.system_cpu_usage;
  const ms = s.memory_stats ?? {};
  return {
    cpu: sysDelta > 0 ? (cpuDelta / sysDelta) * s.cpu_stats.online_cpus * 100 : 0,
    mem: ((ms.usage ?? 0) - (ms.stats?.inactive_file ?? 0)) / 2 ** 20,
    anon: (ms.stats?.anon ?? 0) / 2 ** 20,
  };
}

// The main container plus EXTRA_CONTAINERS, summed (e.g. HAProxy + the server behind it).
async function stat() {
  const xs = await Promise.all([CONTAINER, ...EXTRA].map(statOne));
  return { cpu: xs.reduce((a, x) => a + x.cpu, 0), mem: xs.reduce((a, x) => a + x.mem, 0), anon: xs.reduce((a, x) => a + x.anon, 0) };
}

// RSS per process inside the container (in "whole machine" mode: caddy vs the server), from `docker top`.
async function procs() {
  const out = {};
  for (const name of [CONTAINER, ...EXTRA]) {
    try {
      const t = await docker(`/containers/${name}/top?ps_args=-o%20pid,rss,comm`);
      for (const [, rss, comm] of t.Processes) out[comm] = (out[comm] ?? 0) + Number(rss) / 1024;
    } catch {}
  }
  return out;
}

async function statAvg(n) {
  const xs = [];
  for (let i = 0; i < n; i++) xs.push(await stat());
  const avg = (k) => xs.reduce((a, x) => a + x[k], 0) / xs.length;
  return { cpu: avg('cpu'), mem: avg('mem'), anon: avg('anon'), cpuMax: Math.max(...xs.map((x) => x.cpu)) };
}

async function alive() {
  let r = { running: true, oom: false, exit: 0 };
  for (const name of [CONTAINER, ...EXTRA]) {
    const c = await docker(`/containers/${name}/json`);
    if (!c.State?.Running) return { running: false, oom: c.State?.OOMKilled, exit: c.State?.ExitCode };
    r = { running: true, oom: r.oom || c.State?.OOMKilled, exit: 0 };
  }
  return r;
}

const pct = (xs, p) => (xs.length ? xs[Math.min(xs.length - 1, Math.floor((p / 100) * xs.length))] : null);

// --- workers --------------------------------------------------------------------------------------------
const workers = [];
const wstats = [];
let latencyReplies = [];
for (let i = 0; i < WORKERS; i++) {
  const w = fork(new URL('./worker.mjs', import.meta.url), {
    env: { ...process.env, HOST: SERVER, PORT: String(PORT), KEY: APP.key, WORKER_ID: String(i) },
    execArgv: ['--max-old-space-size=4096'],
  });
  w.on('message', (m) => {
    if (m.type === 'stats') wstats[i] = m.stats;
    if (m.type === 'latencies') latencyReplies.push(m.latencies);
  });
  // A worker's IPC channel can close as it exits during teardown; a send to it then
  // emits 'error' (ERR_IPC_CHANNEL_CLOSED). Swallow it so cleanup doesn't crash the run.
  w.on('error', () => {});
  workers.push(w);
}
// Send a command only to workers whose channel is still open.
const tell = (msg) => workers.forEach((w) => { if (w.connected) try { w.send(msg); } catch {} });

function totals() {
  const t = { attempted: 0, subscribed: 0, samplers: 0, live: 0, failed: 0, dropped: 0, pending: 0, queued: 0, errors: {} };
  for (const s of wstats) {
    if (!s) continue;
    for (const k of Object.keys(t)) if (k !== 'errors') t[k] += s[k];
    for (const [k, v] of Object.entries(s.errors)) t.errors[k] = (t.errors[k] || 0) + v;
  }
  return t;
}

const publisher = new PusherServer({ ...APP, host: SERVER, port: String(PORT), useTLS: process.env.TLS === '1' });

async function broadcast() {
  latencyReplies = [];
  tell({ cmd: 'record' });
  const cpuSamples = [];
  let sampling = true;
  const sampler = (async () => {
    while (sampling) cpuSamples.push(await stat());
  })();
  const publishMs = [];
  let publishErrors = 0;
  for (let i = 0; i < BROADCASTS; i++) {
    const started = Date.now();
    try {
      await publisher.trigger('bench', 'tick', { t: Date.now(), seq: i, pad: 'x'.repeat(64) });
      publishMs.push(Date.now() - started);
    } catch (e) {
      publishErrors++;
    }
    await sleep(Math.max(0, 1000 - (Date.now() - started)));
  }
  await sleep(5000);
  sampling = false;
  await sampler;
  tell({ cmd: 'dump' });
  while (latencyReplies.length < WORKERS) await sleep(100);
  const lat = latencyReplies.flat().sort((a, b) => a - b);
  const expected = totals().samplers * (BROADCASTS - publishErrors);
  publishMs.sort((a, b) => a - b);
  return {
    p50: pct(lat, 50),
    p99: pct(lat, 99),
    max: lat.at(-1) ?? null,
    delivered: expected ? lat.length / expected : 0,
    publishP50: pct(publishMs, 50),
    publishErrors,
    cpu: cpuSamples.reduce((a, x) => a + x.cpu, 0) / Math.max(1, cpuSamples.length),
    cpuMax: Math.max(0, ...cpuSamples.map((x) => x.cpu)),
  };
}

// --- run ------------------------------------------------------------------------------------------------
const results = { label: LABEL, server: SERVER, container: CONTAINER, extra: EXTRA, rate: RATE, inflight: INFLIGHT, started: new Date().toISOString(), steps: [] };
const out = `/results/${LABEL}.json`;
const save = () => fs.writeFileSync(out, JSON.stringify(results, null, 2));

await sleep(2000);
results.idle = await statAvg(3);
results.idleProcs = await procs();
log(`${LABEL}: idle mem ${results.idle.mem.toFixed(1)} MiB (anon ${results.idle.anon.toFixed(1)}), cpu ${results.idle.cpu.toFixed(1)} %`);

let requested = 0;
let stop = null;
for (const step of STEPS) {
  const n = step - requested;
  workers.forEach((w, i) => { if (w.connected) try { w.send({ cmd: 'open', n: Math.floor(n / WORKERS) + (i < n % WORKERS ? 1 : 0), rate: RATE / WORKERS, maxInflight: Math.ceil(INFLIGHT / WORKERS) }); } catch {} });
  requested = step;
  const rampStart = Date.now();
  const deadline = rampStart + ((n / RATE) * 4 + 60) * 1000;

  let t;
  while (true) {
    await sleep(1000);
    t = totals();
    const a = await alive();
    if (!a.running) { stop = `container exited (OOMKilled=${a.oom}, exit=${a.exit})`; break; }
    if (t.failed > step * 0.01) { stop = `>1 % of connections refused/failed (${t.failed})`; break; }
    if (t.subscribed + t.failed >= step && t.pending === 0 && t.queued === 0) break;
    if (Date.now() > deadline) { stop = `ramp timeout (${t.subscribed}/${step} subscribed)`; break; }
    if ((Date.now() - rampStart) % 10000 < 1000) log(`  ramp ${t.subscribed}/${step} live=${t.live} failed=${t.failed} pending=${t.pending}`);
  }
  const rampSeconds = (Date.now() - rampStart) / 1000;
  if (stop) { log(`STOP at ${step}: ${stop}`); results.steps.push({ step, stop, totals: t, rampSeconds }); break; }

  const droppedBefore = t.dropped;
  const prev = results.steps.filter((x) => x.memMiB).at(-1);
  await sleep(HOLD * 1000);
  const idle = await statAvg(3);
  const procRss = await procs();
  const bc = await broadcast();
  const after = totals();
  const a = await alive();
  const row = {
    step,
    live: after.live,
    rampSeconds,
    acceptRate: n / rampSeconds,
    memMiB: idle.mem,
    anonMiB: idle.anon,
    kbPerConn: ((idle.mem - results.idle.mem) * 1024) / after.live,
    kbPerConnMarginal: prev ? ((idle.mem - prev.memMiB) * 1024) / (after.live - prev.live) : null,
    procRssMiB: procRss,
    cpuIdle: idle.cpu,
    cpuBroadcast: bc.cpu,
    cpuBroadcastMax: bc.cpuMax,
    p50: bc.p50,
    p99: bc.p99,
    max: bc.max,
    delivered: bc.delivered,
    publishP50: bc.publishP50,
    publishErrors: bc.publishErrors,
    droppedDuringHold: after.dropped - droppedBefore,
    failed: after.failed,
    errors: after.errors,
  };
  results.steps.push(row);
  save();
  log(`step ${step}: live=${row.live} mem=${row.memMiB.toFixed(1)}MiB (${row.kbPerConn.toFixed(1)} KiB/conn, marginal ${row.kbPerConnMarginal?.toFixed(1)}) cpuIdle=${row.cpuIdle.toFixed(0)}% cpuBc=${row.cpuBroadcast.toFixed(0)}%/${row.cpuBroadcastMax.toFixed(0)}% p50=${row.p50}ms p99=${row.p99}ms delivered=${(row.delivered * 100).toFixed(1)}% drops=${row.droppedDuringHold} procs=${JSON.stringify(Object.fromEntries(Object.entries(procRss).map(([k, v]) => [k, Math.round(v)])))}`);
  if (!a.running) { stop = `container exited after broadcast (OOMKilled=${a.oom})`; results.steps.push({ step, stop }); break; }
  if (row.delivered < 0.99 && !results.broadcastLimit) results.broadcastLimit = step;
  if (row.live < step * 0.99) { stop = `unstable at ${step} (live ${row.live})`; log(stop); break; }
}
results.stop = stop ?? 'all steps passed (harness ceiling)';
results.finished = new Date().toISOString();
save();
log(`done: ${results.stop}`);
tell({ cmd: 'close' });
await sleep(2000);
process.exit(0);
