// Protocol check with Laravel Echo + pusher-js: public, private (signed auth), presence (here() with 2 members),
// client events (whisper), plus the HTTP API channel/user queries and a forged-signature rejection.
// The channel authorizer signs locally with the app secret, exactly as Laravel's /broadcasting/auth would.
//
// env: SERVER, PORT
import crypto from 'node:crypto';
import { createRequire } from 'node:module';

const require = createRequire('/deps/package.json');
const Pusher = require('pusher-js');
const PusherServer = require('pusher');
const { default: Echo } = await import('/deps/node_modules/laravel-echo/dist/echo.js');

const SERVER = process.env.SERVER ?? 'reverb';
const PORT = Number(process.env.PORT ?? (SERVER === 'reverb' ? 8080 : 6001));
const APP = { appId: 'bench', key: 'bench-key', secret: 'bench-secret' };
const TLS = process.env.TLS === '1';
const api = new PusherServer({ ...APP, host: SERVER, port: String(PORT), useTLS: TLS });

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const sign = (s, secret = APP.secret) => crypto.createHmac('sha256', secret).update(s).digest('hex');

function echoFor(user, { badSecret = false } = {}) {
  return new Echo({
    broadcaster: 'reverb',
    Pusher,
    key: APP.key,
    wsHost: SERVER,
    wsPort: PORT,
    wssPort: PORT,
    forceTLS: TLS,
    enabledTransports: ['ws'],
    disableStats: true,
    channelAuthorization: {
      customHandler: ({ socketId, channelName }, callback) => {
        const secret = badSecret ? 'wrong-secret' : APP.secret;
        if (channelName.startsWith('presence-')) {
          // Laravel (pusher-php-server) always sends user_id as a string; NUMERIC_IDS=1 sends a JSON number.
          const user_id = process.env.NUMERIC_IDS === '1' ? user.id : String(user.id);
          const channel_data = JSON.stringify({ user_id, user_info: { name: user.name } });
          callback(null, { auth: `${APP.key}:${sign(`${socketId}:${channelName}:${channel_data}`, secret)}`, channel_data });
        } else {
          callback(null, { auth: `${APP.key}:${sign(`${socketId}:${channelName}`, secret)}` });
        }
      },
    },
  });
}

function waitFor(label, register, ms = 5000) {
  const p = new Promise((resolve, reject) => {
    const t = setTimeout(() => reject(new Error(`timeout: ${label}`)), ms);
    register((v) => {
      clearTimeout(t);
      resolve(v);
    });
  });
  p.catch(() => {}); // a sibling wait may fail first; the awaiting check still sees the rejection
  return p;
}

const results = [];
async function check(name, fn) {
  try {
    const detail = await fn();
    results.push({ name, ok: true, detail: detail ?? '' });
  } catch (e) {
    results.push({ name, ok: false, detail: e.message });
  }
}

const alice = echoFor({ id: 1, name: 'Alice' });
const bob = echoFor({ id: 2, name: 'Bob' });
const mallory = echoFor({ id: 3, name: 'Mallory' }, { badSecret: true });

await check('public channel: HTTP publish reaches Echo listener', async () => {
  const got = waitFor('public event', (done) => alice.channel('orders').listen('.OrderShipped', done));
  await sleep(500);
  await api.trigger('orders', 'OrderShipped', { id: 42 });
  const data = await got;
  if (data.id !== 42) throw new Error(`payload ${JSON.stringify(data)}`);
});

await check('private channel: signed auth + HTTP publish', async () => {
  const got = waitFor('private event', (done) => alice.private('user.1').listen('.Paid', done));
  await sleep(500);
  await api.trigger('private-user.1', 'Paid', { amount: 10 });
  const data = await got;
  if (data.amount !== 10) throw new Error(`payload ${JSON.stringify(data)}`);
});

