// Winston bridge with no winston dependency: a plain object-mode Writable that
// reads each info's level/message. log() covers paths that call it directly.

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
