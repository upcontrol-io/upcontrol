// The auto entry: app_started on boot, unhandled_exception on crashes, flush on loop drain.
// Installs only observers that do not change process behavior; no signal handlers.

import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { track, flush } from './index.js';

function appVersion(): string {
  try {
    const raw = readFileSync(join(process.cwd(), 'package.json'), 'utf8');
    const v = JSON.parse(raw).version;
    return typeof v === 'string' && v ? v : '0';
  } catch {
    return '0';
  }
}

const g = globalThis as { __upcontrolAuto?: boolean };
if (!g.__upcontrolAuto) {
  g.__upcontrolAuto = true;

  track('app_started', {
    version: appVersion(),
    env: process.env.NODE_ENV || 'production',
  });

  process.on('uncaughtExceptionMonitor', (err: unknown) => {
    try {
      const e = err instanceof Error ? err : new Error(String(err));
      track('unhandled_exception', { error_type: e.name || 'Error' });
    } catch {
      /* never interfere with the crash path */
    }
  });

  process.on('beforeExit', () => {
    void flush();
  });
}
