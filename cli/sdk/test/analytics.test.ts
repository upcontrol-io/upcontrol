import test, { after } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { createBreakdown, createExperiment, createRetention } from '../dist/esm/analytics.js';
import { createFunnel } from '../dist/esm/funnel.js';
import { REPORTER, type CounterDeps } from '../dist/esm/counters.js';

// The three feeds that joined the funnel on the shared counter machinery: counts that only
// grow, a person counted once per bucket, and nothing on the wire that identifies anyone.

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
  const dir = mkdtempSync(join(tmpdir(), 'uc-analytics-'));
  dirs.push(dir);
  return dir;
}

// Every test awaits stop(), so no save is in flight when the dirs go.
after(() => {
  for (const dir of dirs) rmSync(dir, { recursive: true, force: true });
});

// everyMs is effectively never: each test calls report() itself, so nothing races a timer.
function deps(dir: string, client: CounterDeps['client']): CounterDeps {
  return { client, env: { UPCONTROL_STATE_DIR: dir }, everyMs: 1e9 };
}

/** The readings of one metric, as {bucket: value}, from the newest report in `lines`. */
function readings(lines: Line[], metric: string, key: (labels: Record<string, string>) => string) {
  const out: Record<string, number> = {};
  for (const line of lines) {
    if (line.metric !== metric) continue;
    out[key(line.labels as Record<string, string>)] = line.value as number;
  }
  return out;
}

test('an experiment reports one reading per variant per stat, control first, and counts a person once', async () => {
  const f = fake();
  const cta = createExperiment('checkout CTA', ['control', 'B'], deps(tmp(), f.client));

  cta.expose('control', 'user-1');
  cta.expose('control', 'user-1'); // the same person twice is one exposure
  cta.expose('B', 'user-2');
  cta.convert('B', 'user-2');
  cta.report();

  const at = readings(f.lines, 'experiment', (l) => `${l.variant}|${l.stat}`);
  assert.equal(at['control|exposed'], 1);
  assert.equal(at['control|converted'], 0);
  assert.equal(at['B|exposed'], 1);
  assert.equal(at['B|converted'], 1);

  // `i` is the declaration index, zero-padded: the board orders the card's rows by it and
  // control is the first variant declared. A raw 1 and 10 would sort wrong as text.
  const byVariant = new Map<string, string>();
  for (const line of f.lines) {
    const l = line.labels as Record<string, string>;
    if (line.metric === 'experiment') byVariant.set(l.variant, l.i);
  }
  assert.equal(byVariant.get('control'), '01');
  assert.equal(byVariant.get('B'), '02');

  // Every reading rides level 'metric', so a server-sent keep rate by log level cannot
  // sample a count away.
  assert.ok(f.lines.filter((l) => l.metric === 'experiment').every((l) => l.level === 'metric'));

  await (cta as unknown as { stop(): Promise<void> }).stop();
});

test('an experiment ignores a variant it does not have, and says so once', async () => {
  const f = fake();
  const cta = createExperiment('checkout CTA', ['control', 'B'], deps(tmp(), f.client));
  cta.expose('C', 'user-1');
  cta.report();

  const at = readings(f.lines, 'experiment', (l) => `${l.variant}|${l.stat}`);
  assert.equal(at['C|exposed'], undefined);
  assert.deepEqual(Object.keys(at).sort(), ['B|converted', 'B|exposed', 'control|converted', 'control|exposed']);

  await (cta as unknown as { stop(): Promise<void> }).stop();
});

test('retention puts a first-seen id in week 00 of its cohort and does not count it twice', async () => {
  const f = fake();
  const signups = createRetention('signups', deps(tmp(), f.client));

  signups.seen('user-1');
  signups.seen('user-1');
  signups.seen('user-2');
  signups.report();

  const at = readings(f.lines, 'retention', (l) => `${l.cohort}|${l.week}`);
  const buckets = Object.keys(at);
  assert.equal(buckets.length, 1, 'two people first seen this week share one cohort-week');
  assert.match(buckets[0], /^\d{4}-\d{2}-\d{2}\|00$/);
  assert.equal(at[buckets[0]], 2);

  // The id never leaves the process: the wire carries counts, a cohort date and the
  // process's reporter stamp, nothing else.
  const line = f.lines.find((l) => l.metric === 'retention') as Line;
  assert.deepEqual(Object.keys(line.labels as object).sort(), ['cohort', 'retention', 'uc.reporter', 'week']);
  assert.ok(!JSON.stringify(f.lines).includes('user-1'));

  await (signups as unknown as { stop(): Promise<void> }).stop();
});

test('retention ignores an empty id rather than counting an anonymous person', async () => {
  const f = fake();
  const signups = createRetention('signups', deps(tmp(), f.client));
  signups.seen('');
  signups.seen('   ');
  signups.report();

  assert.equal(f.lines.filter((l) => l.metric === 'retention').length, 0);
  await (signups as unknown as { stop(): Promise<void> }).stop();
});

