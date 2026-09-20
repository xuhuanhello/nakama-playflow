#!/usr/bin/env node
// Requires Node >=22 and the isolated mock Compose stack. No cloud credentials.
import assert from 'node:assert/strict';
import { createHmac, randomUUID } from 'node:crypto';
import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import { fileURLToPath } from 'node:url';

const base = process.env.NAKAMA_TEST_URL || 'http://127.0.0.1:17350';
const mock = process.env.PLAYFLOW_MOCK_TEST_URL || 'http://127.0.0.1:18090';
for (const url of [base, mock]) assert(['127.0.0.1', 'localhost'].includes(new URL(url).hostname), 'Smoke is restricted to loopback');
const adminToken = 'local-admin-token-for-development-only';
const mockKey = 'test-playflow-key';
const sleep = ms => new Promise(resolve => setTimeout(resolve, ms));
const sockets = [], agents = new Map();
let running = true, simulatorFailure;

async function request(url, { method = 'GET', body, token, headers = {} } = {}) {
  if (token) headers = { ...headers, Authorization: `Bearer ${token}` };
  if (body !== undefined) headers = { ...headers, 'Content-Type': 'application/json' };
  const response = await fetch(url, { method, headers, body: body === undefined ? undefined : JSON.stringify(body), signal: AbortSignal.timeout(4000) });
  if (!response.ok) {
    const err = new Error(`${method} ${new URL(url).pathname}: HTTP ${response.status}`);
    err.status = response.status;
    throw err;
  }
  return response.json();
}
const admin = (path, body) => request(`${base}/fleet/v1/admin/${path}`, { token: adminToken, method: body === undefined ? 'GET' : 'POST', body });
const providerInstances = async () => (await request(`${mock}/__mock/instances`, { headers: { 'api-key': mockKey } })).instances;
async function waitFor(label, fn, timeout = 90000) {
  const end = Date.now() + timeout;
  while (Date.now() < end) {
    if (simulatorFailure) throw simulatorFailure;
    try { const result = await fn(); if (result) return result; }
    catch (err) { if (![404, 409, 503].includes(err.status) && !['TypeError', 'TimeoutError'].includes(err.name)) throw err; }
    await sleep(250);
  }
  throw new Error(`Timed out: ${label}`);
}

async function simulate() {
  while (running) {
    try {
      const instances = await providerInstances();
      for (const instance of instances) {
        if (instance.status === 'stopped') continue;
        const env = instance.environment_variables;
        if (!env?.FLEET_WORKER_ID) continue;
        const wid = env.FLEET_WORKER_ID;
        let agent = agents.get(wid);
        if (!agent) {
          agent = { wid, env, boot: `smoke-${randomUUID()}`, sequence: 0, rooms: new Map(), results: [], seen: new Set(), cancelled: new Set(), metrics: {} };
          agents.set(wid, agent);
        }
        if (!agent.token) {
          const bootstrap = await request(`${base}/fleet/v1/agent/bootstrap`, { method: 'POST', body: { worker_id: wid, bootstrap_token: env.FLEET_BOOTSTRAP_TOKEN, boot_id: agent.boot, build_hash: env.FLEET_BUILD_HASH } });
          agent.token = bootstrap.agent_token;
        }
        const sentResults = [...agent.results];
        const sentRooms = [...agent.rooms.values()].map(room => ({ room_id: room.room_id, state: room.state, user_ids: [...room.user_ids] }));
        const response = await request(`${base}/fleet/v1/agent/heartbeat`, { method: 'POST', token: agent.token, body: { worker_id: wid, boot_id: agent.boot, sequence: ++agent.sequence, ready: true, rooms: sentRooms, command_results: sentResults, metrics: agent.metrics } });
        agent.results = agent.results.filter(item => !sentResults.includes(item));
        for (const room of sentRooms) if (room.state === 'closed') agent.rooms.delete(room.room_id);
        for (const command of response.commands) {
          if (agent.seen.has(command.command_id)) continue;
          agent.seen.add(command.command_id);
          let success = true;
          if (command.type === 'prepare_room') {
            success = command.expires_at > Date.now() / 1000 && !agent.cancelled.has(command.room_id);
            if (success) agent.rooms.set(command.room_id, { room_id: command.room_id, allocation_id: command.allocation_id, roster: command.user_ids, state: 'waiting_players', user_ids: [] });
          } else if (command.type === 'cancel_room') {
            agent.cancelled.add(command.room_id);
            agent.rooms.delete(command.room_id);
          } else if (command.type === 'drain') agent.draining = true;
          else success = false;
          agent.results.push({ command_id: command.command_id, success });
        }
      }
    } catch (err) {
      // The restart test intentionally interrupts the control connection.
      if (![403, 404, 409, 503].includes(err.status) && !['TypeError', 'TimeoutError'].includes(err.name)) simulatorFailure = err;
    }
    await sleep(250);
  }
}

