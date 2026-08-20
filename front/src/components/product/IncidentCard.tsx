import { useEffect, useRef } from 'react';
import { Badge, BrandMark, IconButton } from '@/components/primitives';
import { ChevronIcon, CloseIcon } from '@/icons';
import { useScrollIntoView } from '@/lib/useScrollIntoView';
import { formatDurationMinutes } from '@/lib/formatTime';
import { LogMessage } from './LogMessage';
import { ExplainAnswer } from './ExplainAnswer';
import type { ExplainResult } from '@/lib/client';
import type { Incident } from '@/lib/types';
import { TimelineEvent } from './TimelineEvent';
import { ActionBar, type ActionBarItem } from './ActionBar';
import styles from './IncidentCard.module.css';

export type IncidentResultTone = 'open' | 'running' | 'still' | 'fixed';

/**
 * Triage answers the only question a person has at 23:40 — get up, or sleep.
 * It is the model's read of THIS incident and nothing else: what the evidence
 * shows, the labelled guess at why, a fix, and ordered steps that carry
 * runnable commands.
 */
export interface IncidentTriage {
  loading: boolean;
  /**
   * The model's answer, whole, rendered by the same `ExplainAnswer` the logs
   * panel uses. Absent whenever the read did not run.
   *
   * NAMED REVERSAL (user decision, Aug 19, 2026): the panel no longer carries
   * a code-computed verdict or fact list. It used to open with "This can wait
   * until morning" over three derived sentences, and a reader could not tell
   * which half a machine had actually read — the sentences were true, generic,
   * and drowned the one paragraph that was about THIS incident. The card shows
   * the read or it shows nothing. Do not reintroduce a derived verdict.
   */
  answer?: ExplainResult;
  /**
   * Why there is no answer, when there is none — the SERVER's own words (the
   * throttle's message, a cap, a transport failure), never a sentence this
   * page wrote to fill the space.
   */
  note?: string;
  /**
   * Where the read honestly hit the plan's edge (upgrade-cta-pass.md CTA 2):
   * a fact plus one door, rendered as the triage's quiet footer. Never part of
   * the copied text — Share and the copy buttons forward facts, not upsell.
   * The callback comes from the page: this card stays account-agnostic.
   */
  wall?: { text: string; cta: string; onCta: () => void };
}

export interface IncidentChip {
  /** 'ok' renders a green dot before the text (e.g. "Stripe itself is fine"). */
  dot?: 'ok';
  text: string;
}

export interface IncidentPager {
  index: number;
  total: number;
  onPrev: () => void;
  onNext: () => void;
  /**
   * Leaves the history and returns to the calm state. Absent while the shown
   * incident is open: an incident that is still happening is not something the
   * reader may dismiss, and a close button that only sometimes closes is worse
   * than none. Its absence is why the control is hidden rather than disabled —
   * a control that cannot act is a bug report waiting to be filed.
   */
  onClose?: () => void;
}

export interface IncidentCardProps {
  incident: Incident;
  actions: ActionBarItem[];
  /** Small facts under the timeline — "Stripe itself is fine", "17 people affected". */
  chips?: IncidentChip[];
  /** The strip that closes the loop ("still down" / "fixed, resumed after 43 min"). */
  result?: { tone: IncidentResultTone; text: string };
  /** Shown once the reader asks for it — the "understood" half of the plan's
   *  found out -> understood -> acted loop. */
  triage?: IncidentTriage | null;
  /** Steps through the history when there is more than one incident to read. */
  pager?: IncidentPager;
  /**
   * The countdown before this incident leaves the plan's history
   * (upgrade-cta-pass.md CTA 1) — real deletion, so a real fact row. Passed by
   * the account app only; a public surface never carries upsell.
   */
  expiry?: { text: string; cta: string; onCta: () => void };
}

/**
 * Severity grades the blast radius; the tone only seconds the word
 * (design-brief §2.4 — status is never colour alone, the badge carries text).
 */
const SEVERITY_TONE = { critical: 'down', major: 'check', minor: 'neutral' } as const;

/** The badge's word: the severity capitalized, plus the area when the read named one. */
function severityLabel(severity: 'critical' | 'major' | 'minor', area?: string): string {
  return `${severity[0].toUpperCase()}${severity.slice(1)}${area ? ` · ${area}` : ''}`;
}

