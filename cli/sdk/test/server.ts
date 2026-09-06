import { createServer } from 'node:http';
import type { IncomingMessage, Server, ServerResponse } from 'node:http';
import type { AddressInfo } from 'node:net';

// A real local HTTP server that records every body and its headers, shared by the tests that
// prove wire behaviour (the client's and the funnel's), so the two cannot drift apart.

export interface RecordedRequest {
  body: string;
  headers: Record<string, string | string[] | undefined>;
}

export interface TestServer {
  server: Server;
  bodies: RecordedRequest[];
  url: string;
  close: () => Promise<void>;
}

export type Handler = (req: IncomingMessage, res: ServerResponse, body: string, count: number) => void;

export function startServer(handler: Handler): Promise<TestServer> {
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

export const ok = (res: ServerResponse, accepted: number) => {
  res.writeHead(200, { 'content-type': 'application/json' });
  res.end(JSON.stringify({ accepted }));
};
