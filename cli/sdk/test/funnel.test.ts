import test, { after } from 'node:test';
import assert from 'node:assert/strict';
import { copyFileSync, existsSync, mkdtempSync, readFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { createFunnel, type FunnelDeps } from '../dist/esm/funnel.js';
import { Client } from '../dist/esm/client.js';
import { startServer } from './server.ts';

// The funnel feed: counters that only grow, one person counted once per step, nothing that
// identifies a person on the wire or on disk.

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

const dirs: string[] = [];

function tmp(): string {
  const dir = mkdtempSync(join(tmpdir(), 'uc-funnel-'));
  dirs.push(dir);
  return dir;
}

// Every test awaits stop(), so no save is in flight when the dirs go.
after(() => {
  for (const dir of dirs) rmSync(dir, { recursive: true, force: true });
});

function deps(dir: string, extra: Partial<FunnelDeps> = {}): FunnelDeps {
  return { client: fake().client, env: { UPCONTROL_STATE_DIR: dir }, everyMs: 1e9, ...extra };
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

const labels = (line: Line) => line.labels as Record<string, string>;
const last = (lines: Line[], step: string) => [...lines].reverse().find((l) => labels(l).step === step);

test('a funnel reports every step as a counter the moment it is declared', async () => {
  const steps = ['visit', 'signup', 'checkout', 'paid'];
  const { lines, client } = fake();
  const f = createFunnel('declared', steps, deps(tmp(), { client }));

  assert.equal(lines.length, 4);
  lines.forEach((line, i) => {
    assert.equal(line.metric, 'funnel');
    assert.equal(line.value, 0);
    assert.equal(line.level, 'metric');
    // Zero-padded, so twelve steps sort as strings the way they were declared.
    assert.deepEqual(line.labels, { funnel: 'declared', step: steps[i], i: String(i + 1).padStart(2, '0') });
    assert.ok(!Number.isNaN(Date.parse(String(line.ts))), 'ts parses as a date');
  });
  await f.stop();
});

test('a person counts once per step, a crawler never, every request shape works, and no address is not counted', async () => {
  const { lines, client } = fake();
  const f = createFunnel('one person', ['visit', 'signup'], deps(tmp(), { client }));

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
  f.step('signup', 'user-42');
  f.step('signup', 'user-42');
  f.step('signup', ' ');
  f.report();
  await f.stop();
  err.restore();

  assert.equal(last(lines, 'visit')?.value, 3);
  assert.equal(last(lines, 'signup')?.value, 1);
  assert.equal(err.lines.filter((l) => l.includes('no address')).length, 1, 'warned once, counted never');
});

test('the people are kept as salted hashes, and a restart keeps the counts', async () => {
  const reqA = { headers: { 'x-forwarded-for': '203.0.113.5', 'user-agent': 'Mozilla/5.0 A' } };
  const reqB = { headers: { 'user-agent': 'Mozilla/5.0 B' }, socket: { remoteAddress: '198.51.100.7' } };

  const dirA = tmp();
  const a = fake();
  const fa = createFunnel('restart', ['visit', 'signup'], deps(dirA, { client: a.client }));
  fa.step('visit', reqA);
  fa.step('visit', reqB);
  fa.step('signup', 'user-42');
  fa.report();
  await fa.stop();

  const fileA = join(dirA, 'funnels.json');
  assert.ok(existsSync(fileA), 'the state file is written');
  const text = readFileSync(fileA, 'utf8');
  for (const secret of ['Mozilla', '203.0.113.5', '198.51.100.7', 'user-42']) {
    assert.ok(!text.includes(secret), `the state file must not carry ${secret}`);
  }
  assert.ok(text.includes('"salt"'), 'the salt is stored with the counts');

  const dirB = tmp();
  copyFileSync(fileA, join(dirB, 'funnels.json'));
  const b = fake();
  const fb = createFunnel('restart', ['visit', 'signup'], deps(dirB, { client: b.client }));
  assert.equal(b.lines[0].value, 2, 'the counts survive the restart');
  fb.step('visit', reqA);
  fb.report();
  assert.equal(last(b.lines, 'visit')?.value, 2, 'the id and the salt survived the round trip');
  await fb.stop();
});

test('the cap evicts the oldest and the counter never goes down', async () => {
  const { lines, client } = fake();
  const err = stderr();
  const f = createFunnel('capped', ['visit', 'signup'], deps(tmp(), { client, maxIds: 2 }));
  for (const id of ['u1', 'u2', 'u3']) f.step('visit', id);
  f.report();
  f.step('visit', 'u1');
  f.report();
  await f.stop();
  err.restore();

  // Two lines per report, the visit line first: declaration, then the two reports.
  assert.equal(lines[2].value, 3);
  assert.equal(lines[4].value, 4, 'an evicted visitor who returns counts again, the counter never shrinks');
  assert.equal(err.lines.filter((l) => l.includes('keeps the last 2 people')).length, 1);
});

test('a bad declaration is a warned no-op, and step() never throws', async () => {
  const dir = tmp();
  const { lines, client } = fake();
  const err = stderr();

  const bad = createFunnel('too short', ['only'], deps(dir, { client }));
  bad.step('only', 'u1');
  bad.report();

  const good = createFunnel('never throws', ['visit', 'signup'], deps(dir, { client }));
  good.step('nope', 'u1');
  good.step('nope', 'u2');
  good.step('visit', null as never);
  good.step('visit', 42 as never);
  good.step('visit', {} as never);
  good.report();
  await good.stop();
  err.restore();

  assert.equal(err.lines.filter((l) => l.includes('is ignored')).length, 1);
  assert.equal(err.lines.filter((l) => l.includes('has no step "nope"')).length, 1);
  assert.equal(lines.length, 4, 'the ignored funnel sends nothing: two steps, declared and reported once');
  assert.equal(last(lines, 'visit')?.value, 0);
});

test('the same name in one process is one funnel', async () => {
  const dir = tmp();
  const { client } = fake();
  const one = createFunnel('single', ['visit', 'signup'], deps(dir, { client }));
  const two = createFunnel('single', ['visit', 'signup'], deps(dir, { client }));
  assert.equal(one, two);
  const three = createFunnel('single', ['visit', 'paid'], deps(dir, { client }));
  assert.equal(three, one, 'the first declaration wins, whatever the second says');
  await one.stop();
});

test('on the wire the funnel is metric readings that sampling never drops', async () => {
  const srv = await startServer((req, res, body) => {
    res.writeHead(200, { 'content-type': 'application/json' });
    res.end(JSON.stringify({ accepted: body.split('\n').length, sampling: { level: 'info', keep: 0 } }));
  });
  const client = new Client({ UPCONTROL_API_KEY: 'uc_live_test', UPCONTROL_ENDPOINT: srv.url });
  const f = createFunnel('wire', ['visit', 'signup', 'checkout', 'paid'], deps(tmp(), { client }));

  f.report();
  await client.flush();
  f.report();
  await client.flush();
  await f.stop();
  await srv.close();

  assert.equal(srv.bodies[0].headers['x-upcontrol-key'], 'uc_live_test');
  const second = srv.bodies[1].body.split('\n').map((l) => JSON.parse(l));
  assert.equal(second.length, 4);
  assert.ok(
    second.every((l) => l.metric === 'funnel'),
    'a metric reading is never sampled out',
  );
});
