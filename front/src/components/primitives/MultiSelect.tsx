import { useEffect, useRef, useState } from 'react';
import { createPortal } from 'react-dom';
import { ChevronIcon } from '@/icons';
import { Checkbox } from './Checkbox';
import styles from './MultiSelect.module.css';

/** The tier where this control stops being a dropdown (mirrors useDrawer). */
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

interface MultiSelectOption {
  value: string;
  label: string;
  /** Muted figure at the row's trailing edge (e.g. a line count). */
  note?: string;
}

interface MultiSelectProps {
  /** The control's name, spoken inline before the trigger. */
  label: string;
  /** What an empty selection means ("All services"): the first menu row and
   *  the trigger's summary; an empty set IS the unfiltered state. */
  allLabel: string;
  options: MultiSelectOption[];
  selected: ReadonlySet<string>;
  onChange: (next: ReadonlySet<string>) => void;
  /** Applied to the trigger, the way Select takes its control class. */
  className?: string;
}

/** Checkbox dropdown for a small closed set; the menu stays open across
 *  toggles. On a phone: a portalled bottom sheet (a dropdown was untappable). */
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
    // The sheet is portalled outside rootRef, so this listener would read
    // every tap on it as outside; its own scrim dismisses it.
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
