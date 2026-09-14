/** The backend widens the bucket as a target's history grows — 900 s out to
 *  24 h, 1800 s to 48 h, 7200 s to 7 days, 43200 s beyond that, each further
 *  widened when the target's own check interval is coarse — and bars are
 *  aligned to the UTC clock (floor(now / bucket) · bucket), never counted
 *  back from page-load. No bucket is a day wide any more, so the axis always
 *  ends "now". */
import type { HealthStatus } from './types';

const DAY_SEC = 86400;

/** The bucket to assume when a backend omits `barSpanSec` — the ladder's first rung. */
export const BASE_SPAN_SEC = 900;

const MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];

/** Status as a word: colour never carries a state alone. */
const BAR_WORD: Record<HealthStatus, string> = {
  ok: 'up',
  check: 'degraded',
  down: 'down',
  nodata: 'no data',
};

/** How far back a whole strip reaches, in words: "24 h", "48 h", "7 days", "30 days". */
export function spanLabel(spanSec: number, count: number): string {
  const total = spanSec * count;
  const hours = total / 3600;
  const days = total / DAY_SEC;
  if (total <= 2 * DAY_SEC && Number.isInteger(hours)) return `${hours} h`;
  if (Number.isInteger(days)) return `${days} days`;
  if (total >= 3600 && Number.isInteger(hours)) return `${hours} h`;
  return `${Math.round(total / 60)} min`;
}

/** When one bucket was, anchored to `nowMs` — pass the backend's own
 *  `updatedAt`, not the browser's clock, or a skewed or merely-slower client
 *  labels bars a bucket off from the ones the server actually sent. Every
 *  bucket prints a local clock range, prefixed with the local date once the
 *  strip spans more than a day (the 7-day and 30-day rungs would otherwise
 *  repeat the same clock range across several bars). */
function bucketTime(index: number, count: number, spanSec: number, nowMs: number): string {
  if (index === count - 1) return 'now';
  const spanMs = spanSec * 1000;
  const current = Math.floor(nowMs / spanMs) * spanMs;
  const start = new Date(current - (count - 1 - index) * spanMs);
  const clock = (d: Date) => `${d.getHours()}:${String(d.getMinutes()).padStart(2, '0')}`;
  const range = `${clock(start)}–${clock(new Date(start.getTime() + spanMs))}`;
  return count * spanSec > DAY_SEC ? `${MONTHS[start.getMonth()]} ${start.getDate()} ${range}` : range;
}

/** A whole tooltip: when, and what it was. `nowMs` anchors the alignment —
 *  the backend's `updatedAt`, never the browser's clock. */
export function bucketLabel(
  index: number,
  count: number,
  spanSec: number,
  status: HealthStatus,
  nowMs: number,
): string {
  return `${bucketTime(index, count, spanSec, nowMs)} · ${BAR_WORD[status]}`;
}
