import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import type {
	KeyboardEvent as ReactKeyboardEvent,
	PointerEvent as ReactPointerEvent,
} from "react";
import styles from "./LogTimeline.module.css";

export interface LogVolumeBucket {
	minute: string;
	level: string;
	lines: number;
}

/** One sub-minute bucket. `bucket`, not `minute`: at five seconds that name
 *  would be a claim about precision nobody measured. */
export interface LogDetailBucket {
	bucket: string;
	level: string;
	lines: number;
}

/** Sub-minute counts for the range the reader is holding, when they asked for
 *  them. `bucketSeconds` is the width the server actually used, which is not
 *  always the width requested — it snaps a request up to a size it can answer,
 *  so nothing below this can be drawn however far the reader zooms. */
export interface LogVolumeDetail {
	bucketSeconds: number;
	buckets: LogDetailBucket[];
}

/** A viewport over the window: epoch milliseconds, half-open `[from, to)`. */
export interface LogRange {
	from: number;
	to: number;
}

export interface LogTimelineProps {
	buckets: LogVolumeBucket[];
	/**
	 * Finer counts for the committed range, when one is held and the reader
	 * asked. It never replaces `buckets`: those span the whole ring and are what
	 * the domain and every zoomed-out column are read from, and a map narrowed to
	 * the territory already chosen cannot get the reader back. This is only
	 * consulted below a minute, where `buckets` has nothing left to say.
	 */
	detail?: LogVolumeDetail;
	/**
	 * Fires when the reader *settles* on a range — pointer up, key press, chip —
	 * and never mid-drag. Panning re-renders this component on every frame; the
	 * stream underneath must not follow it frame by frame, which is why the
	 * committed range is a different value from the one being dragged.
	 * `null` means the whole window.
	 */
	onRangeChange?: (range: LogRange | null) => void;
}

const MINUTE = 60_000;
const HOUR = 60 * MINUTE;
const DAY = 24 * HOUR;

/**
 * Chart body height. Geometry, not palette: the stacked segments are laid out
 * in JS, so the number has to be readable from here — a stylesheet cannot hand
 * it back. The colours all live in tokens.css, where the rule applies.
 */
const CHART_H = 76;

/** Floor for one column plus its gap. The ladder below is chosen so that no
 *  column ever falls under it — a hairline bar is a bar nobody can hit. */
const COL_MIN = 5;
const COL_GAP = 1;

/** Pixels an axis label needs before the next one starts colliding with it. */
const TICK_MIN = 88;

const SECOND = 1_000;

/** Bucket sizes, finest first. The sub-minute rungs are only reachable while a
 *  `detail` answer is in hand — the ring's own strip is per minute, and drawing
 *  a second's column from a minute's number would be inventing five-nines of
 *  precision from one measurement. */
const STEPS = [
	...[1, 2, 5, 10, 15, 30].map((s) => s * SECOND),
	...[1, 2, 5, 10, 15, 30, 60, 120, 180, 360, 720, 1440].map((m) => m * MINUTE),
];

/** The tightest zoom with nothing finer than minutes to draw from. Below five
 *  of them the per-minute source is spreading five columns across a whole panel
 *  — magnification with no new information in it. */
const MIN_SPAN = 5 * MINUTE;

/** The same rule one rung down: with detail in hand the floor is ten of its
 *  buckets, so zooming stops exactly where the measurements stop, not where the
 *  pixels do. */
const MIN_DETAIL_COLUMNS = 10;

/** Bottom to top. Errors sit on the baseline, so a run of them reads as one
 *  block against the axis rather than a band floating on ordinary traffic. */
const LEVELS = ["error", "warn", "info"] as const;
type Level = (typeof LEVELS)[number];

const BAR_CLASS: Record<Level, string> = {
	error: styles.barError,
	warn: styles.barWarn,
	info: styles.barInfo,
};

const DOT_CLASS: Record<Level, string> = {
	error: styles.dotError,
	warn: styles.dotWarn,
	info: styles.dotInfo,
};

/** Window presets, anchored to the newest edge. Only the ones that are actually
 *  narrower than the ring are offered — the rest would all be "All" wearing a
 *  different label, and a control that cannot act is a bug report waiting. */
