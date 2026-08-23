# "My cron / queue / background jobs die silently"

Three events do all the work: `job_started` (T2), `job_done` (T2),
`job_failed` (T1 - may wake), plus `heartbeat` (T1) for schedule-based
absence detection. `job` is the required label everywhere - a stable job name,
not an invocation ID.

## BullMQ

```ts
worker.on('completed', (job) => track('job_done', { job: job.queueName, duration_ms: Date.now() - job.processedOn! }));
worker.on('failed', (job, err) => track('job_failed', { job: job?.queueName ?? 'unknown', error_type: err.name }));
```

## node-cron / plain setInterval jobs

Wrap the body's OUTCOME, not the schedule:

```ts
cron.schedule('0 3 * * *', async () => {
  track('job_started', { job: 'nightly-report' });
  try {
    await runNightlyReport();
    track('job_done', { job: 'nightly-report' });
  } catch (err) {
    track('job_failed', { job: 'nightly-report', error_type: (err as Error).name });
    throw err; // rethrow - rule 3: never change error behavior
  }
});
```

Note: this is the ONE pattern where a try/catch may be added - around a job body
you are instrumenting whole, with an unconditional rethrow. Inside existing
code, rule 2 (no wrappers) still applies.

## heartbeat - absence detection for schedules

`heartbeat` says "I ran"; upcontrol alerts when a beat MISSES its declared
window. Place it as the last line of a successful run:

```ts
track('heartbeat', { job: 'nightly-report' });
```

The schedule is declared in the upcontrol app (the check's cron expression),
not in code - tell the user to add a heartbeat check named after the job in
their dashboard, or offer to open it for them.

For a shell-level cron that is not a Node process, the SDK does not apply; the
heartbeat can be a curl in the crontab line's tail (the endpoint accepts a
plain POST):

```
0 3 * * * /usr/local/bin/backup.sh && curl -fsS -X POST -H "X-UpControl-Key: $UPCONTROL_API_KEY" -d '{"event":"heartbeat","name":"backup"}' https://upcontrol.io/i
```

(Read the key from the environment; never paste it into the crontab literally.)

## What "silently" means here

A job that stops being scheduled at all produces no `job_failed` - nothing runs
to fail. That is what `heartbeat` exists for: absence is the signal. When the
user's complaint is "it died and nobody noticed for a week", heartbeat is the
fix; `job_failed` alone would not have caught it.
