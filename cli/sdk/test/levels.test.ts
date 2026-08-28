import test from 'node:test';
import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import type { AddressInfo } from 'node:net';

// Levels on the wire: upcontrolLine builds the line through the real client,
// so this catches a normalizeLevel rewrite before the server ever sees it.

test('a trace line reaches the wire as trace, not debug', async () => {
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
  const { upcontrolLine, flush } = await import('../dist/esm/index.js');

  upcontrolLine('trace', 'deep trace line');
  upcontrolLine('debug', 'plain debug line');
  await flush();
  await new Promise<void>((r) => server.close(() => r()));

  const lines = bodies.join('\n').split('\n').map((l) => JSON.parse(l));
  assert.equal(lines.find((l) => l.msg === 'deep trace line')?.level, 'trace');
  assert.equal(lines.find((l) => l.msg === 'plain debug line')?.level, 'debug');
});