async function player() {
  const session = await request(`${base}/v2/account/authenticate/device?create=true`, { method: 'POST', body: { id: `fleet-smoke-${randomUUID()}` }, headers: { Authorization: `Basic ${Buffer.from('local-server-key:').toString('base64')}` } });
  const user = JSON.parse(Buffer.from(session.token.split('.')[1], 'base64url')).uid;
  const ws = new WebSocket(`${base.replace('http:', 'ws:')}/ws?lang=en&status=true&format=json&token=${encodeURIComponent(session.token)}`);
  sockets.push(ws);
  const messages = [];
  ws.addEventListener('message', event => messages.push(JSON.parse(event.data)));
  await new Promise((resolve, reject) => { ws.addEventListener('open', resolve, { once: true }); ws.addEventListener('error', () => reject(new Error('WebSocket failed')), { once: true }); });
  return { token: session.token, refreshToken: session.refresh_token, user, ws, messages };
}
async function rpc(player, name, payload = {}) {
  let result;
  try {
    result = await request(`${base}/v2/rpc/${name}`, { method: 'POST', token: player.token, body: JSON.stringify(payload) });
  } catch (err) {
    if (err.status !== 401) throw err;
    const refreshed = await request(`${base}/v2/account/session/refresh`, { method: 'POST', body: { token: player.refreshToken }, headers: { Authorization: `Basic ${Buffer.from('local-server-key:').toString('base64')}` } });
    player.token = refreshed.token;
    if (refreshed.refresh_token) player.refreshToken = refreshed.refresh_token;
    result = await request(`${base}/v2/rpc/${name}`, { method: 'POST', token: player.token, body: JSON.stringify(payload) });
  }
  return JSON.parse(result.payload);
}
async function match() {
  const players = await Promise.all([player(), player()]);
  const group = randomUUID().replaceAll('-', '');
  for (const p of players) p.ws.send(JSON.stringify({ cid: 'match-1', matchmaker_add: { min_count: 2, max_count: 2, query: `+properties.smoke_group:${group}`, string_properties: { region: 'local', build_hash: 'local-build', smoke_group: group } } }));
  await waitFor('real Nakama matchmaker hook', () => {
    for (const p of players) for (const message of p.messages) if (message.error) throw new Error(`Nakama websocket error: ${message.error.code}`);
    return players.every(p => p.messages.some(message => message.matchmaker_matched));
  });
  const assignments = await Promise.all(players.map(p => waitFor('signed room assignment', async () => {
    const out = await rpc(p, 'fleet_assignment_get_v1');
    if (['failed', 'expired', 'cancelled'].includes(out.state)) throw new Error(`Allocation terminated: ${out.state}`);
    return out.state === 'assigned' && out.admission_token ? out : undefined;
  })));
  assert.equal(assignments[0].allocation_id, assignments[1].allocation_id);
  assert.notEqual(assignments[0].seat, assignments[1].seat);
  for (let i = 0; i < 2; i++) {
    const out = assignments[i], env = agents.get(out.worker_id).env;
    const [payload, signature] = out.admission_token.split('.');
    assert.equal(createHmac('sha256', Buffer.from(env.FLEET_ADMISSION_KEY, 'base64url')).update(payload).digest('base64url'), signature);
    const claims = JSON.parse(Buffer.from(payload, 'base64url'));
    assert.equal(claims.user_id, players[i].user);
    assert.equal(claims.room_id, out.room_id);
    assert.equal(out.endpoint.transport, 'tugboat-udp');
    assert(out.endpoint.port >= 30000);
  }
  return { players, assignment: assignments[0] };
}
function reportPlaying(game) {
  const a = game.assignment, room = agents.get(a.worker_id).rooms.get(a.room_id);
  room.state = 'playing'; room.user_ids = [...room.roster];
}

