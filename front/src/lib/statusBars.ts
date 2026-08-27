/** Everything a strip says about time derives from `barSpanSec`; shared by the
 *  status page and the Dashboard so both read the same bars the same way. One bar
 *  covers five minutes normally, the check's own interval when that is longer, and
 *  more on the top rungs where the bar count is capped — the strip reaches back that
 *  times its bar count, and never more than a day. */
import type { HealthStatus } from './types';

/** The bucket to assume when a backend omits `barSpanSec` — the ladder's first rung. */
export const BASE_SPAN_SEC = 300;

/** Status as a word: colour never carries a state alone. */
const BAR_WORD: Record<HealthStatus, string> = {
  ok: 'up',
  check: 'degraded',
  down: 'down',
  nodata: 'no data',
};

/** How far back a whole strip reaches, in words: "24 h", "1 h", "35 min". Never days — the
 *  window ladder stops at 24 h, and "1 days" is what the days branch used to print there. */
export function spanLabel(spanSec: number, count: number): string {
  const total = spanSec * count;
  if (total >= 3600 && total % 3600 === 0) return `${total / 3600} h`;
  return `${Math.round(total / 60)} min`;
}

/** When one bucket was — a clock time, since no bucket is a day wide any more. */
function bucketTime(index: number, count: number, spanSec: number): string {
  if (index === count - 1) return 'now';
  const at = new Date(Date.now() - (count - 1 - index) * spanSec * 1000);
  return `${at.getHours()}:${String(at.getMinutes()).padStart(2, '0')}`;
}

/** A whole tooltip: when, and what it was. */
export function bucketLabel(index: number, count: number, spanSec: number, status: HealthStatus): string {
  return `${bucketTime(index, count, spanSec)} · ${BAR_WORD[status]}`;
}
