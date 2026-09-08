import test from 'node:test';
import assert from 'node:assert/strict';
import { createFunnel, type FunnelDeps } from '../dist/esm/funnel.js';
import { Client } from '../dist/esm/client.js';
import { startServer } from './server.ts';

// The funnel feed: every step one event named by the step, the person as `uc.actor`, and
// nothing raw about them on the wire. The server counts DISTINCT people from these events —
// the SDK does not dedup, and the same person stepping twice is two events by design.

type Line = Record<string, unknown>;

function fake() {
  const lines: Line[] = [];
  return {
    lines,
    client: {
      enqueue(fields: Record<string, unknown>, level: string) {
        lines.push({ ...fields, level });
      },
    },
  };
}

function deps(client: FunnelDeps['client']): FunnelDeps {
  return { client };
}

function stderr() {
  const lines: string[] = [];
  const original = process.stderr.write;
  process.stderr.write = ((chunk: string | Uint8Array) => {
    lines.push(String(chunk));
    return true;
  }) as typeof process.stderr.write;
  return {
    lines,
    restore() {
      process.stderr.write = original;
    },
  };
}

test('a step sends one event named by the step, carrying uc.actor and uc.funnel', () => {
  const { lines, client } = fake();
  const f = createFunnel('signup', ['visit', 'paid'], deps(client));

  f.step('visit', 'user-1');

  assert.equal(lines.length, 1);
  assert.equal(lines[0].msg, 'visit');
  assert.equal(lines[0].level, 'info');
  assert.equal(lines[0]['uc.actor'], 'user-1');
  assert.equal(lines[0]['uc.funnel'], 'signup');
  assert.equal(lines[0]['uc.event'], true, 'a step IS a track() event');
  assert.ok(!Number.isNaN(Date.parse(String(lines[0].ts))), 'ts parses as a date');
});

test('the same person stepping twice sends two events — the server dedups, not the SDK', () => {
  const { lines, client } = fake();
  const f = createFunnel('signup', ['visit', 'paid'], deps(client));

  f.step('visit', 'user-1');
  f.step('visit', 'user-1');
  f.step('paid', 'user-1');

  assert.deepEqual(
    lines.map((l) => [l.msg, l['uc.actor']]),
    [
      ['visit', 'user-1'],
      ['visit', 'user-1'],
      ['paid', 'user-1'],
    ],
  );
});

test('a request and a string id produce the same attribute shape, and no address is on the wire', () => {
  const { lines, client } = fake();
  const f = createFunnel('anon', ['visit', 'paid'], deps(client));

  const req = { headers: { 'x-forwarded-for': '203.0.113.5, 10.0.0.1', 'user-agent': 'Mozilla/5.0 A' } };
  f.step('visit', req);
  f.step('paid', 'user-42');

  assert.deepEqual(Object.keys(lines[0]).sort(), Object.keys(lines[1]).sort());
  assert.match(String(lines[0]['uc.actor']), /^[0-9a-f]{16}$/, 'a request becomes a fingerprint, not an address');
  const wire = JSON.stringify(lines);
  assert.ok(!wire.includes('203.0.113.5'), 'no raw address on the wire');
  assert.ok(!wire.includes('Mozilla'), 'no user-agent on the wire either');
});

test('every request shape resolves, one person through two shapes is one actor, and a crawler or a missing address is not counted', () => {
  const { lines, client } = fake();
  const f = createFunnel('shapes', ['visit', 'paid'], deps(client));

  const reqA = { headers: { 'x-forwarded-for': '203.0.113.5', 'user-agent': 'Mozilla/5.0 A' } };
  const reqA2 = {
    headers: new Headers({ 'x-forwarded-for': '203.0.113.5, 10.0.0.1', 'user-agent': 'Mozilla/5.0 A' }),
  };
  const reqB = { headers: { 'user-agent': 'Mozilla/5.0 B' }, socket: { remoteAddress: '198.51.100.7' } };
  // Express's req: the framework's own address wins over the proxy header.
  const reqC = { headers: { 'x-forwarded-for': '203.0.113.5', 'user-agent': 'Mozilla/5.0 A' }, ip: '203.0.113.9' };
  const bot = {
    headers: { 'user-agent': 'Mozilla/5.0 (compatible; Googlebot/2.1)' },
    socket: { remoteAddress: '198.51.100.8' },
  };
  const noAddress = { headers: { 'user-agent': 'Mozilla/5.0 D' } };

  const err = stderr();
  for (const who of [reqA, reqA, reqA2, reqB, reqC, bot, noAddress, noAddress]) f.step('visit', who);
  err.restore();

  assert.equal(lines.length, 5, 'three for one person through two shapes, then B and C');
  assert.equal(lines[0]['uc.actor'], lines[1]['uc.actor']);
  assert.equal(lines[0]['uc.actor'], lines[2]['uc.actor'], 'the proxy list first hop and the bare header name one person');
  assert.notEqual(lines[3]['uc.actor'], lines[0]['uc.actor'], 'another address is another person');
  assert.notEqual(lines[4]['uc.actor'], lines[0]['uc.actor'], "the framework's own address wins over the proxy header");
  assert.equal(err.lines.filter((l) => l.includes('no address')).length, 1, 'warned once, counted never');
  assert.ok(!JSON.stringify(lines).includes('198.51.100'), 'no raw address on the wire');
});

test('a bad declaration is a warned no-op, and step() never throws', () => {
  const { lines, client } = fake();
  const err = stderr();

  const bad = createFunnel('too short', ['only'], deps(client));
  bad.step('only', 'u1');

  const good = createFunnel('never throws', ['visit', 'paid'], deps(client));
  good.step('nope', 'u1');
  good.step('nope', 'u2');
  good.step('visit', null as never);
  good.step('visit', 42 as never);
  good.step('visit', {} as never);
  good.step('visit', ' ');
  err.restore();

  assert.equal(lines.length, 0, 'nothing to send: an ignored funnel, an unknown step, no person to name');
  assert.equal(err.lines.filter((l) => l.includes('is ignored')).length, 1);
  assert.equal(err.lines.filter((l) => l.includes('has no step "nope"')).length, 1);
});

test('on the wire a step is an event line like any track()', async () => {
  const srv = await startServer((req, res, body) => {
    res.writeHead(200, { 'content-type': 'application/json' });
    res.end(JSON.stringify({ accepted: body.split('\n').length }));
  });
  const client = new Client({ UPCONTROL_API_KEY: 'uc_live_test', UPCONTROL_ENDPOINT: srv.url });
  const f = createFunnel('wire', ['visit', 'paid'], { client });

  f.step('visit', 'user-1');
  await client.flush();
  await srv.close();

  assert.equal(srv.bodies[0].headers['x-upcontrol-key'], 'uc_live_test');
  const lines = srv.bodies[0].body.split('\n').map((l) => JSON.parse(l) as Line);
  const step = lines.find((l) => l.msg === 'visit');
  assert.equal(step?.['uc.actor'], 'user-1');
  assert.equal(step?.['uc.funnel'], 'wire');
  assert.equal(step?.['uc.event'], true);
});