await check('private channel: forged signature is rejected', async () => {
  // Servers answer either with a channel subscription_error or a connection-level pusher:error (e.g. 4009).
  const pusher = mallory.connector.pusher;
  const seen = [];
  pusher.connection.bind('error', (e) => seen.push(`pusher:error ${e?.error?.data?.code ?? ''}`));
  let subscribed = false;
  mallory.private('user.1').subscribed(() => (subscribed = true)).error((e) => seen.push(`subscription_error ${e?.status ?? ''}`));
  await sleep(3000);
  if (subscribed) throw new Error('forged subscription accepted');
  return seen.join(', ') || 'no reply, not subscribed';
});

await check('presence channel: here() lists 2 members, joining() fires', async () => {
  const joined = waitFor('joining', (done) => alice.join('room.1').here(() => {}).joining(done));
  await sleep(700);
  const here = await waitFor('here', (done) => bob.join('room.1').here(done));
  const member = await joined;
  const names = here.map((m) => m.name).sort().join(',');
  if (here.length !== 2 || names !== 'Alice,Bob') throw new Error(`here() = ${JSON.stringify(here)}`);
  if (member.name !== 'Bob') throw new Error(`joining() = ${JSON.stringify(member)}`);
  return `here=[${names}]`;
});

await check('presence channel: HTTP API lists users', async () => {
  const res = await api.get({ path: '/channels/presence-room.1/users' });
  const body = await res.json();
  if (body.users?.length !== 2) throw new Error(JSON.stringify(body));
  return `users=${body.users.map((u) => u.id).join(',')}`;
});

await check('presence channel: leaving() fires on unsubscribe', async () => {
  const left = waitFor('leaving', (done) => alice.join('room.1').leaving(done));
  bob.leave('room.1');
  const member = await left;
  if (member.name !== 'Bob') throw new Error(JSON.stringify(member));
});

await check('client events: whisper on private channel', async () => {
  const got = waitFor('whisper', (done) => bob.private('chat.1').listenForWhisper('typing', done));
  alice.private('chat.1');
  await sleep(1000);
  alice.private('chat.1').whisper('typing', { name: 'Alice' });
  const data = await got;
  if (data.name !== 'Alice') throw new Error(JSON.stringify(data));
});

await check('client events: whisper on presence channel', async () => {
  const got = waitFor('whisper', (done) => bob.join('room.2').listenForWhisper('typing', done));
  alice.join('room.2');
  await sleep(1000);
  alice.join('room.2').whisper('typing', { name: 'Alice' });
  const data = await got;
  if (data.name !== 'Alice') throw new Error(JSON.stringify(data));
});

await check('HTTP API: channel list and occupancy', async () => {
  const res = await api.get({ path: '/channels', params: { info: 'subscription_count' } });
  const body = await res.json();
  const names = Object.keys(body.channels ?? {});
  if (!names.includes('orders')) throw new Error(JSON.stringify(body));
  return `${names.length} channels`;
});

await check('HTTP API: forged publish signature is rejected', async () => {
  const forged = new PusherServer({ ...APP, secret: 'wrong-secret', host: SERVER, port: String(PORT), useTLS: TLS });
  try {
    // Soketi 1.6 answers a bad signature with HTTP 200 and an error body; count it as rejected but say so.
    const res = await forged.trigger('orders', 'OrderShipped', { id: 1 });
    const body = await res.text();
    if (/authentication failed|"code":\s*401/i.test(body)) return `rejected, but with HTTP ${res.status} and body ${body}`;
  } catch (e) {
    return `status ${e.status}`;
  }
  throw new Error('publish accepted');
});

for (const r of results) console.log(`${r.ok ? 'PASS' : 'FAIL'}  ${r.name}${r.detail ? `  (${r.detail})` : ''}`);
console.log(JSON.stringify({ server: SERVER, passed: results.filter((r) => r.ok).length, total: results.length, results }));
[alice, bob, mallory].forEach((e) => e.disconnect());
process.exit(0);
