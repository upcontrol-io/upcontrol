import test from 'node:test';
import assert from 'node:assert/strict';
import { createBreakdown, createExperiment, createRetention } from '../dist/esm/analytics.js';
import type { FeedDeps } from '../dist/esm/counters.js';

// The three feeds beside the funnel: every call one event, the person as `uc.actor`, and
// the counting the server's — the SDK sends, it does not dedup, and nothing raw about
// anyone reaches the wire.

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

function deps(client: FeedDeps['client']): FeedDeps {
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

test('an experiment sends one event per expose and per convert, named by the test', () => {
  const { lines, client } = fake();
  const cta = createExperiment('checkout CTA', ['control', 'B'], deps(client));

  cta.expose('control', 'user-1');
  cta.expose('control', 'user-1'); // two exposures are two events: the server dedups
  cta.convert('B', 'user-2');

  assert.deepEqual(
    lines.map((l) => [l.msg, l['uc.actor'], l['uc.variant'], l['uc.stat']]),
    [
      ['checkout CTA', 'user-1', 'control', 'exposed'],
      ['checkout CTA', 'user-1', 'control', 'exposed'],
      ['checkout CTA', 'user-2', 'B', 'converted'],
    ],
  );
  assert.ok(lines.every((l) => l.level === 'info' && l['uc.event'] === true));
});

test('an experiment ignores a variant it does not have, and says so once', () => {
  const { lines, client } = fake();
  const err = stderr();
  const cta = createExperiment('checkout CTA', ['control', 'B'], deps(client));
  cta.expose('C', 'user-1');
  err.restore();

  assert.equal(lines.length, 0);
  assert.equal(err.lines.filter((l) => l.includes('has no variant "C"')).length, 1);
});

test('retention sends one event per seen, the user id as uc.actor', () => {
  const { lines, client } = fake();
  const signups = createRetention('signups', deps(client));

  signups.seen('user-1');
  signups.seen('user-1'); // the id never leaves the process as anything but uc.actor, and no local dedup
  signups.seen('user-2');

  assert.deepEqual(
    lines.map((l) => [l.msg, l['uc.actor']]),
    [
      ['signups', 'user-1'],
      ['signups', 'user-1'],
      ['signups', 'user-2'],
    ],
  );
});

test('retention ignores an unusable id rather than naming an anonymous person', () => {
  const { lines, client } = fake();
  const signups = createRetention('signups', deps(client));
  signups.seen('');
  signups.seen('   ');
  signups.seen(42 as never);
  assert.equal(lines.length, 0);
});

test('a breakdown without a who counts events: no uc.actor on the line', () => {
  const { lines, client } = fake();
  const pages = createBreakdown('page', deps(client));

  pages.value('/pricing');
  pages.value('/pricing');
  pages.value('/app');

  assert.deepEqual(
    lines.map((l) => [l.msg, l.value]),
    [
      ['page', '/pricing'],
      ['page', '/pricing'],
      ['page', '/app'],
    ],
  );
  assert.ok(lines.every((l) => !('uc.actor' in l)), 'an event with no actor is an event count, not a people count');
});

test('a breakdown with a who carries uc.actor — the server dedups people, the SDK sends every event', () => {
  const { lines, client } = fake();
  const pages = createBreakdown('page', deps(client));

  pages.value('/pricing', 'user-1');
  pages.value('/pricing', 'user-1');
  pages.value('/pricing', 'user-2');

  assert.deepEqual(
    lines.map((l) => l['uc.actor']),
    ['user-1', 'user-1', 'user-2'],
  );
  assert.ok(lines.every((l) => l.value === '/pricing'));
});

test('a breakdown passed a request fingerprints the person: no address on the wire', () => {
  const { lines, client } = fake();
  const pages = createBreakdown('page', deps(client));
  const req = { headers: { 'x-forwarded-for': '203.0.113.5', 'user-agent': 'Mozilla/5.0 A' } };

  pages.value('/pricing', req);

  assert.equal(lines.length, 1);
  assert.match(String(lines[0]['uc.actor']), /^[0-9a-f]{16}$/, 'a request becomes a fingerprint, not an address');
  const wire = JSON.stringify(lines);
  assert.ok(!wire.includes('203.0.113.5'), 'no raw address on the wire');
  assert.ok(!wire.includes('Mozilla'), 'no user-agent on the wire either');
});

test('a breakdown value that is empty or over 80 characters sends nothing', () => {
  const { lines, client } = fake();
  const pages = createBreakdown('page', deps(client));

  pages.value('');
  pages.value('   ');
  pages.value('x'.repeat(81));
  pages.value('  /ok  ');

  assert.deepEqual(lines.map((l) => l.value), ['/ok'], 'the value is trimmed; junk is dropped, not sent');
});

test('a bad declaration is a warned no-op and never throws', () => {
  const { lines, client } = fake();
  const err = stderr();

  const noName = createExperiment('', ['a', 'b'], deps(client));
  noName.expose('a', 'u1');
  const longName = createRetention('r'.repeat(61), deps(client));
  longName.seen('u1');
  const oneArm = createExperiment('one arm', ['only'], deps(client));
  oneArm.expose('only', 'u1');
  const dupArm = createExperiment('dup arm', ['a', 'a'], deps(client));
  dupArm.expose('a', 'u1');
  const longBreakdown = createBreakdown('b'.repeat(61), deps(client));
  longBreakdown.value('/x');
  err.restore();

  assert.equal(lines.length, 0);
  assert.equal(err.lines.filter((l) => l.includes('is ignored')).length, 5);
});
