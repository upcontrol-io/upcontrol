/**
 * How to read a strip of measured uptime buckets.
 *
 * The backend sends `bars` (oldest first) and `barSpanSec` — a day once there is
 * more than a day of history, otherwise the check's own interval, so a brand-new
 * account fills a strip within the hour instead of showing six empty days for a
 * week. Everything a strip says about *time* is derived from that one number, so
 * a row of five-minute buckets can never be labelled "7 days ago".
 *
 * Shared because two surfaces draw the same strips: the public status page and
 * the Dashboard, which shows one line per check exactly as the status page does.
 * Two copies of this arithmetic is how the two would start disagreeing about
 * what the same bar means.
 */
import type { HealthStatus } from './types';

export const DAY_SEC = 86400;

const MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];

/** Status as a word, because colour never carries a state alone (brief §1). */
const BAR_WORD: Record<HealthStatus, string> = {
  ok: 'up',
  check: 'degraded',
  down: 'down',
  nodata: 'no data',
};

/** How far back a whole strip reaches, in words: "7 days", "1 h", "35 min". */
export function spanLabel(spanSec: number, count: number): string {
  const total = spanSec * count;
  if (total % DAY_SEC === 0) return `${total / DAY_SEC} days`;
  if (total >= 3600 && total % 3600 === 0) return `${total / 3600} h`;
  return `${Math.round(total / 60)} min`;
}

/** The right-hand end of the axis: a day-bucketed strip ends today, a finer one now. */
export function spanEndLabel(spanSec: number): string {
  return spanSec >= DAY_SEC ? 'today' : 'now';
}

/** When one bucket was — a date for daily buckets, a clock time for finer ones. */
function bucketTime(index: number, count: number, spanSec: number, end = new Date()): string {
  if (index === count - 1) return spanEndLabel(spanSec);
  const at = new Date(end.getTime() - (count - 1 - index) * spanSec * 1000);
  return spanSec >= DAY_SEC
    ? `${MONTHS[at.getMonth()]} ${at.getDate()}`
    : `${at.getHours()}:${String(at.getMinutes()).padStart(2, '0')}`;
}

/** A whole tooltip: when, and what it was. */
export function bucketLabel(index: number, count: number, spanSec: number, status: HealthStatus): string {
  return `${bucketTime(index, count, spanSec)} · ${BAR_WORD[status]}`;
}
