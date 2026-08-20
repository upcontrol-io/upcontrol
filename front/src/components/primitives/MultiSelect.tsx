import { useEffect, useRef, useState } from 'react';
import { createPortal } from 'react-dom';
import { ChevronIcon } from '@/icons';
import { Checkbox } from './Checkbox';
import styles from './MultiSelect.module.css';

/**
 * Mirrors BOTTOM_BAR_MAX_WIDTH in lib/useDrawer.ts and the `max-width: 700px`
 * block below: the tier where this control stops being a dropdown.
 */
const PHONE = '(max-width: 700px)';

function usePhone(): boolean {
  const [phone, setPhone] = useState(
    () => typeof window !== 'undefined' && window.matchMedia?.(PHONE).matches === true,
  );
  useEffect(() => {
    const mql = window.matchMedia?.(PHONE);
    if (!mql) return;
    const onChange = () => setPhone(mql.matches);
    mql.addEventListener('change', onChange);
    return () => mql.removeEventListener('change', onChange);
  }, []);
  return phone;
}

export interface MultiSelectOption {
  value: string;
  label: string;
  /** Muted figure at the row's trailing edge (e.g. a line count). */
  note?: string;
}

export interface MultiSelectProps {
  /** The control's name, spoken inline before the trigger. */
  label: string;
  /**
   * What an empty selection means ("All services") — the first row of the
   * menu and the trigger's summary when nothing is picked. An empty set IS
   * the unfiltered state: there is no way to select nothing, because a
   * control that can only show nothing is a bug report waiting to be filed.
   */
  allLabel: string;
  options: MultiSelectOption[];
  selected: ReadonlySet<string>;
  onChange: (next: ReadonlySet<string>) => void;
  /** Applied to the trigger, the way Select takes its control class. */
  className?: string;
}

/**
 * A checkbox dropdown for picking a subset of a small closed set. The menu
 * stays open across toggles — picking three services is one visit, not three.
 *
 * On a phone it is a bottom sheet instead, and that is not a flourish. As a
 * dropdown it was unusable below 700px: anchored `right: 0` to its trigger, a
 * 200px menu started at x = -11 on a 390px screen, and the logs panel's own
 * `overflow: hidden` (there for its rounded corners) clipped what was left, so
 * `elementFromPoint` over a row returned the page behind it. The menu rendered
 * and could not be tapped. A portalled sheet leaves the clipping ancestor
 * entirely and lands under the thumb, which is the same answer MoreSheet gives
 * for the same question.
 */
export function MultiSelect({
  label,
  allLabel,
  options,
  selected,
  onChange,
  className,
}: MultiSelectProps) {
  const [open, setOpen] = useState(false);
  const phone = usePhone();
  const rootRef = useRef<HTMLDivElement>(null);
  const triggerRef = useRef<HTMLButtonElement>(null);

  useEffect(() => {
    if (!open) return;
    function onPointerDown(event: PointerEvent) {
      if (!rootRef.current?.contains(event.target as Node)) setOpen(false);
    }
    function onKeyDown(event: KeyboardEvent) {
      if (event.key === 'Escape') {
        setOpen(false);
        triggerRef.current?.focus();
      }
    }
    // The sheet is portalled to the body, so it is NOT inside rootRef and this
    // listener would read every tap on it as a tap outside. Its own scrim is
    // what dismisses it; Escape stays either way.
    if (!phone) document.addEventListener('pointerdown', onPointerDown);
    document.addEventListener('keydown', onKeyDown);
    return () => {
      document.removeEventListener('pointerdown', onPointerDown);
      document.removeEventListener('keydown', onKeyDown);
    };
  }, [open, phone]);

  // Narrowing the window while the dropdown is open would otherwise leave a
  // sheet nobody asked for, and widening it a dropdown with no anchor.
  useEffect(() => setOpen(false), [phone]);

  function toggle(value: string) {
    const next = new Set(selected);
    if (!next.delete(value)) next.add(value);
    // Every box ticked is the same question as none: the full set. Collapsing
    // it keeps "all" a single state instead of two that drift apart.
    onChange(next.size === options.length ? new Set() : next);
  }

  const summary =
    selected.size === 0
      ? allLabel
      : selected.size === 1
        ? (options.find((option) => selected.has(option.value))?.label ?? allLabel)
        : `${selected.size} of ${options.length}`;

  // One set of rows, two containers: the dropdown and the sheet differ in where
  // they hang, never in what they offer.
  const rows = (
    <>
      <Checkbox
        className={styles.row}
        checked={selected.size === 0}
        onChange={() => onChange(new Set())}
        label={<span className={styles.rowLabel}>{allLabel}</span>}
      />
      <div className={styles.divider} />
      {options.map((option) => (
        <Checkbox
          key={option.value}
          className={styles.row}
          checked={selected.has(option.value)}
          onChange={() => toggle(option.value)}
          label={
            <>
              <span className={styles.rowLabel}>{option.label}</span>
              {option.note && <span className={styles.note}>{option.note}</span>}
            </>
          }
        />
      ))}
    </>
  );

  return (
    <div ref={rootRef} className={styles.wrap}>
      <span className={styles.label}>{label}</span>
      <button
        ref={triggerRef}
        type="button"
        className={[styles.trigger, className].filter(Boolean).join(' ')}
        aria-haspopup="true"
        aria-expanded={open}
        onClick={() => setOpen((current) => !current)}
      >
        {summary}
        <ChevronIcon
          className={[styles.chevron, open && styles.chevronOpen].filter(Boolean).join(' ')}
          width={10}
          height={10}
        />
      </button>
      {open && !phone && (
        <div className={styles.pop} role="group" aria-label={label}>
          {rows}
        </div>
      )}
      {open &&
        phone &&
        createPortal(
          <div className={styles.overlay} onClick={() => setOpen(false)}>
            <div
              role="group"
              aria-label={label}
              className={[styles.sheet, 'uc-glass'].join(' ')}
              onClick={(event) => event.stopPropagation()}
            >
              <span className={styles.grabber} aria-hidden="true" />
              <span className={styles.sheetTitle}>{label}</span>
              {rows}
            </div>
          </div>,
          document.body,
        )}
    </div>
  );
}
