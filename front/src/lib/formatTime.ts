/** Copy rule: time is always absolute + relative together, e.g. "14:32, 40 minutes ago". */
export function formatMinutesAgo(minutes: number): string {
  if (minutes < 1) return 'just now';
  if (minutes === 1) return '1 minute ago';
  return `${minutes} minutes ago`;
}

/** Under a minute reads "just now": "0 minutes" is the absence of a duration. */
export function formatDurationMinutes(minutes: number): string {
  if (minutes < 1) return 'just now';
  if (minutes === 1) return '1 minute';
  return `${minutes} minutes`;
}
