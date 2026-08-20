import test from 'node:test';
import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { once } from 'node:events';
import type { IncomingMessage, Server, ServerResponse } from 'node:http';
import type { AddressInfo } from 'node:net';
import { Client } from '../dist/esm/client.js';

// Wire behavior against a real local HTTP server: NDJSON body, key header,
// install_verified on the first batch only, byte-identical retry, 401 kill.

interface RecordedRequest {
  body: string;
  headers: Record<string, string | string[] | undefined>;
}

interface TestServer {
  server: Server;
  bodies: RecordedRequest[];
  url: string;
  close: () => Promise<void>;
}

type Handler = (
  req: IncomingMessage,
  res: ServerResponse,
  body: string,
  count: number,
) => void;

function startServer(handler: Handler): Promise<TestServer> {
  const bodies: RecordedRequest[] = [];
  const server = createServer(async (req, res) => {
    let body = '';
    for await (const chunk of req) body += chunk;
    bodies.push({ body, headers: req.headers });
    handler(req, res, body, bodies.length);
  });
  return new Promise<TestServer>((resolve) => {
    server.listen(0, '127.0.0.1', () => {
      resolve({
        server,
        bodies,
        url: `http://127.0.0.1:${(server.address() as AddressInfo).port}`,
        close: () => new Promise<void>((r) => server.close(() => r())),
      });
    });
  });
}

const ok = (res: ServerResponse, accepted: number) => {
  res.writeHead(200, { 'content-type': 'application/json' });
  res.end(JSON.stringify({ accepted }));
};

test('first batch carries install_verified, second does not', async () => {
  const srv = await startServer((req, res, body) => ok(res, body.split('\n').length));
  const c = new Client({ UPCONTROL_API_KEY: 'uc_live_test', UPCONTROL_ENDPOINT: srv.url });

  c.enqueue({ msg: 'payment_succeeded', provider: 'stripe' }, 'info');
  await c.flush();
  c.enqueue({ msg: 'second_line' }, 'info');
  await c.flush();
  await srv.close();

  assert.equal(srv.bodies.length, 2);
  const first = srv.bodies[0].body.split('\n').map((l) => JSON.parse(l));
  assert.equal(first[0].msg, 'install_verified');
  assert.ok(first[0].version, 'install_verified carries version');
  assert.ok(first[0].env, 'install_verified carries env');
  assert.equal(first[1].msg, 'payment_succeeded');
  assert.equal(srv.bodies[0].headers['x-upcontrol-key'], 'uc_live_test');
  assert.ok(String(srv.bodies[0].headers['x-upcontrol-sdk']).startsWith('js/'));
  assert.equal(srv.bodies[0].headers['content-type'], 'application/x-ndjson');

  const second = srv.bodies[1].body.split('\n').map((l) => JSON.parse(l));
  assert.equal(second.length, 1);
  assert.equal(second[0].msg, 'second_line');
});

test('failed batch retries byte-identical', async () => {
  let calls = 0;
  const srv = await startServer((req, res, body) => {
    calls++;
    if (calls === 1) {
      res.writeHead(500);
      res.end();
      return;
    }
    ok(res, body.split('\n').length);
  });
  const c = new Client({ UPCONTROL_API_KEY: 'uc_live_test', UPCONTROL_ENDPOINT: srv.url });

  c.enqueue({ msg: 'line_a' }, 'info');
  await c.flush(); // attempt 1 fails, batch parked
  c.enqueue({ msg: 'line_b' }, 'info'); // arrives AFTER the failed batch froze
  await c.flush(); // retries the frozen batch, then sends line_b
  await c.flush();
  await srv.close();

  assert.ok(srv.bodies.length >= 2);
  assert.equal(srv.bodies[0].body, srv.bodies[1].body, 'retry must be byte-identical');
  const all = srv.bodies.map((b) => b.body).join('\n');
  assert.ok(all.includes('line_b'), 'the late line still arrives');
});

test('401 disables sending and drops the batch', async () => {
  const srv = await startServer((req, res) => {
    res.writeHead(401);
    res.end();
  });
  const c = new Client({ UPCONTROL_API_KEY: 'uc_live_bad', UPCONTROL_ENDPOINT: srv.url });
  c.enqueue({ msg: 'x' }, 'info');
  await c.flush();
  c.enqueue({ msg: 'y' }, 'info');
  await c.flush();
  await srv.close();
  assert.equal(srv.bodies.length, 1, 'no more requests after a 401');
});

test('no key: enqueue is a warned no-op and flush resolves', async () => {
  const c = new Client({});
  assert.equal(c.hasKey, false);
  c.enqueue({ msg: 'x' }, 'info');
  await c.flush(); // must resolve immediately, nothing buffered
});

test('unreachable endpoint: flush resolves without throwing', async () => {
  const c = new Client({ UPCONTROL_API_KEY: 'uc_live_test', UPCONTROL_ENDPOINT: 'http://127.0.0.1:9' });
  c.enqueue({ msg: 'x' }, 'info');
  await c.flush();
});

test('sampling instruction from the receipt is honored', async () => {
  const srv = await startServer((req, res, body, n) => {
    res.writeHead(200, { 'content-type': 'application/json' });
    res.end(JSON.stringify({ accepted: body.split('\n').length, sampling: { level: 'debug', keep: 0 } }));
  });
  const c = new Client({ UPCONTROL_API_KEY: 'uc_live_test', UPCONTROL_ENDPOINT: srv.url });
  c.enqueue({ msg: 'first' }, 'info');
  await c.flush();
  for (let i = 0; i < 50; i++) c.enqueue({ msg: 'noise' + i }, 'debug');
  c.enqueue({ msg: 'kept' }, 'info');
  await c.flush();
  await srv.close();
  const all = srv.bodies.map((b) => b.body).join('\n');
  assert.ok(!all.includes('noise'), 'debug lines must be sampled out at keep=0');
  assert.ok(all.includes('kept'), 'info lines still flow');
});

test('buffer eviction reports a drop line', async () => {
  const srv = await startServer((req, res, body) => ok(res, body.split('\n').length));
  const c = new Client({ UPCONTROL_API_KEY: 'uc_live_test', UPCONTROL_ENDPOINT: srv.url });
  const big = 'z'.repeat(64 * 1024);
  // ~9 MB queued while nothing can flush synchronously: oldest lines evict.
  for (let i = 0; i < 144; i++) c.enqueue({ msg: 'bulk' + i, pad: big }, 'info');
  await c.flush();
  while (srv.bodies.length && srv.bodies[srv.bodies.length - 1].body.length > 0 && (await c.flush(), false));
  await c.flush();
  await srv.close();
  const all = srv.bodies.map((b) => b.body).join('\n');
  assert.ok(all.includes('upcontrol_buffer_dropped'), 'eviction must be announced, never silent');
});

test('track-style enqueue never throws on garbage', () => {
  const c = new Client({ UPCONTROL_API_KEY: 'uc_live_test', UPCONTROL_ENDPOINT: 'http://127.0.0.1:9' });
  const cyclic: Record<string, unknown> = {};
  cyclic.self = cyclic;
  c.enqueue({ msg: 'x', bad: cyclic }, 'info'); // JSON.stringify throws inside; enqueue must not
  c.enqueue({ msg: undefined }, 'info');
});
