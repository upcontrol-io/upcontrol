import { useEffect, useRef } from 'react';
import { IconButton } from '@/components/primitives';
import { ChevronIcon, CloseIcon } from '@/icons';
import { formatDurationMinutes } from '@/lib/formatTime';
import { LogMessage } from './LogMessage';
import type { Incident } from '@/lib/types';
import { TimelineEvent } from './TimelineEvent';
import { ActionBar, type ActionBarItem } from './ActionBar';
import styles from './IncidentCard.module.css';

type IncidentResultTone = 'open' | 'running' | 'still' | 'fixed';

interface IncidentChip {
  /** 'ok' renders a green dot before the text (e.g. "Stripe itself is fine"). */
  dot?: 'ok';
  text: string;
}

interface IncidentPager {
  index: number;
  total: number;
  onPrev: () => void;
  onNext: () => void;
  /** Absent while the shown incident is open: still happening is not
   *  dismissible, and a hidden control beats a dead one. */
  onClose?: () => void;
}

interface IncidentCardProps {
  incident: Incident;
  actions: ActionBarItem[];
  /** Small facts under the timeline — "Stripe itself is fine", "17 people affected". */
  chips?: IncidentChip[];
  /** The strip that closes the loop ("still down" / "fixed, resumed after 43 min"). */
  result?: { tone: IncidentResultTone; text: string };
  /** Steps through the history when there is more than one incident to read. */
  pager?: IncidentPager;
  /** The countdown before this incident leaves the plan's history: real
   *  deletion, so a real fact row; public surfaces never pass it. */
  expiry?: { text: string; cta: string; onCta: () => void };
}

/** The product's centerpiece: red-accented card, timeline and log slice side
 *  by side, an action row, and a result strip answering "and then what". */
export function IncidentCard({ incident: inc, actions, chips = [], result, pager, expiry }: IncidentCardProps) {
  // The timeline pane opens at the end (newest row = the verdict); `scrollTop`
  // rather than scrollIntoView, which would yank the page. Keyed by length.
  const timelineRef = useRef<HTMLDivElement>(null);
  useEffect(() => {
    const el = timelineRef.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [inc.id, inc.timeline.length]);

  // A closed incident is not an emergency, and status is never colour alone:
  // it keeps the layout and drops the red.
  const closed = !inc.ongoing;

  return (
    <div className={[styles.card, closed && styles.cardClosed].filter(Boolean).join(' ')}>
      {pager && (
        <div className={styles.pager}>
          <span className={styles.pagerLabel}>{inc.ongoing ? 'Open now' : 'Earlier'}</span>
          <span className={styles.pagerCount}>
            {pager.index + 1} of {pager.total}
          </span>
          <div className={styles.pagerSpacer} />
          {/* The shared primitive: uc-tap-inline keeps the 32px chip while
              the finger gets a 44px target on a phone. */}
          <IconButton
            size="sm"
            aria-label="Previous incident"
            disabled={pager.index === 0}
            onClick={pager.onPrev}
            icon={<ChevronIcon width={14} height={14} className={styles.pagerPrev} />}
          />
          <IconButton
            size="sm"
            aria-label="Next incident"
            disabled={pager.index === pager.total - 1}
            onClick={pager.onNext}
            icon={<ChevronIcon width={14} height={14} className={styles.pagerNext} />}
          />
          {pager.onClose && (
            <IconButton
              size="sm"
              aria-label="Close incident history"
              onClick={pager.onClose}
              icon={<CloseIcon width={14} height={14} />}
            />
          )}
        </div>
      )}

      <div className={styles.header}>
        <span className={styles.headerTitleWrap}>
          <span className={styles.headerDot} />
          <span className={styles.title}>{inc.title}</span>
        </span>
        <span className={styles.since}>
          since {inc.since} · {formatDurationMinutes(inc.durationMinutes)} · {inc.ongoing ? 'ongoing' : 'closed'}
        </span>
      </div>

      <div className={[styles.body, inc.logSlice.length === 0 && styles.bodyNoLogs].filter(Boolean).join(' ')}>
        <div className={styles.timelinePane}>
          {/* A cropped scroller has to be reachable without a mouse, or the rows
              past the fold exist for pointer users only (WCAG 2.1.1). */}
          <div
            ref={timelineRef}
            className={styles.timeline}
            tabIndex={0}
            role="group"
            aria-label="Incident timeline"
          >
            {inc.timeline.map((entry, index) => (
              <TimelineEvent key={index} entry={entry} />
            ))}
          </div>
          {chips.length > 0 && (
            <div className={styles.chips}>
              {chips.map((chip) => (
                <span key={chip.text} className={styles.chip}>
                  {chip.dot === 'ok' && <span className={styles.chipDot} />}
                  {chip.text}
                </span>
              ))}
            </div>
          )}
        </div>

        {/* No slice to show: an empty <pre> would read as a broken panel,
            so the pane is absent. */}
        {inc.logSlice.length > 0 && (
          <div className={styles.logPane}>
            <span className={styles.logLabel}>Log slice · {inc.logSlice.length} lines, trimmed</span>
            {/* Same colouring as the live stream: the app container logs JSON, so
                the slice does too, and an object has to be readable in both. */}
            {/* Same reachability rule as the timeline: it has always been a
                scroller, and has never been focusable. */}
            <pre className={styles.log} tabIndex={0} role="group" aria-label="Log slice">
              {inc.logSlice.map((line, index) => (
                <span key={index}>
                  <LogMessage text={line} />
                  {'\n'}
                </span>
              ))}
              …
            </pre>
          </div>
        )}
      </div>

      <div className={styles.actions}>
        <ActionBar actions={actions} />
      </div>

      {result && (
        <div className={[styles.result, styles[`result_${result.tone}`]].join(' ')}>
          <span
            className={[
              styles.resultDot,
              styles[`resultDot_${result.tone}`],
              // Amber phases are the "still watching" ones, so they breathe.
              (result.tone === 'open' || result.tone === 'running') && 'uc-pulse',
            ]
              .filter(Boolean)
              .join(' ')}
          />
          <span className={styles.resultText}>{result.text}</span>
        </div>
      )}

      {/* After the story's ending, because it is about the record, not the
          incident: the deletion is real, so the countdown is a fact row. */}
      {expiry && (
        <div className={[styles.wallRow, styles.expiryRow].join(' ')}>
          <span className={styles.wallText}>{expiry.text}</span>
          <button type="button" className={styles.wallButton} onClick={expiry.onCta}>
            {expiry.cta}
          </button>
        </div>
      )}
    </div>
  );
}