try {
  await providerInstances(); // Prove this is the test-only provider before allocating.
  const initial = await admin('status');
  assert(!Object.values(initial.allocations).some(a => !['completed', 'cancelled', 'expired', 'failed'].includes(a.state)), 'Smoke requires an idle isolated fleet; see the explicit reset instructions');
  const simulation = simulate();
  console.log('Authenticating players and exercising real Nakama WebSocket matching…');
  const first = await match(), second = await match(), third = await match();
  assert.equal(first.assignment.worker_id, second.assignment.worker_id, 'Two rooms should share one Linux instance');
  assert.notEqual(first.assignment.room_id, second.assignment.room_id);
  assert.notEqual(first.assignment.worker_id, third.assignment.worker_id, 'Third room should scale out');
  await assert.rejects(rpc(third.players[0], 'fleet_assignment_get_v1', { allocation_id: first.assignment.allocation_id }), err => err.status === 403);
  for (const game of [first, second, third]) reportPlaying(game);
  await waitFor('all games active', async () => { const s = await admin('status'); return [first, second, third].every(g => s.allocations[g.assignment.allocation_id].state === 'active'); });
  console.log('PASS: real matching → three rooms → two physical instances → signed seat tickets');

  const cancelled = await match();
  await rpc(cancelled.players[0], 'fleet_assignment_cancel_v1', { allocation_id: cancelled.assignment.allocation_id });
  await waitFor('cancel cleanup acknowledgement', async () => (await admin('status')).allocations[cancelled.assignment.allocation_id].state === 'cancelled');
  console.log('PASS: cancelling a reservation releases capacity after server acknowledgement');

  const originalIDs = [first, second, third].map(g => g.assignment.allocation_id);
  if (!process.argv.includes('--skip-restart')) {
    console.log('Restarting only the isolated Nakama container to verify durable state…');
    await promisify(execFile)('docker', ['compose', '-f', fileURLToPath(new URL('../deploy/compose.yml', import.meta.url)), 'restart', 'nakama'], { timeout: 60000 });
    await waitFor('persisted assignments and continuing agent heartbeats', async () => {
      const state = await admin('status');
      return originalIDs.every(id => state.allocations[id]?.state === 'active') && [first, third].every(g => state.workers[g.assignment.worker_id]?.state === 'ready');
    });
    const resumed = await rpc(first.players[0], 'fleet_resume_v1', { allocation_id: first.assignment.allocation_id });
    assert.equal(resumed.worker_id, first.assignment.worker_id);
    assert.equal(resumed.room_id, first.assignment.room_id);
    assert.equal(JSON.parse(Buffer.from(resumed.admission_token.split('.')[0], 'base64url')).resume, true);
    console.log('PASS: Nakama restart retains room ownership and resumes signed admission');
  }

  const draining = first.assignment.worker_id;
  await admin('drain', { worker_id: draining });
  await waitFor('drain command acknowledgement', async () => (await admin('status')).workers[draining].drain_ack);
  assert.equal((await admin('status')).workers[draining].state, 'draining');
  const agent = agents.get(draining);
  agent.metrics = { pending_results: 1 };
  for (const room of agent.rooms.values()) { room.state = 'closed'; room.user_ids = []; }
  await waitFor('completed rooms waiting for durable results', async () => {
    const s = await admin('status');
    return s.allocations[first.assignment.allocation_id].state === 'completed' && s.allocations[second.assignment.allocation_id].state === 'completed' && s.workers[draining].metrics.pending_results === 1;
  });
  assert.equal((await admin('status')).workers[draining].state, 'draining');
  agent.metrics = {};
  await waitFor('safe provider stop confirmation', async () => (await admin('status')).workers[draining].state === 'stopped');
  assert.equal((await admin('status')).allocations[third.assignment.allocation_id].state, 'active');
  console.log('PASS: drain preserves live rooms and waits for pending results before stopping');

  const remaining = agents.get(third.assignment.worker_id);
  for (const room of remaining.rooms.values()) { room.state = 'closed'; room.user_ids = []; }
  await admin('drain', { worker_id: remaining.wid });
  await waitFor('final test instance cleanup', async () => (await admin('status')).workers[remaining.wid].state === 'stopped');
  running = false; await simulation;
  console.log('PASS: control-plane integration complete; no real PlayFlow instances were created');
} catch (err) {
  console.error(`FAIL: ${err.message}`);
  process.exitCode = 1;
} finally {
  running = false;
  for (const socket of sockets) socket.close();
}