/**
 * The product's centerpiece — layout per 05-dashboard.dc.html's incident view:
 * red-accented card, header on --down-bg, timeline and the trimmed log slice
 * side by side on wide screens, an action row, and a result strip that always
 * answers "and then what happened".
 */
export function IncidentCard({ incident: inc, actions, chips = [], result, triage, pager, expiry }: IncidentCardProps) {
  // Explain sits near the foot of a card that is taller than a phone, so the
  // read it produces renders off-screen. Scroll it up to meet the reader.
  const triageRef = useScrollIntoView<HTMLDivElement>(triage && (triage.loading || Boolean(triage.answer) || Boolean(triage.note)));

  // The timeline is cropped to --pane-max, and it is chronological: the row that
  // explains the incident is the newest one, at the bottom. Cropped from the top
  // it would show traffic from before the fault and hide the detector's own
  // verdict, so the pane opens at the end and the reader scrolls back for
  // history, the way a log tail reads. `scrollTop` rather than the shared
  // `useScrollIntoView`: that hook scrolls every scrollable ancestor, which here
  // means yanking the page on mount. Keyed by incident so the pager re-anchors,
  // and by length so a live incident follows its own new rows; a poll that
  // changes neither leaves the reader's scroll position alone.
  const timelineRef = useRef<HTMLDivElement>(null);
  useEffect(() => {
    const el = timelineRef.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [inc.id, inc.timeline.length]);

  // A closed incident is not an emergency, and status is never colour alone
  // (design-brief §2.4): it keeps the layout and drops the red.
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
          {/* The shared primitive rather than three hand-rolled buttons: it
              carries the `uc-tap-inline` contract, so the 32px chip keeps its
              size while the finger gets a 44px target on a phone. The pair used
              to be bespoke 28px boxes with no such overlay, under the touch
              floor in a row that is already tight. */}
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

        {/* An incident whose logs are outside the window (or whose project sends
            none) has no slice to show. An empty <pre> under "Log slice · 0 lines"
            reads as a broken panel, so the pane is absent instead. */}
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

      {triage && (
        <div ref={triageRef} className={styles.triage}>
          {triage.loading ? (
            /* The reading mark, identical to the logs panel's (user decision):
               the same question is being answered, so the same wait is shown.
               The one named exception to "never a spinner" — this is the brand
               mark reading, and it is scoped to the two explain surfaces. */
            <div className={styles.triageChase} role="status" aria-label="Reading the incident">
              <BrandMark variant="chase" size={48} />
            </div>
          ) : (
            /* Keyed by the answer's own text so pressing Explain again replays
               the reveal. `prefers-reduced-motion` flattens it in global.css. */
            <div key={triage.answer?.cause ?? triage.note} className={styles.triageBody}>
              {triage.answer?.severity && (
                /* Wrapper so the badge keeps its natural width: triageBody is a
                   stretching flex column, and a bare Badge child would render as
                   a full-width bar. The wrapper is boxless — only the badge shows. */
                <span className="uc-reveal">
                  <Badge tone={SEVERITY_TONE[triage.answer.severity]}>
                    {severityLabel(triage.answer.severity, triage.answer.area ?? undefined)}
                  </Badge>
                </span>
              )}
              {/* The read itself, through the one renderer the logs panel also
                  uses — same question, same answer shape, so it reads the same
                  in both places. It brings its own copy pair and its own quota
                  footer; the card adds nothing around it. */}
              {triage.answer && (
                <ExplainAnswer
                  result={triage.answer}
                  lines={inc.logSlice}
                  className="uc-reveal"
                />
              )}
              {/* No answer: the server's own words for why, never a sentence
                  this page wrote to fill the space. */}
              {!triage.answer && triage.note && (
                <span className="uc-reveal">
                  <span className={styles.triageGuess}>{triage.note}</span>
                </span>
              )}

              {/* The read's honest edge, last: the answer above stands on its
                  own, and this is the one wall this card state may carry. */}
              {triage.wall && (
                <div className={[styles.wallRow, 'uc-reveal'].join(' ')}>
                  <span className={styles.wallText}>{triage.wall.text}</span>
                  <button type="button" className={styles.wallButton} onClick={triage.wall.onCta}>
                    {triage.wall.cta}
                  </button>
                </div>
              )}
            </div>
          )}
        </div>
      )}

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