test('a breakdown counts events, not people', async () => {
  const f = fake();
  const pages = createBreakdown('page', deps(tmp(), f.client));

  pages.value('/pricing');
  pages.value('/pricing');
  pages.value('/app');
  pages.report();

  const at = readings(f.lines, 'breakdown', (l) => l.value);
  assert.equal(at['/pricing'], 2, 'the same value twice is two events');
  assert.equal(at['/app'], 1);
  assert.ok(f.lines.filter((l) => l.metric === 'breakdown').every((l) => (l.labels as Record<string, string>).dimension === 'page'));

  await (pages as unknown as { stop(): Promise<void> }).stop();
});

test('a breakdown passed a who counts a person once, however many events they carry', async () => {
  const f = fake();
  const pages = createBreakdown('page', deps(tmp(), f.client));

  pages.value('/pricing', 'user-1');
  pages.value('/pricing', 'user-1'); // the same person twice is one
  pages.value('/pricing', 'user-1');
  pages.report();

  const at = readings(f.lines, 'breakdown', (l) => l.value);
  assert.equal(at['/pricing'], 1, 'three events, one person');

  await (pages as unknown as { stop(): Promise<void> }).stop();
});

test('a breakdown passed a who counts two people as 2', async () => {
  const f = fake();
  const pages = createBreakdown('page', deps(tmp(), f.client));

  pages.value('/pricing', 'user-1');
  pages.value('/pricing', 'user-2');
  pages.report();

  const at = readings(f.lines, 'breakdown', (l) => l.value);
  assert.equal(at['/pricing'], 2, 'two distinct people');

  await (pages as unknown as { stop(): Promise<void> }).stop();
});

test('a breakdown stops at its distinct-value ceiling instead of growing without limit', async () => {
  const f = fake();
  const pages = createBreakdown('page', deps(tmp(), f.client));

  for (let i = 0; i < 260; i++) pages.value(`/p/${i}`);
  pages.value('/p/0'); // an existing value still counts past the ceiling
  pages.report();

  const at = readings(f.lines, 'breakdown', (l) => l.value);
  assert.equal(Object.keys(at).length, 200);
  assert.equal(at['/p/0'], 2);
  assert.equal(at['/p/259'], undefined);

  await (pages as unknown as { stop(): Promise<void> }).stop();
});

test('readings carry one reporter id per process, shared by every feed', async () => {
  const f = fake();
  const pages = createBreakdown('page', deps(tmp(), f.client));
  const cta = createExperiment('checkout CTA', ['control', 'B'], deps(tmp(), f.client));
  pages.value('/pricing');
  cta.expose('control', 'user-1');
  pages.report();
  cta.report();

  // The stamp names the process, not the feed: the server folds each reporter's counters
  // apart, so two instances of one app must not share a series.
  const reporters = (metric: string) =>
    new Set(f.lines.filter((l) => l.metric === metric).map((l) => (l.labels as Record<string, string>)['uc.reporter']));
  assert.deepEqual(reporters('breakdown'), new Set([REPORTER]));
  assert.deepEqual(reporters('experiment'), new Set([REPORTER]));

  await (pages as unknown as { stop(): Promise<void> }).stop();
  await (cta as unknown as { stop(): Promise<void> }).stop();
});

test('a 0.3.0 funnel state file is read forward, so nobody is counted twice after the upgrade', async () => {
  const dir = tmp();
  const file = join(dir, 'funnels.json');
  // What 0.3.0 wrote: one funnel, one step, one person already counted.
  writeFileSync(
    file,
    JSON.stringify({
      v: 1,
      salt: 'a'.repeat(32),
      funnels: { 'visit to paid': { visit: { seen: 1, ids: ['deadbeefdeadbeef'] } } },
    }),
  );

  const f = fake();
  const funnel = createFunnel('visit to paid', ['visit', 'paid'], deps(dir, f.client) as never);
  // A second person at the same step: the stored one is already counted, so this must land
  // on 2 rather than restarting at 1. It also marks the state dirty, so stop() saves.
  funnel.step('visit', 'user-2');
  funnel.report();

  const at = readings(f.lines, 'funnel', (l) => l.step);
  assert.equal(at.visit, 2, 'the stored count survived the upgrade and the new person added to it');
  assert.equal(at.paid, 0);

  await (funnel as unknown as { stop(): Promise<void> }).stop();

  // Written forward in the new shape, salt kept, so the ids still hash the same way.
  const written = JSON.parse(readFileSync(file, 'utf8')) as { v: number; salt: string };
  assert.equal(written.v, 2);
  assert.equal(written.salt, 'a'.repeat(32));
});
