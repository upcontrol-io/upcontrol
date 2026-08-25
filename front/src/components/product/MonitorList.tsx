import { useState, useEffect } from 'react';
import { Link } from 'react-router-dom';
import { Callout, LoadError, SkeletonPanel } from '@/components/primitives';
import { CheckIcon } from '@/icons';
import type { Monitor } from '@/lib/types';
import { invalidateApiData, useApiData } from '@/lib/useApiData';
import { monitors as monitorsApi } from '@/lib/client';
import { normalizeTarget } from '@/lib/normalizeTarget';
import { MonitorOnboarding } from './MonitorOnboarding';
import styles from './MonitorList.module.css';

/** The checks this instance runs: list, create form, delete confirm. One
 *  kind (Website); SSL and domain expiry are free facts on every row. */

// `30s` is gone: the probe is sized against a minute. The floor is the
// server's; every option here is at or above it.
const INTERVALS = ['1m', '5m', '30m', '1h'];

/** The part of the target the name does not already say, or '' when the two
 *  are the same address. */
function restOfTarget(monitor: Monitor): string {
  const bare = monitor.target.replace(/^https?:\/\//, '').replace(/\/$/, '');
  return bare === monitor.name ? '' : monitor.target;
}

/** SSL and domain expiry, halves without a date left out (a fresh check has
 *  neither; a row of dashes reads as a missing fact). */
function expiryLabel(monitor: Monitor): string {
  if (!monitor.expiry) return '';
  return [monitor.expiry.ssl, monitor.expiry.domain]
    .filter((part) => part && !/—\s*$/.test(part))
    .join(' · ');
}

function statusDot(status: Monitor['status']) {
  if (status === 'ok') return { bg: 'var(--ok)', border: 'none', text: 'up' };
  if (status === 'down') return { bg: 'var(--down)', border: 'none', text: 'down' };
  return { bg: 'transparent', border: '1px solid var(--nodata)', text: 'no data yet' };
}

/** Ids for the optimistic row, replaced by the server's own on the re-read. */
let nextId = 1;

interface MonitorListProps {
  /** When present, rows carry a "public" checkbox: the monitor is also a
   *  status-page component; monitor id IS the component key. */
  publish?: {
    shown: Record<string, boolean>;
    onToggle: (key: string, next: boolean) => void;
  };
}

export function MonitorList({ publish }: MonitorListProps) {
  const {
    live: monitorsLive,
    loading: monitorsLoading,
    failed: monitorsFailed,
    data: liveMonitors,
  } = useApiData('monitors', () => monitorsApi.list());
  // Starts empty and stays empty until the server answers: an unanswered read
  // is not a list of checks.
  const [monitors, setMonitors] = useState<Monitor[]>([]);
  const [userEdited] = useState(false);
  useEffect(() => {
    if (!userEdited && monitorsLive && liveMonitors) {
      setMonitors(liveMonitors as Monitor[]);
    }
  }, [liveMonitors, monitorsLive, userEdited]);
  const [formOpen, setFormOpen] = useState(false);
  const [newTarget, setNewTarget] = useState('');
  const [newKeyword, setNewKeyword] = useState('');
  const [newName, setNewName] = useState('');
  const [newInterval, setNewInterval] = useState('5m');
  // Deleting a check takes its history with it, so it asks first, with the
  // light inline confirm (ConfirmPanel's PIN belongs to other actions).
  const [removingId, setRemovingId] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);

  const canCreate = newTarget.trim().length > 3;

  // Every mutation goes to the API first and re-reads the list from it. The
  // optimistic row is only ever a preview: the server's answer is the truth.
  function refetchMonitors() {
    invalidateApiData('monitors', 'overview', 'statusPage', 'plan');
  }

  function removeMonitor(id: string) {
    setMonitors((current) => current.filter((monitor) => monitor.id !== id));
    setRemovingId(null);
    void monitorsApi
      .delete(id)
      .then(refetchMonitors)
      .catch(() => {
        // Every failure is said out loud, an unreachable backend included.
        setError('Could not delete that check. It is still there. Try again.');
      });
  }

  function createMonitor() {
    // The scheme is added and the trailing slash dropped before anything
    // travels, so spelling never decides whether the check creates.
    const target = normalizeTarget(newTarget);
    // What the reader typed wins; empty falls back to the address. The row
    // hides the URL line when the name already IS the address (restOfTarget).
    const name = newName.trim() || target.replace(/^https?:\/\//, '').replace(/\/$/, '');
    const draft: Monitor = {
      id: `mon_${nextId++}`,
      type: 'Website',
      name,
      target,
      status: 'nodata',
      interval: newInterval,
      keyword: newKeyword.trim() || undefined,
    };
    setMonitors((current) => [...current, draft]);
    setFormOpen(false);
    setNewTarget('');
    setNewKeyword('');
    setNewName('');
    setError(null);

    void monitorsApi
      .create({
        type: 'Website',
        name,
        target: draft.target,
        keyword: draft.keyword,
        interval: newInterval,
      } as never)
      .then(refetchMonitors)
      .catch((err: unknown) => {
        // Take the draft back off the screen: it does not exist. The message
        // is the server's own words when it sent any.
        setMonitors((current) => current.filter((monitor) => monitor.id !== draft.id));
        const message = err instanceof Error && err.message !== 'unauthorized' ? err.message : '';
        setError(message || 'Could not create that check. Nothing was saved.');
      });
  }

  if (monitorsLoading) {
    return (
      <div className={styles.wrap}>
        <SkeletonPanel rows={3} label="Loading checks" />
      </div>
    );
  }

  // Settled with nothing. An empty table here would say "you run no checks",
  // which is a claim about the account rather than about the request.
  if (monitorsFailed) {
    return (
      <div className={styles.wrap}>
        <LoadError what="your checks" onRetry={() => invalidateApiData('monitors')} />
      </div>
    );
  }

  // A live-and-empty list is a first run: the scan onboarding replaces the
  // table; the re-read after Start watching flips this screen by itself.
  if (monitorsLive && monitors.length === 0 && !formOpen) {
    return (
      <div className={styles.wrap}>
        <MonitorOnboarding />
        <div className={styles.headRow}>
          <span className={styles.count}>Prefer to add one by hand?</span>
          <button type="button" className={styles.newButton} onClick={() => setFormOpen(true)}>
            New monitor
          </button>
        </div>
        {error && (
          <Callout tone="danger" title="That did not save">
            {error}
          </Callout>
        )}
      </div>
    );
  }

  return (
    <div className={styles.wrap}>
      <div className={styles.headRow}>
        <span className={styles.count}>{monitors.length} checks</span>
        <div className={styles.spacer} />
        <button type="button" className={styles.newButton} onClick={() => setFormOpen((o) => !o)}>
          New monitor
        </button>
      </div>

      {/* A write that failed has to say so on the screen that asked for it — the
          list is re-read from the server, so silence would look like success. */}
      {error && (
        <Callout tone="danger" title="That did not save">
          {error}
        </Callout>
      )}

      <div className={[styles.table, publish && styles.tablePublish].filter(Boolean).join(' ')}>
        <div className={styles.tableHead}>
          {publish && <span>Public</span>}
          <span>Type</span>
          <span>Target</span>
          <span>Status</span>
          <span>Interval</span>
        </div>
        {monitors.map((monitor) => {
          const dot = statusDot(monitor.status);
          const shown = publish ? publish.shown[monitor.id] !== false : false;
          return (
            <div key={monitor.id} className={styles.row}>
              <div className={styles.tableRow}>
                {publish && (
                  <button
                    type="button"
                    role="checkbox"
                    aria-checked={shown}
                    aria-label={`Publish ${monitor.name} on the status page`}
                    className={[styles.publishCell, 'uc-tap-inline'].join(' ')}
                    onClick={() => publish.onToggle(monitor.id, !shown)}
                  >
                    <span className={[styles.checkbox, shown && styles.checkboxChecked].filter(Boolean).join(' ')}>
                      {shown && <CheckIcon width={8} height={8} />}
                    </span>
                  </button>
                )}
                <span className={styles.type}>{monitor.type}</span>
                <div className={styles.targetCell}>
                  <Link to={`/monitors/${monitor.id}`} className={styles.name}>
                    {monitor.name}
                  </Link>
                  {restOfTarget(monitor) && <span className={styles.target}>{restOfTarget(monitor)}</span>}
                  {monitor.keyword && <span className={styles.target}>must contain "{monitor.keyword}"</span>}
                  {/* Watched without being asked, so it reads as a fact, not a
                      setting — and only where there is a date (zero is silence). */}
                  {expiryLabel(monitor) && <span className={styles.expiry}>{expiryLabel(monitor)}</span>}
                </div>
                <span className={styles.status}>
                  <span className={styles.statusDot} style={{ background: dot.bg, border: dot.border }} />
                  {dot.text}
                </span>
                <span className={styles.interval}>{monitor.interval}</span>
                <button
                  type="button"
                  className={[styles.remove, 'uc-tap-inline'].join(' ')}
                  aria-label={`Delete ${monitor.name}`}
                  onClick={() => setRemovingId((current) => (current === monitor.id ? null : monitor.id))}
                >
                  ×
                </button>
              </div>
              {removingId === monitor.id && (
                <div className={styles.confirmStrip}>
                  <span className={styles.confirmText}>Stop watching this, and delete its history?</span>
                  <button type="button" className={styles.confirmYes} onClick={() => removeMonitor(monitor.id)}>
                    Delete
                  </button>
                  <button type="button" className={styles.confirmNo} onClick={() => setRemovingId(null)}>
                    Keep
                  </button>
                </div>
              )}
            </div>
          );
        })}
      </div>

      {formOpen && (
        <div className={styles.form}>
          <span className={styles.formLabel}>Watch a website</span>
          <div className={styles.detail}>
            <label className={styles.field}>
              <span className={styles.fieldLabel}>URL</span>
              <input
                className={styles.fieldInput}
                placeholder="https://example.com"
                value={newTarget}
                onChange={(event) => setNewTarget(event.target.value)}
              />
            </label>
            {/* Was its own monitor type. It is one optional condition. */}
            <label className={styles.field}>
              <span className={styles.fieldLabel}>Page must contain (optional)</span>
              <input
                className={styles.fieldInput}
                placeholder="e.g. Add to cart"
                value={newKeyword}
                onChange={(event) => setNewKeyword(event.target.value)}
              />
            </label>

            <div className={styles.field}>
              <span className={styles.fieldLabel}>How often</span>
              <div className={styles.intervalPicker}>
                {INTERVALS.map((interval) => (
                  <button
                    key={interval}
                    type="button"
                    className={[styles.intervalButton, newInterval === interval && styles.intervalButtonActive].filter(Boolean).join(' ')}
                    onClick={() => setNewInterval(interval)}
                  >
                    {interval}
                  </button>
                ))}
              </div>
              <span className={styles.fieldHint}>Every minute is the fastest we run a check.</span>
            </div>

            <label className={styles.field}>
              <span className={styles.fieldLabel}>Name</span>
              <input
                className={styles.fieldInput}
                placeholder="Marketing site"
                value={newName}
                onChange={(event) => setNewName(event.target.value)}
              />
              <span className={styles.fieldHint}>Optional. The address is used when this is empty.</span>
            </label>

            <button type="button" className={styles.createButton} disabled={!canCreate} onClick={createMonitor}>
              Create monitor
            </button>
          </div>
        </div>
      )}
    </div>
  );
}