const PRESETS = [
	{ label: "15m", ms: 15 * MINUTE },
	{ label: "1h", ms: HOUR },
	{ label: "6h", ms: 6 * HOUR },
	{ label: "24h", ms: DAY },
];

interface Column {
	t: number;
	error: number;
	warn: number;
	info: number;
	total: number;
}

/** Folds either histogram's rows into one column per instant. The two answers
 *  name their timestamp differently on the wire — `minute` for the ring's map,
 *  `bucket` for the finer read — so the caller says which field to read rather
 *  than this guessing from the shape. */
function foldRows<T extends { level: string; lines: number }>(
	rows: T[],
	at: (row: T) => string,
): Column[] {
	const byInstant = new Map<number, Column>();
	for (const row of rows) {
		const t = Date.parse(at(row));
		if (Number.isNaN(t)) continue;
		let column = byInstant.get(t);
		if (!column) {
			column = { t, error: 0, warn: 0, info: 0, total: 0 };
			byInstant.set(t, column);
		}
		const level: Level = row.level === "error" || row.level === "warn" ? row.level : "info";
		column[level] += row.lines;
		column.total += row.lines;
	}
	return [...byInstant.values()].sort((a, b) => a.t - b.t);
}

const clockFmt = new Intl.DateTimeFormat("en-US", {
	hour: "2-digit",
	minute: "2-digit",
	hour12: false,
});
const dayFmt = new Intl.DateTimeFormat("en-US", { month: "short", day: "numeric" });
const secondFmt = new Intl.DateTimeFormat("en-US", {
	hour: "2-digit",
	minute: "2-digit",
	second: "2-digit",
	hour12: false,
});

/** How precise a timestamp has to be to tell two of them apart at this zoom.
 *  Under a few minutes that means seconds: hour:minute would print the same
 *  label over every column of a burst and the axis would stop being one. */
function markAt(t: number, span: number): string {
	const d = new Date(t);
	if (span > 4 * DAY) return dayFmt.format(d);
	if (span > 12 * HOUR) return `${dayFmt.format(d)} ${clockFmt.format(d)}`;
	if (span < 5 * MINUTE) return secondFmt.format(d);
	return clockFmt.format(d);
}

/** Keeps a viewport inside the ring, at a span the reader is allowed to hold.
 *  Every navigation goes through here, so no gesture can strand the view past
 *  an edge or collapse it to nothing. */
function clampRange(range: LogRange, domain: LogRange, minSpan: number): LogRange {
	const full = domain.to - domain.from;
	const span = Math.min(Math.max(range.to - range.from, minSpan), full);
	let from = range.from;
	if (from + span > domain.to) from = domain.to - span;
	if (from < domain.from) from = domain.from;
	return { from, to: from + span };
}

/** Scales the viewport around a fixed instant — the one under the cursor for a
 *  wheel, the centre for a key. Zooming around the middle when the pointer is
 *  at the edge slides the thing being pointed at out from under it. */
function zoomedTo(
	view: LogRange,
	domain: LogRange,
	anchor: number,
	factor: number,
	minSpan: number,
): LogRange {
	const span = view.to - view.from;
	const next = Math.min(Math.max(span * factor, minSpan), domain.to - domain.from);
	const ratio = span === 0 ? 0.5 : (anchor - view.from) / span;
	const from = anchor - ratio * next;
	return clampRange({ from, to: from + next }, domain, minSpan);
}

/** Lines per level inside an exact instant range. The bars are drawn on aligned
 *  buckets that can overhang the viewport by up to one bucket, so the readout's
 *  numbers are summed from the source instead: the sentence says "in view", and
 *  it has to mean the view rather than the bars that approximate it. */
function sumBetween(source: Column[], from: number, to: number): Column {
	const out: Column = { t: from, error: 0, warn: 0, info: 0, total: 0 };
	let lo = 0;
	let hi = source.length;
	while (lo < hi) {
		const mid = (lo + hi) >> 1;
		if (source[mid].t < from) lo = mid + 1;
		else hi = mid;
	}
	for (let i = lo; i < source.length && source[i].t < to; i += 1) {
		const row = source[i];
		out.error += row.error;
		out.warn += row.warn;
		out.info += row.info;
		out.total += row.total;
	}
	return out;
}

