// Load worker: holds a share of the pusher-js clients and reports to bench.mjs over IPC.
import { createRequire } from 'node:module';

const require = createRequire('/deps/package.json');
const Pusher = require('pusher-js');

const { HOST, PORT, KEY, CHANNEL = 'bench', SAMPLE_EVERY = '10' } = process.env;
const sampleEvery = Number(SAMPLE_EVERY);

const clients = [];
const stats = { attempted: 0, subscribed: 0, samplers: 0, live: 0, failed: 0, dropped: 0, pending: 0, errors: {} };
let recording = false;
let latencies = [];
let queue = 0;          // connections still to open
let ratePerSec = 0;
let maxInflight = 50;

function openOne(index) {
  stats.attempted++;
  stats.pending++;
  let settled = false;
  const settle = (ok) => {
    if (settled) return;
    settled = true;
    stats.pending--;
    ok ? stats.subscribed++ : stats.failed++;
  };

  const client = new Pusher(KEY, {
    wsHost: HOST,
    wsPort: Number(PORT),
    wssPort: Number(PORT),
    forceTLS: process.env.TLS === '1',
    enabledTransports: ['ws'],
    cluster: 'mt1',
    disableStats: true,
  });
  clients.push(client);

  // A connected client that leaves "connected" counts as dropped (pusher-js reconnects on its own).
  let wasConnected = false;
  client.connection.bind('state_change', ({ current }) => {
    if (current === 'connected') {
      wasConnected = true;
      stats.live++;
    } else if (wasConnected && (current === 'connecting' || current === 'unavailable' || current === 'disconnected')) {
      wasConnected = false;
      stats.live--;
      stats.dropped++;
    }
    if (current === 'unavailable' || current === 'failed') settle(false);
  });
  client.connection.bind('error', (err) => {
    const code = err?.error?.data?.code ?? err?.type ?? 'unknown';
    stats.errors[code] = (stats.errors[code] || 0) + 1;
  });

  const channel = client.subscribe(CHANNEL);
  const sampled = index % sampleEvery === 0;
  channel.bind('pusher:subscription_succeeded', () => {
    if (sampled && !settled) stats.samplers++;
    settle(true);
  });
  channel.bind('pusher:subscription_error', () => settle(false));
  setTimeout(() => settle(false), 30_000);

  if (sampled) {
    channel.bind('tick', (data) => {
      if (recording) latencies.push(Date.now() - data.t);
    });
  }
}

// Token bucket: open up to ratePerSec per second, never more than maxInflight pending handshakes.
let budget = 0;
setInterval(() => {
  if (queue <= 0) return;
  budget = Math.min(budget + ratePerSec / 20, ratePerSec);
  while (budget >= 1 && queue > 0 && stats.pending < maxInflight) {
    budget--;
    queue--;
    openOne(clients.length);
  }
}, 50);

setInterval(() => process.send({ type: 'stats', stats: { ...stats, queued: queue, open: clients.length } }), 500);

process.on('message', (msg) => {
  if (msg.cmd === 'open') {
    queue += msg.n;
    ratePerSec = msg.rate;
    maxInflight = msg.maxInflight;
  } else if (msg.cmd === 'record') {
    latencies = [];
    recording = true;
  } else if (msg.cmd === 'dump') {
    recording = false;
    process.send({ type: 'latencies', latencies });
    latencies = [];
  } else if (msg.cmd === 'close') {
    clients.forEach((c) => c.disconnect());
    setTimeout(() => process.exit(0), 1000);
  }
});
