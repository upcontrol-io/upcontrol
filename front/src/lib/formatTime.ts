/** Copy rule (design-brief §4): time is always shown absolute + relative together, e.g. "14:32, 40 minutes ago". */
export function formatMinutesAgo(minutes: number): string {
  if (minutes < 1) return 'just now';
  if (minutes === 1) return '1 minute ago';
  return `${minutes} minutes ago`;
}

/** An incident's age in words (audit §15): under a minute is "just now", not
 *  "0 minutes" — a duration of zero is the absence of one, and the old copy
 *  said it about an incident that was happening right then. */
export function formatDurationMinutes(minutes: number): string {
  if (minutes < 1) return 'just now';
  if (minutes === 1) return '1 minute';
  return `${minutes} minutes`;
}
