import test from 'node:test';
import assert from 'node:assert/strict';
import { scrub } from '../dist/esm/scrub.js';

// Every scrub vector: the secret is replaced by a typed marker and counted.
// A regression here is a security incident, not a bug.

function expectRedacted(input: string, type: string, mustKeep: string[] = []) {
  const r = scrub(input);
  assert.ok(r.counts[type] >= 1, `expected a ${type} hit in: ${input} -> ${r.cleaned}`);
  assert.ok(r.cleaned.includes(`[redacted:${type}:`), `marker missing: ${r.cleaned}`);
  for (const part of mustKeep) {
    assert.ok(r.cleaned.includes(part), `over-scrubbed, lost "${part}": ${r.cleaned}`);
  }
}

test('aws access key', () => {
  expectRedacted('creds AKIAIOSFODNN7EXAMPLE in env', 'aws-key', ['creds', 'in env']);
});

test('gcp api key', () => {
  expectRedacted('key=AIzaSyD4fE8hG2jK1lM3nO5pQ7rS9tU1vW3xY5z', 'gcp-key');
});

test('bearer token', () => {
  expectRedacted('Authorization: Bearer dGhpcy1pcy1hLXNlY3JldC10b2tlbg==', 'bearer', ['Authorization:']);
});

test('jwt by shape', () => {
  expectRedacted(
    'token eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJVadQssw5c expired',
    'jwt',
    ['token', 'expired'],
  );
});

test('db connection string password only', () => {
  const r = scrub('postgres://app:s3cr3t-pw@db.internal:5432/prod timeout');
  assert.equal(r.counts['db-password'], 1);
  assert.ok(r.cleaned.includes('postgres://app:[redacted:db-password:'), r.cleaned);
  assert.ok(r.cleaned.includes('@db.internal:5432/prod'), r.cleaned);
});

test('pem block', () => {
  expectRedacted(
    'dump -----BEGIN RSA PRIVATE KEY-----\nMIIEow==\n-----END RSA PRIVATE KEY----- end',
    'pem',
    ['dump', 'end'],
  );
});

test('card number with luhn', () => {
  expectRedacted('paid with 4242 4242 4242 4242 ok', 'card', ['paid with', 'ok']);
  expectRedacted('card 4111111111111111 declined', 'card');
});

test('luhn rejects plain long numbers', () => {
  const r = scrub('request id 12345678901234 took 5ms');
  assert.equal(r.counts.card, undefined, r.cleaned);
});

test('timestamps and ids survive', () => {
  const r = scrub('2026-08-15T14:22:31.000Z order=982477 status=200 in 154ms');
  assert.deepEqual(r.counts, {});
  assert.equal(r.cleaned, '2026-08-15T14:22:31.000Z order=982477 status=200 in 154ms');
});

test('email', () => {
  expectRedacted('user jane.doe+test@example.co.uk logged in', 'email', ['user', 'logged in']);
});

test('session cookie', () => {
  expectRedacted('Set-Cookie: session=abc123def456ghi789; Path=/', 'cookie', ['Path=/']);
});

test('vendor token prefixes', () => {
  expectRedacted('stripe sk_live_4eC39HqLyjWDarjtT1zdp7dc fail', 'stripe-key', ['stripe', 'fail']);
  expectRedacted('gh ghp_16C7e42F292c6912E7710c838347Ae178B4a done', 'github-token');
  expectRedacted('slack xoxb-1234567890-abcdefghijkl', 'slack-token');
  expectRedacted('own key uc_live_00112233445566778899aabbccddeeff!', 'upcontrol-key');
});

test('multiple secrets in one line all counted', () => {
  const r = scrub('a@b.io paid 4242424242424242 via Bearer c2VjcmV0LXRva2VuLWhlcmU');
  assert.equal(r.counts.email, 1);
  assert.equal(r.counts.card, 1);
  assert.equal(r.counts.bearer, 1);
});

test('a 2MB pathological line completes fast', () => {
  const big = ('x'.repeat(1000) + ' eyJ ' + '9'.repeat(50) + ' ').repeat(1800);
  const started = Date.now();
  scrub(big);
  assert.ok(Date.now() - started < 2000, 'scrub took too long');
});

test('empty and clean strings pass through untouched', () => {
  assert.equal(scrub('').cleaned, '');
  assert.equal(scrub('hello world').cleaned, 'hello world');
});
