// Winston bridge without a winston dependency (the SDK ships zero runtime
// deps). Winston v3 pipes info objects into transports as object-mode
// writables; UpcontrolTransport is a plain stream.Writable that reads the
// info's level/message and mirrors them. It also implements log() for code
// paths that duck-call transports directly.

import { Writable } from 'node:stream';
import { upcontrolLine } from './index.js';

const MESSAGE = Symbol.for('message');

interface WinstonInfo {
  level?: string;
  message?: unknown;
  [MESSAGE]?: string;
  [key: string]: unknown;
}

export class UpcontrolTransport extends Writable {
  constructor() {
    super({ objectMode: true });
  }

  override _write(info: WinstonInfo, _enc: string, cb: (err?: Error | null) => void): void {
    this.mirror(info);
    cb();
  }

  log(info: WinstonInfo, cb?: () => void): void {
    this.mirror(info);
    if (cb) cb();
  }

  private mirror(info: WinstonInfo): void {
    try {
      if (!info || typeof info !== 'object') return;
      const { level, message, ...rest } = info;
      delete (rest as Record<PropertyKey, unknown>)[MESSAGE as unknown as string];
      upcontrolLine(level ?? 'info', typeof message === 'string' ? message : info[MESSAGE] ?? String(message), rest);
    } catch {
      /* a mirror failure must never reach winston */
    }
  }
}