/**
 * The window's volume as a timeline the reader can drive.
 *
 * The strip used to be a fixed picture of the whole ring: one column per
 * minute, squeezed to whatever width was left. Past a few hours of traffic that
 * is a texture, not a chart — every column is a hairline and nothing can be
 * pointed at. So the ring became a *domain* and the picture became a *viewport*
 * over it: pan with a drag, zoom with ctrl+wheel or the keys, select a stretch
 * with shift+drag, and the bucket size follows the zoom rather than the data.
 *
 * NOTE — this deliberately reverses the older "the window is a line count, not
 * a time range" rule (owner decision). What it does *not* reverse is the reason
 * behind it: the ring is finite, so the domain ends where the ring ends. Every
 * gesture clamps to it and the readout says "start of window" when the reader
 * reaches the edge, rather than panning on into a past that was never stored.
 *
 * Colour never carries anything alone (brief §5): the readout states each level
 * in words and the hovered bucket prints its own numbers.
 */
export function LogTimeline({ buckets, detail, onRangeChange }: LogTimelineProps) {
	// One row per source bucket. Levels fold into the API's own three: `debug`
	// (and anything else a collector labelled a line with) arrives as its own
	// group and belongs with `info`. The previous strip dropped those rows on the
	// floor, so a debug-only minute drew as an empty column.
	const map = useMemo(
		() => foldRows(buckets, (bucket) => bucket.minute),
		[buckets],
	);
	// The fine rows, when the server sent any. They cover the committed range and
	// nothing else, which is why they are a second source rather than the source:
	// everything outside that range still has to come from the minutes.
	const fine = useMemo(
		() => (detail ? foldRows(detail.buckets, (bucket) => bucket.bucket) : []),
		[detail],
	);

	// The domain is always the map's. Reading it from `fine` would shrink the
	// whole strip to the range the reader just picked, leaving them zoomed in
	// with nothing to navigate back out by.
	const domain = useMemo<LogRange | null>(() => {
		if (map.length === 0) return null;
		return { from: map[0].t, to: map[map.length - 1].t + MINUTE };
	}, [map]);
	const hasDomain = domain !== null;

	// Nothing below this was measured, so nothing below it is drawn: without a
	// detail answer the finest real width is a minute, and with one it is
	// whatever width the server says it used — never the width that was asked
	// for, which it is free to coarsen.
	const finest = fine.length > 0 && detail ? detail.bucketSeconds * SECOND : MINUTE;
	const minSpan = fine.length > 0 ? finest * MIN_DETAIL_COLUMNS : MIN_SPAN;
	const clamp = useCallback(
		(range: LogRange, dom: LogRange) => clampRange(range, dom, minSpan),
		[minSpan],
	);
	const zoom = useCallback(
		(view: LogRange, dom: LogRange, anchor: number, factor: number) =>
			zoomedTo(view, dom, anchor, factor, minSpan),
		[minSpan],
	);

	// `null` is the whole window AND a standing promise to keep following it: the
	// ring grows while the panel is open, and a reader who never navigated must
	// not have to press anything to see the newest minute.
	const [view, setView] = useState<LogRange | null>(null);
	const [hover, setHover] = useState<number | null>(null);
	const [brush, setBrush] = useState<{ from: number; to: number } | null>(null);
	const [dragging, setDragging] = useState(false);
	const [width, setWidth] = useState(0);
	// Chrome matches `:focus-visible` on a tabindex'd div even when the focus
	// arrived from a click, because it cannot tell whether the element expects
	// keyboard input. The product's one focus ring therefore fired around the
	// whole strip every time somebody merely dragged it. The ring is not dropped
	// — it is re-drawn from here, and only the keyboard sets it.
	const pointerFocus = useRef(false);
	const [ringed, setRinged] = useState(false);

	const bodyRef = useRef<HTMLDivElement>(null);
	const effective: LogRange = view ?? domain ?? { from: 0, to: MIN_SPAN };
	const span = effective.to - effective.from;

	// New data lands under a reader who may be looking somewhere else.
	useEffect(() => {
		if (!domain) return;
		setView((current) => {
			if (!current) return null;
			const held = current.to - current.from;
			// Parked at the live edge → follow. Anchored in the past → stay put, or
			// every arriving line drags the reader off the thing they were reading.
			const atEdge = current.to >= domain.to - MINUTE;
			const wanted = atEdge ? { from: domain.to - held, to: domain.to } : current;
			const next = clamp(wanted, domain);
			return next.from === current.from && next.to === current.to ? current : next;
		});
	}, [domain]);

	useLayoutEffect(() => {
		const el = bodyRef.current;
		if (!el) return;
		const observer = new ResizeObserver((entries) => {
			const entry = entries[0];
			if (entry) setWidth(entry.contentRect.width);
		});
		observer.observe(el);
		setWidth(el.getBoundingClientRect().width);
		return () => observer.disconnect();
	}, [hasDomain]);

	// Level of detail: the finest bucket whose columns still clear COL_MIN, and
	// never one finer than what was measured. `finest` is the floor, so a rung
	// the source cannot fill is not a rung at all.
	const step = useMemo(() => {
		const room = Math.max(1, Math.floor(width / COL_MIN));
		return (
			STEPS.find((s) => s >= finest && span / s <= room) ?? STEPS[STEPS.length - 1]
		);
	}, [span, width, finest]);

	// Below a minute only the fine rows can answer; at a minute and above the map
	// is both complete and the only thing that covers the whole ring.
	const source = step < MINUTE ? fine : map;

	const columns = useMemo(() => {
		const out: Column[] = [];
		if (!domain || width === 0) return out;
		const first = Math.floor(effective.from / step) * step;
		for (let t = first; t < effective.to; t += step) {
			out.push({ t, error: 0, warn: 0, info: 0, total: 0 });
		}
		if (out.length === 0) return out;
		// Binary search rather than a scan from zero: the ring can hold days of
		// minutes and this re-runs on every frame of a drag.
		let lo = 0;
		let hi = source.length;
		while (lo < hi) {
			const mid = (lo + hi) >> 1;
			if (source[mid].t < first) lo = mid + 1;
			else hi = mid;
		}
		for (let i = lo; i < source.length && source[i].t < effective.to; i += 1) {
			const row = source[i];
			const index = Math.floor((row.t - first) / step);
			if (index < 0 || index >= out.length) continue;
			const column = out[index];
			column.error += row.error;
			column.warn += row.warn;
			column.info += row.info;
			column.total += row.total;
		}
		return out;
	}, [source, domain, effective.from, effective.to, step, width]);

	const colW = columns.length > 0 ? width / columns.length : 0;
	const barW = Math.max(1, colW - COL_GAP);
	const peak = columns.reduce((high, column) => Math.max(high, column.total), 1);

	// Three paths for the whole chart instead of one node per segment: at full
	// width this is several hundred columns re-shaped on every frame of a drag,
	// and a thousand DOM nodes cannot be re-laid out at that rate.
	const shapes = useMemo(() => {
		const out: Record<Level, string> = { error: "", warn: "", info: "" };
		for (let i = 0; i < columns.length; i += 1) {
			const column = columns[i];
			if (column.total === 0) continue;
			const present = LEVELS.filter((level) => column[level] > 0);
			// A column that carries anything is at least one pixel per level in it:
			// "one error in this minute" is the reading this panel exists to make
			// visible, and rounding it away would draw silence over it.
			const height = Math.max(present.length, Math.round((column.total / peak) * CHART_H));
			const x = (i * colW).toFixed(1);
			const w = barW.toFixed(1);
			let left = height;
			let y = CHART_H;
			for (let n = 0; n < present.length; n += 1) {
				const level = present[n];
				const segment =
					n === present.length - 1
						? left
						: Math.max(
								1,
								Math.min(
									left - (present.length - 1 - n),
									Math.round((column[level] / column.total) * height),
								),
							);
				left -= segment;
				y -= segment;
				out[level] += `M${x} ${y}h${w}v${segment}h-${w}z`;
			}
		}
		return out;
	}, [columns, colW, barW, peak]);

	const ticks = useMemo(() => {
		if (width === 0) return [] as number[];
		const room = Math.max(1, Math.floor(width / TICK_MIN));
		const tickStep = STEPS.find((s) => span / s <= room) ?? STEPS[STEPS.length - 1];
		const out: number[] = [];
		for (let t = Math.ceil(effective.from / tickStep) * tickStep; t < effective.to; t += tickStep) {
			out.push(t);
		}
		return out;
	}, [width, span, effective.from, effective.to]);

	const totals = useMemo(
		() => sumBetween(source, effective.from, effective.to),
		[source, effective.from, effective.to],
	);

	// Latest-value mirror for the wheel listener, which is attached once and
	// non-passively (React's own onWheel cannot preventDefault reliably) and so
	// would otherwise close over the first render's viewport forever.
	const stateRef = useRef({ view: effective, domain, width });
	stateRef.current = { view: effective, domain, width };

	const commit = useCallback(
		(next: LogRange | null) => {
			setView(next);
			const dom = stateRef.current.domain;
			const whole = next === null || (dom !== null && next.from <= dom.from && next.to >= dom.to);
			onRangeChange?.(whole ? null : next);
		},
		[onRangeChange],
	);
	const commitRef = useRef(commit);
	commitRef.current = commit;

	useEffect(() => {
		const el = bodyRef.current;
		if (!el) return;
		let settle: number | undefined;
		// The stream follows where the wheel stopped, not every notch on the way.
		const schedule = () => {
			window.clearTimeout(settle);
			settle = window.setTimeout(() => commitRef.current(stateRef.current.view), 180);
		};
		const onWheel = (event: WheelEvent) => {
			const { view: current, domain: dom, width: w } = stateRef.current;
			if (!dom || w === 0) return;
			// Deliberately NO ctrl/meta+wheel zoom (owner decision). It is the usual
			// chart gesture and a trackpad pinch arrives as exactly that, but the
			// browser's own binding on ctrl+wheel is page zoom — so the cost of
			// aiming a few pixels short of the strip is the entire page jumping a
			// zoom level. Zoom is on the buttons, the keys, double-click and
			// shift-drag instead, none of which can misfire into the browser.
			//
			// A horizontal gesture is a pan. A plain vertical one is left alone: the
			// panel sits mid-page, and a strip that swallows the wheel traps the
			// reader on it.
			if (Math.abs(event.deltaX) <= Math.abs(event.deltaY)) return;
			event.preventDefault();
			const shift = (event.deltaX / w) * (current.to - current.from);
			setView(clamp({ from: current.from + shift, to: current.to + shift }, dom));
			schedule();
		};
		el.addEventListener("wheel", onWheel, { passive: false });
		return () => {
			window.clearTimeout(settle);
			el.removeEventListener("wheel", onWheel);
		};
	}, [hasDomain]);

	const dragRef = useRef<{ mode: "pan" | "brush"; x: number; view: LogRange } | null>(null);

	function localX(clientX: number): number {
		const rect = bodyRef.current?.getBoundingClientRect();
		return rect ? clientX - rect.left : 0;
	}

	function onPointerDown(event: ReactPointerEvent<HTMLDivElement>) {
		if (event.button !== 0 || !domain) return;
		pointerFocus.current = true;
		event.currentTarget.setPointerCapture(event.pointerId);
		const x = localX(event.clientX);
		dragRef.current = { mode: event.shiftKey ? "brush" : "pan", x, view: effective };
		setDragging(true);
		setHover(null);
		if (event.shiftKey) setBrush({ from: x, to: x });
	}

	function onPointerMove(event: ReactPointerEvent<HTMLDivElement>) {
		const drag = dragRef.current;
		const x = localX(event.clientX);
		if (!drag) {
			if (colW > 0) {
				setHover(Math.min(columns.length - 1, Math.max(0, Math.floor(x / colW))));
			}
			return;
		}
		if (!domain || width === 0) return;
		if (drag.mode === "brush") {
			setBrush({ from: drag.x, to: x });
			return;
		}
		const perPx = (drag.view.to - drag.view.from) / width;
		const shift = (drag.x - x) * perPx;
		setView(clamp({ from: drag.view.from + shift, to: drag.view.to + shift }, domain));
	}

	function onPointerUp(event: ReactPointerEvent<HTMLDivElement>) {
		const drag = dragRef.current;
		dragRef.current = null;
		setDragging(false);
		setBrush(null);
		if (!drag || !domain) return;
		const x = localX(event.clientX);
		if (drag.mode !== "brush") {
			// A tap rather than a drag. Touch has no hover, so this is the only way
			// to read one bucket's own numbers on a phone; with a mouse the next
			// move overwrites it immediately and nothing changes.
			if (Math.abs(x - drag.x) < 6) {
				if (colW > 0) setHover(Math.min(columns.length - 1, Math.max(0, Math.floor(x / colW))));
				return;
			}
			commit(effective);
			return;
		}
		// Under a few pixels this was a shift-click, not a selection. Zooming to a
		// 200 ms window because a finger slipped is worse than doing nothing.
		if (Math.abs(x - drag.x) < 6 || width === 0) return;
		const perPx = (drag.view.to - drag.view.from) / width;
		const from = drag.view.from + Math.min(drag.x, x) * perPx;
		const to = drag.view.from + Math.max(drag.x, x) * perPx;
		commit(clamp({ from, to }, domain));
	}

	/** Zoom in on the instant under the pointer — the maps gesture, and the one
	 *  that replaces ctrl+wheel. Zooming out stays on the button and the key: a
	 *  modifier here would be the same trap in a different costume. */
	function onDoubleClick(event: ReactPointerEvent<HTMLDivElement>) {
		if (!domain || width === 0) return;
		const anchor = effective.from + (localX(event.clientX) / width) * span;
		commit(zoom(effective, domain, anchor, 0.5));
	}

	function onPointerCancel() {
		dragRef.current = null;
		setDragging(false);
		setBrush(null);
		setHover(null);
	}

	function onKeyDown(event: ReactKeyboardEvent<HTMLDivElement>) {
		if (!domain) return;
		const centre = effective.from + span / 2;
		const nudge = span / 4;
		let next: LogRange | null;
		switch (event.key) {
			case "ArrowLeft":
				next = clamp({ from: effective.from - nudge, to: effective.to - nudge }, domain);
				break;
			case "ArrowRight":
				next = clamp({ from: effective.from + nudge, to: effective.to + nudge }, domain);
				break;
			case "+":
			case "=":
				next = zoom(effective, domain, centre, 0.6);
				break;
			case "-":
			case "_":
				next = zoom(effective, domain, centre, 1.7);
				break;
			case "Home":
				next = null;
				break;
			default:
				return;
		}
		event.preventDefault();
		// Driving it from the keyboard earns the ring even if the focus originally
		// came from a click — otherwise the arrows move a strip with no indication
		// of what they are moving.
		setRinged(true);
		commit(next);
	}

	if (!domain) return null;

	const full = domain.to - domain.from;
	const atStart = effective.from <= domain.from;
	const atEdge = effective.to >= domain.to - MINUTE;
	const atFull = span >= full - 1;
	const centre = effective.from + span / 2;
	const hovered = hover !== null && hover < columns.length ? columns[hover] : null;
	const shown = hovered ?? totals;
	const present = LEVELS.filter((level) => shown[level] > 0);
	const summary = `Log volume from ${markAt(effective.from, span)} to ${markAt(
		effective.to,
		span,
	)}: ${totals.total} lines`;

	return (
		<div className={styles.wrap}>
			<div className={styles.controls}>
				<span className={styles.readout}>
					{hovered
						? `${markAt(hovered.t, span)} – ${markAt(hovered.t + step, span)}`
						: `${markAt(effective.from, span)} – ${markAt(effective.to, span)}`}
				</span>
				<span className={styles.counts}>
					{present.length === 0 ? (
						<span className={styles.quiet}>no lines</span>
					) : (
						present.map((level) => (
							<span key={level} className={styles.count}>
								<span className={`${styles.dot} ${DOT_CLASS[level]}`} />
								{shown[level].toLocaleString("en-US")} {level}
							</span>
						))
					)}
				</span>

				<div className={styles.actions}>
					<button
						type="button"
						className={styles.step}
						onClick={() => commit(zoom(effective, domain, centre, 1.7))}
						disabled={atFull}
						aria-label="Zoom out"
					>
						&minus;
					</button>
					<button
						type="button"
						className={styles.step}
						onClick={() => commit(zoom(effective, domain, centre, 0.6))}
						disabled={span <= MIN_SPAN}
						aria-label="Zoom in"
					>
						+
					</button>
					{PRESETS.filter((preset) => preset.ms < full).map((preset) => (
						<button
							key={preset.label}
							type="button"
							className={styles.chip}
							aria-pressed={atEdge && Math.abs(span - preset.ms) <= MINUTE}
							onClick={() =>
								commit(clamp({ from: domain.to - preset.ms, to: domain.to }, domain))
							}
						>
							{preset.label}
						</button>
					))}
					<button
						type="button"
						className={styles.chip}
						aria-pressed={atFull}
						onClick={() => commit(null)}
					>
						All
					</button>
					{/* Always holds its slot. This button comes and goes with every pan
					    away from the live edge, and dropping it from the flow shunted
					    every control to its left — including the zoom pair, which is the
					    one place the pointer keeps returning to. Hidden rather than
					    disabled: `visibility` keeps the width but takes the button out of
					    the tab order and the a11y tree, so nothing dead is exposed. */}
					<button
						type="button"
						className={`${styles.chip} ${atEdge ? styles.reserved : ""}`}
						onClick={() => commit(clamp({ from: domain.to - span, to: domain.to }, domain))}
					>
						Now
					</button>
				</div>
			</div>

			{/* `pan-y` and not `none`: a horizontal drag belongs to the timeline, but
			    a vertical one is the reader scrolling the page, and swallowing that
			    strands them on a phone. */}
			<div
				ref={bodyRef}
				className={[styles.body, dragging && styles.dragging, ringed && styles.ringed]
					.filter(Boolean)
					.join(" ")}
				role="group"
				aria-label="Log volume timeline"
				tabIndex={0}
				onFocus={() => {
					setRinged(!pointerFocus.current);
					pointerFocus.current = false;
				}}
				onBlur={() => {
					setRinged(false);
					pointerFocus.current = false;
				}}
				onPointerDown={onPointerDown}
				onPointerMove={onPointerMove}
				onPointerUp={onPointerUp}
				onPointerCancel={onPointerCancel}
				onDoubleClick={onDoubleClick}
				onPointerLeave={() => {
					if (!dragRef.current) setHover(null);
				}}
				onKeyDown={onKeyDown}
			>
				<svg
					className={styles.svg}
					width={width}
					height={CHART_H}
					viewBox={`0 0 ${Math.max(width, 1)} ${CHART_H}`}
					role="img"
					aria-label={summary}
				>
					{hovered && (
						<rect
							className={styles.hoverBand}
							x={(hover ?? 0) * colW}
							y={0}
							width={colW}
							height={CHART_H}
						/>
					)}
					<path className={BAR_CLASS.info} d={shapes.info} />
					<path className={BAR_CLASS.warn} d={shapes.warn} />
					<path className={BAR_CLASS.error} d={shapes.error} />
					{hovered && (
						<line
							className={styles.crosshair}
							x1={((hover ?? 0) + 0.5) * colW}
							x2={((hover ?? 0) + 0.5) * colW}
							y1={0}
							y2={CHART_H}
						/>
					)}
					{brush && (
						<rect
							className={styles.brush}
							x={Math.min(brush.from, brush.to)}
							y={0}
							width={Math.abs(brush.to - brush.from)}
							height={CHART_H}
						/>
					)}
					{/* Drawn even where nothing arrived: an empty stretch of a measured
					    window is a fact, and a blank area is the absence of one. */}
					<line
						className={styles.baseline}
						x1={0}
						x2={Math.max(width, 1)}
						y1={CHART_H - 0.5}
						y2={CHART_H - 0.5}
					/>
				</svg>
			</div>

			<div className={styles.axis}>
				{ticks.map((t) => {
					const at = ((t - effective.from) / span) * 100;
					// Centred on its own instant, except at the two ends, where half the
					// label would hang off the strip and be clipped to ":00". Anchoring
					// the outermost label to the edge shifts it by a few pixels; leaving
					// it centred loses the hour it was there to state.
					const anchor = at < 3 ? "none" : at > 97 ? "translateX(-100%)" : "translateX(-50%)";
					return (
						<span key={t} className={styles.tick} style={{ left: `${at}%`, transform: anchor }}>
							{markAt(t, span)}
						</span>
					);
				})}
			</div>

			<div className={styles.foot}>
				<span className={styles.hint}>
					Drag to pan · double-click to zoom in · shift-drag to select
				</span>
				{atStart && <span className={styles.edge}>start of window</span>}
			</div>
		</div>
	);
}
