import test from 'node:test';
import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import type { AddressInfo } from 'node:net';

// UPCONTROL_SERVICE on the wire: the env name rides every line the SDK sends,
// and a service attribute the caller passes wins over it.

test('the env service rides every line, and a caller service wins', async () => {
  const bodies: string[] = [];
  const server = createServer(async (req, res) => {
    let body = '';
    for await (const chunk of req) body += chunk;
    bodies.push(body);
    res.writeHead(200, { 'content-type': 'application/json' });
    res.end(JSON.stringify({ accepted: body.split('\n').length }));
  });
  await new Promise<void>((r) => server.listen(0, '127.0.0.1', r));

  // index.js builds its client from process.env at import time; set first.
  process.env.UPCONTROL_API_KEY = 'uc_live_test';
  process.env.UPCONTROL_ENDPOINT = `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
  process.env.UPCONTROL_SERVICE = 'front';
  const { track, upcontrolLine, flush } = await import('../dist/esm/index.js');
  const { Client } = await import('../dist/esm/client.js');

  track('signup');
  track('signup_api', { service: 'api' });
  upcontrolLine('info', 'hello');
  upcontrolLine('info', 'hello_worker', { service: 'worker' });
  await flush();
  await new Promise<void>((r) => server.close(() => r()));

  const lines = bodies.join('\n').split('\n').map((l) => JSON.parse(l));
  assert.equal(lines.find((l) => l.msg === 'signup')?.service, 'front');
  assert.equal(lines.find((l) => l.msg === 'signup_api')?.service, 'api');
  assert.equal(lines.find((l) => l.msg === 'hello')?.service, 'front');
  assert.equal(lines.find((l) => l.msg === 'hello_worker')?.service, 'worker');
  assert.equal(lines.find((l) => l.msg === 'install_verified')?.service, 'front');
  assert.equal(new Client({ UPCONTROL_SERVICE: ' front ' }).service, 'front');
  assert.equal(new Client({}).service, undefined);
});
