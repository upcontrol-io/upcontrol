import {
	useCallback,
	useEffect,
	useLayoutEffect,
	useMemo,
	useRef,
	useState,
	type ReactNode,
} from "react";
import {
	BrandMark,
	Button,
	EmptyState,
	LinkButton,
	LoadError,
	MultiSelect,
	SkeletonPanel,
} from "@/components/primitives";
import { invalidateApiData, useApiData } from "@/lib/useApiData";
import {
	explainLogs,
	logs as logsApi,
	type ExplainResult,
} from "@/lib/client";
import { useScrollIntoView } from "@/lib/useScrollIntoView";
import { ExplainAnswer } from "./ExplainAnswer";
import { LogMessage } from "./LogMessage";
import { LogTimeline, type LogRange } from "./LogTimeline";
import styles from "./LiveLogsPanel.module.css";

/**
 * The live half of the logs surface: the account's own ring window, straight
 * from `/v1/logs`.
 *
 * Deliberately smaller than `LogsPanel`, which it replaces on a live account.
 * That panel is built on the mock's shape — three named containers, per-container
 * small multiples, nodata ranges, six time windows — and none of those exist in
 * the backend yet: `/v1/logs` returns a flat list of the last lines inside the
 * cutoff, plus per-minute volume. Rendering the mock's tiles over live lines
 * would be inventing figures, and rendering the mock's *lines* on a live account
 * (which is what happened before this component) is worse: a fabricated stream
 * that reads exactly like the customer's own. So this shows what the window
 * actually holds and nothing more; the two converge when the backend serves
 * per-service aggregates.
 *
 * What the *server* holds is still a line count and not a time range
 * (backend-from-new-plan.md §0.3) — one request brings back the whole ring. The
 * range picked on the timeline is therefore a lens over what already arrived,
 * applied here, and never a second question put to the API. That distinction is
 * why narrowing the range cannot fail, cannot spend a read, and cannot empty the
 * strip: the bars keep describing the ring while the stream describes the slice.
 */
// The stream's three buckets. `info` is the server's name for "neither an
// error nor a warning" — debug rides in it — so the three always partition
// the window and any combination is a real question.
const LEVEL_OPTIONS = [
	{ value: "error", label: "Errors" },
	{ value: "warn", label: "Warnings" },
	{ value: "info", label: "Info & debug" },
];

/**
 * The server's three input caps, mirrored from `back/internal/ai/scenario.go`
 * (`ExplainLogs`). Three, not one: a JSON log line runs to hundreds of bytes,
 * so an ordinary selection reaches the byte caps long before it reaches a
 * hundred lines. Mirroring only the line count offered a read the server was
 * certain to refuse, and the refusal read as a failure rather than as a limit.
 */
const EXPLAIN_MAX_LINES = 100;
const EXPLAIN_MAX_LINE_BYTES = 2000;
const EXPLAIN_MAX_TOTAL_BYTES = 32768;

/** The bytes the server counts: UTF-8, not UTF-16 code units. */
const wireBytes = (text: string) => new TextEncoder().encode(text).length;

/**
 * The exact string sent for one line, and the string the caps are counted on.
 * The raw server ts (ISO, identical for every reader — NOT a locale-rendered
 * clock) and the service ride along: without them the model cannot correlate
 * a burst by time or tell services apart, which is the whole point of
 * Explain. Cache identity stays intact — same selection, same bytes.
 */
const wireLine = (line: { ts: string; service?: string; level: string; message: string }) =>
	`${line.ts} ${line.service ?? "app"} ${line.level} ${line.message}`;

/**
 * The cap this selection breaks, phrased for the chip beside the count, or
 * null when the read is inside all three. Pure: the same arithmetic the
 * handler runs, on the same bytes, before any money is spent.
 */
function selectionOverCap(lines: readonly string[]): string | null {
	if (lines.length > EXPLAIN_MAX_LINES) return `max ${EXPLAIN_MAX_LINES} lines`;
	let total = 0;
	for (const line of lines) {
		const bytes = wireBytes(line);
		if (bytes > EXPLAIN_MAX_LINE_BYTES) return "one line is too long to read";
		total += bytes;
	}
	if (total > EXPLAIN_MAX_TOTAL_BYTES) return "too much text to read at once";
	return null;
}

/**
 * The visible errors narrowed to what the server will actually read, for the
 * no-selection button. A selection past the caps is refused (the reader chose
 * those lines); the timeline's errors cannot be refused, only trimmed —
 * keeping the newest lines, which is what "the latest errors" means to the
 * reader holding the button. A single line past the per-line cap is skipped,
 * not fatal: one oversized line must not make the rest unreadable. The input
 * is the server's newest-first order; the result is the oldest-first order
 * the wire sends.
 */
function errorsUnderCaps(newestFirst: readonly string[]): string[] {
	const kept: string[] = [];
	let total = 0;
	for (const line of newestFirst) {
		if (kept.length === EXPLAIN_MAX_LINES) break;
		const bytes = wireBytes(line);
		if (bytes > EXPLAIN_MAX_LINE_BYTES) continue;
		if (total + bytes > EXPLAIN_MAX_TOTAL_BYTES) break;
		kept.push(line);
		total += bytes;
	}
	return kept.reverse();
}

export function LiveLogsPanel() {
	// Both filters are the server's (`?service=`, `?level=`), so they apply
	// before the stream limit: filtering the capped lines already on the client
	// would show a fraction of what the window holds and call it the window.
	// An empty set is "everything" — the pickers have no way to select nothing.
	const [pickedServices, setPickedServices] = useState<ReadonlySet<string>>(
		new Set(),
	);
	const [pickedLevels, setPickedLevels] = useState<ReadonlySet<string>>(
		new Set(),
	);
	const filtered = pickedServices.size > 0 || pickedLevels.size > 0;
	// A different question is a different cache entry — sharing one would serve
	// the narrowed answer to the next reader of the whole window.
	const cacheKey = filtered
		? `logs:${[...pickedServices].sort().join(",")}|${[...pickedLevels].sort().join(",")}`
		: "logs";
	// The no-selection Explain button's own read (see its useApiData below);
	// defined beside cacheKey because the range-change effect invalidates both.
	const errorsKey = `errors:${[...pickedServices].sort().join(",")}`;
	// The timeline's committed range, `null` for the whole window. It arrives on
	// pointer-up rather than per frame, so a drag across the strip re-renders the
	// chart and not this panel's several hundred rows.
	const [range, setRange] = useState<LogRange | null>(null);
	const { data, loading, failed } = useApiData(cacheKey, () =>
		logsApi([...pickedServices], [...pickedLevels], range),
	);
	// The last settled answer, held across cache-key changes. Picking a service
	// asks a different question (a different key), and until it answers `data`
	// is undefined — rendering the skeleton there collapsed the whole panel for
	// every filter click and took the open picker menu down with it. The
	// narrowing swaps in when it arrives; the previous answer holds the layout
	// until then, exactly what the hook itself does for a same-key refetch.
	const lastAnswerRef = useRef<typeof data>(undefined);
	if (data !== undefined) lastAnswerRef.current = data;
	const answer = data ?? lastAnswerRef.current;
	const lines = answer?.lines ?? [];

	// A range picked over one set of bars means nothing over another: narrowing to
	// a service re-reads a different window, and carrying the old bounds across
	// would silently hide lines the reader just asked to see.
	useEffect(() => setRange(null), [cacheKey]);

	/**
	 * A ranged read is a real request, and the range is deliberately NOT part of
	 * the cache key.
	 *
	 * Keying by it would cost twice. A key that has not settled reads as
	 * `loading`, so every pan would blank the whole panel — including the strip
	 * the pan was performed on. And the module cache in `useApiData` has a TTL but
	 * no eviction, so one entry per range the reader ever visited, a thousand
	 * lines in each, would pile up for the life of the tab.
	 *
	 * Re-reading the SAME key keeps the current lines on screen until the answer
	 * lands, which is exactly what the hook already does after a write.
	 */
	const [awaiting, setAwaiting] = useState(false);
	const lastRead = useRef<string | null>(null);
	const readSig = `${cacheKey}|${range ? `${range.from}:${range.to}` : "all"}`;
	useEffect(() => {
		const previous = lastRead.current;
		lastRead.current = readSig;
		// First mount, or a changed key: `useApiData` is already fetching either way.
		if (previous === null || previous.split("|")[0] !== cacheKey) return;
		if (previous === readSig) return;
		setAwaiting(true);
		// Both reads answer for the committed range: the stream, and the
		// visible errors the no-selection button counts.
		invalidateApiData(cacheKey, errorsKey);
	}, [readSig, cacheKey, errorsKey]);
	// `loading` is false during a refetch by design ("nothing has answered yet",
	// not "a request is in flight"), so the answer's arrival is the only signal.
	useEffect(() => setAwaiting(false), [data]);

	// The server bounds the read now, so this filter is a no-op on a settled
	// answer. It earns its place in the gap: between the pan and its answer the
	// previous, wider response is still on screen, and showing lines from outside
	// the range the reader just drew is the panel contradicting its own header.
	//
	// Reversed at the end: the wire is newest-first (the cap means "the latest
	// N"), but the pane reads like a terminal — time runs downward and the
	// newest line is the last one, where tailing a log has always put it. The
	// DOM order matches the visual order on purpose, so a screen reader walks
	// the same chronology the eye does.
	const visible = useMemo(() => {
		const inRange = range
			? lines.filter((line) => {
					const t = Date.parse(line.ts);
					return !Number.isNaN(t) && t >= range.from && t < range.to;
				})
			: lines;
		return [...inRange].reverse();
	}, [lines, range]);
	const showWholeWindow = useCallback(() => setRange(null), []);

	// The stream opens at its tail — the newest line — and stays pinned there
	// across re-reads, the way a terminal follows its own output. Pinned is a
	// fact about where the reader is, not a mode: scrolling up to read history
	// releases the pin (a refresh must not yank the page out from under a
	// reader mid-line), and returning to the bottom re-arms it. A ref, not
	// state: scroll position changes on every frame of a swipe and none of
	// those frames is a reason to re-render the panel.
	const streamRef = useRef<HTMLPreElement>(null);
	const pinnedRef = useRef(true);

	function onStreamScroll() {
		const el = streamRef.current;
		if (!el) return;
		pinnedRef.current = el.scrollHeight - el.scrollTop - el.clientHeight < 48;
	}

	useLayoutEffect(() => {
		const el = streamRef.current;
		if (!el || !pinnedRef.current) return;
		// Before paint, so the first frame the reader sees is already the tail —
		// an effect after paint shows the top for one frame and then jumps.
		el.scrollTop = el.scrollHeight;
	}, [visible]);

	// The overview ruler: where the window's warnings and errors sit, as marks
	// beside the scrollbar (owner decision, Aug 18, 2026). Positions are
	// MEASURED from the laid-out rows, not derived from indices — messages
	// wrap, so row heights are unequal and an index-proportional mark drifts
	// exactly on the long lines that matter. Info lines get no mark, and a
	// window that fits without scrolling gets no ruler: it would restate what
	// is already fully on screen.
	const [rulerMarks, setRulerMarks] = useState<
		{ top: number; height: number; level: string }[]
	>([]);
	// The native scrollbar's gutter width. A DOM overlay paints OVER the native
	// scrollbar and no z-index reaches it, so the ruler gets its own lane just
	// inside the gutter instead of covering the thumb. Zero on overlay
	// scrollbars (macOS), where hugging the edge is the right place anyway.
	const [rulerGutter, setRulerGutter] = useState(0);
	const measureRuler = useCallback(() => {
		const el = streamRef.current;
		if (!el || el.scrollHeight <= el.clientHeight) {
			setRulerMarks((current) => (current.length === 0 ? current : []));
			return;
		}
		setRulerGutter(el.offsetWidth - el.clientWidth);
		const rows = el.querySelectorAll<HTMLElement>("[data-loglevel]");
		setRulerMarks(
			Array.from(rows, (row) => ({
				top: (row.offsetTop / el.scrollHeight) * 100,
				// A one-line row on a tall window rounds to nothing; 0.75% keeps
				// every mark a visible sliver.
				height: Math.max((row.offsetHeight / el.scrollHeight) * 100, 0.75),
				level: row.dataset.loglevel ?? "",
			})),
		);
	}, []);
	// After layout so offsetTop is real, re-run when the window changes; the
	// observer catches re-wrapping when the panel itself resizes.
	useLayoutEffect(measureRuler, [visible, measureRuler]);
	const streamMounted = visible.length > 0;
	useEffect(() => {
		const el = streamRef.current;
		if (!el) return;
		const observer = new ResizeObserver(measureRuler);
		observer.observe(el);
		return () => observer.disconnect();
	}, [measureRuler, streamMounted]);
	// Lines the read's own bounds hold before the stream limit. The contract has
	// carried this field for exactly the sentence below and the panel never
	// printed it.
	const total = answer?.total ?? 0;

	/**
	 * The head's count, and it always leads with the number of rows actually on
	 * screen. "last 200 lines in your window" was a fixed string dressed as a
	 * measurement: 200 was the server's stream cap, so it said the same thing for
	 * a window holding two hundred lines and one holding twenty thousand.
	 *
	 * `total` is counted over the same bounds and filters the lines were read
	 * with, so the two numbers always describe one question.
	 */
	function headCount(): string {
		if (range) {
			if (visible.length === 0) return "nothing in this range";
			return total > visible.length
				? `showing ${countOfLines(visible.length)} of ${count(total)} in this range`
				: `showing ${countOfLines(visible.length)} in this range`;
		}
		if (lines.length === 0) {
			return filtered ? "nothing matches your filters" : "nothing received yet";
		}
		const scope = filtered ? "matching your filters" : "in your window";
		return total > lines.length
			? `showing ${countOfLines(lines.length)} of ${count(total)} ${scope}`
			: `showing ${countOfLines(lines.length)} ${scope}`;
	}

	// The picker outlives the read it came from. Picking a service starts a new
	// request, and until it answers `data` is undefined — so a picker built
	// straight off the response would vanish at the exact moment it was used, and
	// take the way back to the rest of the window with it.
	const [services, setServices] = useState<
		{ name: string; lines: number }[]
	>([]);
	useEffect(() => {
		if (data?.services?.length) setServices(data.services);
	}, [data]);

	// Explain reads the lines the reader picked, not the whole window: the answer
	// is only as good as the question, and the quota is per read.
	const [selected, setSelected] = useState<ReadonlySet<string>>(new Set());
	const [explaining, setExplaining] = useState(false);
	const [explanation, setExplanation] = useState<ExplainResult | null>(null);
	// Kept, not just counted (the old LogsPanel's invariant): the context copy
	// pastes the answer beside the lines it was READ from, so the wire bytes
	// are snapshotted at explain time. `wire` below is a live memo — unticking
	// a row after the read would shrink it, and the paste would compose an
	// answer with evidence it never saw.
	const [explainedLines, setExplainedLines] = useState<readonly string[]>([]);
	const [explainError, setExplainError] = useState<string | null>(null);
	// The answer renders under the button that asked for it, which on a phone is
	// off-screen — bring it to the reader (same rule as the incident card).
	const readRef = useScrollIntoView<HTMLDivElement>(explaining || explanation);

	function toggle(seq: string) {
		setSelected((current) => {
			const next = new Set(current);
			if (!next.delete(seq)) next.add(seq);
			return next;
		});
	}

	// Only what is both picked and still on screen: narrowing the range after
	// selecting must not send the reader's money on lines they can no longer see.
	const picked = useMemo(
		() => visible.filter((line) => selected.has(line.seq)),
		[visible, selected],
	);
	// The wire line carries the server's own ts and service (canonical bytes,
	// the same for every reader) in the oldest-first order
	// `picked` already holds: these bytes are the cache identity server-side,
	// so a locale-rendered timestamp must not ride along (it would bust the
	// cache for every reader whose formatTime differs). Built once, because
	// the caps below have to be counted on exactly what is sent.
	const wire = useMemo(() => picked.map(wireLine), [picked]);
	// Past any of the server's caps the read is a guaranteed 400 — the button
	// says so instead of sending money on a refusal. The count stays honest
	// beside it.
	const overCap = selectionOverCap(wire);

	// The errors behind the no-selection button: its own question, so its own
	// cache entry. The rule is "what the timeline shows" (owner decision,
	// Aug 18, 2026 — this replaced a last-hour rule, whose intersection with a
	// committed range on a PAST period was empty, so the button vanished
	// exactly where a reader zoomed into trouble): the committed range when
	// one is drawn, the server's own window when not, level=error either way.
	// The service filter is part of the key (one service's errors are a
	// different question); like the main read, the range is NOT part of the
	// key (one cache entry per drawn range would pile up for the life of the
	// tab) — the readSig effect below invalidates this key instead, and
	// useApiData refetches through the latest fetcher closure, which carries
	// the current range. The server narrows `lines` to the range — the panel
	// does not re-check timestamps on the answer.
	const {
		data: errorsData,
		loading: errorsLoading,
		failed: errorsFailed,
	} = useApiData(errorsKey, () => logsApi([...pickedServices], ["error"], range));
	// The count in the button is what will be sent (the caps trim from the
	// newest end), never the server's total. While the read is pending or has
	// failed the button is simply not there: a secondary affordance earns no
	// skeleton and no error state of its own.
	const errorsWire = useMemo(() => {
		if (errorsLoading || errorsFailed || !errorsData) return [];
		return errorsUnderCaps(errorsData.lines.map(wireLine));
	}, [errorsData, errorsLoading, errorsFailed]);

	function explain(linesToSend: string[]) {
		if (linesToSend.length === 0) return;
		setExplaining(true);
		setExplanation(null);
		setExplainedLines(linesToSend);
		setExplainError(null);
		void explainLogs(linesToSend)
			.then((res) => {
				setExplanation(res);
				// The read is metered (audit §11): the sidebar's quota is the plan's
				// own count, so it only moves when the plan is re-read. Without this
				// the first Explain left "2/5" standing until a reload — the server
				// had already counted the use.
				invalidateApiData("plan");
			})
			.catch((err: unknown) => {
				// F12-first: the panel surfaces the server's message, but the
				// console must carry the full failure too — err.message alone
				// hides the HTTP status and any attached response body.
				console.error("explain failed", err);
				// No upgrade wall in this app (Decision 8): a refusal, were one
				// ever to fire, reads like any other — in the server's words.
				// The server writes its refusals for this panel (the
				// throttle's "Too many Explain requests", a 400's caps) and
				// fetchJSON carries them in err.message; show them. Its two
				// machine-generated shapes — the "HTTP n" no-body fallback and
				// the browser's transport text — keep the designed line.
				const message = err instanceof Error ? err.message : "";
				const fromServer = message !== "" && !message.startsWith("HTTP ");
				setExplainError(
					fromServer ? message : "The read did not come back. Nothing was spent.",
				);
			})
			.finally(() => setExplaining(false));
	}

	// One head for all three states, because the pickers belong to the panel and
	// not to any one answer: a reader who narrows to a combination that is
	// loading, or whose read then fails, must still be able to get back.
	const head = (extra?: ReactNode) => (
		<div className={styles.head}>
			{/* No heading of its own. This panel used to restate "Logs" as an <h2>
			    because the page around it had no <h1> at all; now that the page
			    carries one, a second heading with the same word is a duplicate for
			    the eye and for a screen reader alike. The <section aria-label="Logs">
			    below still names the region. */}
			{extra}
			<div className={styles.filter}>
				{/* Nothing to pick between is not a picker (a control that cannot
				    act is a bug report waiting to be filed). */}
				{services.length > 1 && (
					<MultiSelect
						label="Service"
						allLabel="All services"
						className={styles.filterControl}
						options={services.map((entry) => ({
							// A line that carried no label is a real row in the stream,
							// where it prints as "—"; it has to be pickable too. Its
							// value is the empty string, a real name on the wire.
							value: entry.name,
							label: entry.name || "unlabelled",
							note: String(entry.lines),
						}))}
						selected={pickedServices}
						onChange={setPickedServices}
					/>
				)}
				{/* The level picker can always act once anything has arrived: even
				    a window with no errors answers "errors only" honestly, with the
				    filtered-empty state and its way back. */}
				{services.length > 0 && (
					<MultiSelect
						label="Levels"
						allLabel="All levels"
						className={styles.filterControl}
						options={LEVEL_OPTIONS}
						selected={pickedLevels}
						onChange={setPickedLevels}
					/>
				)}
			</div>
		</div>
	);

	// Three states, and they look different: a failed read is a load error (never
	// "No log lines yet" — that says the app sent nothing, when really nobody
	// asked), a pending read is a skeleton, and live-and-empty is the real state.
	if (failed) {
		return (
			<section className={styles.panel} aria-label="Logs">
				{head()}
				<LoadError
					what="your logs"
					onRetry={() => invalidateApiData(cacheKey)}
					framed={false}
				/>
			</section>
		);
	}
	// The skeleton is for the panel's FIRST answer only. A narrowed question is
	// also "loading" under its own key, but the previous answer is still in
	// hand — swapping it for a skeleton collapses the panel's height on every
	// filter click and unmounts the picker menu the reader is holding open.
	if (loading && !answer) {
		return (
			<section className={styles.panel} aria-label="Logs">
				{head()}
				<SkeletonPanel rows={3} label="Loading your logs" />
			</section>
		);
	}

	return (
		<section className={styles.panel} aria-label="Logs">
			{head(
				<span className={styles.count}>{headCount()}</span>,
			)}

			{/* The volume strip /v1/logs has always returned and this panel dropped.
			    Drawn only when there is volume to draw — an empty window shows its
			    empty state below, not an empty chart. It keeps describing the whole
			    ring while a range is picked: the strip is the map, and a map that
			    shrinks to the territory you already chose cannot get you back. */}
			<LogTimeline buckets={answer?.volume ?? []} onRangeChange={setRange} />

			{range && visible.length === 0 && awaiting ? (
				// The pan has been sent and not yet answered. Anything else here is a
				// claim about the range that is not known to be true yet — and "no
				// lines" is the one claim a monitoring panel must never guess at.
				<SkeletonPanel rows={3} label="Loading the picked range" />
			) : range && visible.length === 0 ? (
				// Distinct from both the filter-empty and the never-installed states:
				// the collector is working and the window is carrying lines, just none
				// between the two points the reader dragged. Saying "no log lines yet"
				// here would send somebody with a working install off to wire it again.
				<EmptyState
					framed={false}
					title="No lines in this range"
					body="Nothing arrived between those two points. Pick a wider stretch on the strip above, or go back to the whole window."
					action={
						<Button variant="secondary" size="sm" onClick={showWholeWindow}>
							Show the whole window
						</Button>
					}
				/>
			) : lines.length === 0 && filtered ? (
				// The reader's own filters emptied this, not their install. Same rule
				// as failed-vs-empty: sending someone with a working collector off to
				// wire it up again is the panel answering a question nobody asked.
				<EmptyState
					framed={false}
					title="No lines match your filters"
					body="Nothing inside the window matches that combination of services and levels right now."
					action={
						<Button
							variant="secondary"
							size="sm"
							onClick={() => {
								setPickedServices(new Set());
								setPickedLevels(new Set());
							}}
						>
							Reset filters
						</Button>
					}
				/>
			) : lines.length === 0 ? (
				<EmptyState
					// The panel already draws the frame — see EmptyState's `framed`.
					framed={false}
					title="No log lines yet"
					body="Your app has not sent anything to this project. The agent plugin wires it up in one command."
					action={
						// Straight to the screen that carries the install command —
						// Settings owns it in the OSS app.
						<LinkButton variant="primary" size="sm" to="/settings">
							Add it to your code
						</LinkButton>
					}
				/>
			) : (
				<div className={styles.streamWrap}>
					<pre ref={streamRef} onScroll={onStreamScroll} className={styles.stream}>
						{visible.map((line) => (
							<div
								key={line.seq}
								role="checkbox"
								tabIndex={0}
								data-loglevel={
									line.level === "error" || line.level === "warn"
										? line.level
										: undefined
								}
								aria-checked={selected.has(line.seq)}
								onClick={() => toggle(line.seq)}
								onKeyDown={(event) => {
									if (event.key === "Enter" || event.key === " ") {
										event.preventDefault();
										toggle(line.seq);
									}
								}}
								className={[
									styles.row,
									line.level === "error" && styles.rowError,
									selected.has(line.seq) && styles.rowSelected,
								]
									.filter(Boolean)
									.join(" ")}
							>
								<span className={styles.time}>{formatTime(line.ts)}</span>
								<span className={styles.service}>{line.service || "—"}</span>
								<span className={styles.level}>{line.level.toUpperCase()}</span>
								{/* Same renderer as the incident card's slice, so a JSON line
								    is coloured identically in both places. */}
								<span className={styles.message}>
									<LogMessage text={line.message} />
								</span>
							</div>
						))}
					</pre>
					{/* The marks are presentation: each level already rides its row as
					    text, so the ruler carries no aria of its own. */}
					{rulerMarks.length > 0 && (
						<div
							className={styles.ruler}
							style={{ right: rulerGutter }}
							data-log-ruler
							aria-hidden="true"
						>
							{rulerMarks.map((mark, i) => (
								<span
									key={i}
									className={
										mark.level === "error" ? styles.rulerError : styles.rulerWarn
									}
									style={{ top: `${mark.top}%`, height: `${mark.height}%` }}
								/>
							))}
						</div>
					)}
				</div>
			)}

			{/* The trigger sits under the stream, beside the lines it will read —
			    the reader picks rows at the bottom of the pane, and a button in
			    the head is off-screen at exactly that moment. */}
			{picked.length > 0 ? (
				<div className={styles.selectBar}>
					<span className={styles.selectCount}>
						{countOfLines(picked.length)} selected
						{overCap && (
							<span className={styles.selectCap}>{overCap}</span>
						)}
					</span>
					<div className={styles.selectActions}>
						<Button
							variant="ghost"
							size="sm"
							onClick={() => setSelected(new Set())}
						>
							Clear
						</Button>
						<button
							type="button"
							className={styles.explainButton}
							onClick={() => explain(wire)}
							disabled={explaining || overCap !== null}
						>
							{explaining ? "Reading…" : `Explain ${countOfLines(picked.length)}`}
						</button>
					</div>
				</div>
			) : (
				// Nothing picked: the same bar, one button. The timeline's visible
				// errors answer the question a reader without a selection still has
				// — "what broke?" — through the same read. No errors in view, no
				// button: a control that cannot act gets no render.
				errorsWire.length > 0 && (
					<div className={styles.selectBar}>
						<div className={styles.selectActions}>
							<button
								type="button"
								className={styles.explainButton}
								onClick={() => explain(errorsWire)}
								disabled={explaining}
							>
								{/* "last N" only when the timeline shows its live tail; a
								    committed range is a slice of the past, where "last"
								    would claim a recency the lines do not have. */}
								{explaining
									? "Reading…"
									: range
										? `Explain ${countOfErrors(errorsWire.length)}`
										: `Explain last ${countOfErrors(errorsWire.length)}`}
							</button>
						</div>
					</div>
				)
			)}

			{(explaining || explanation || explainError) && (
				<div ref={readRef} className={styles.read}>
					{explaining ? (
						// The reading mark — the kit's 2c corner-chase, centred where the answer
						// will land. The one named exception to "never a spinner" (owner decision,
						// Aug 18, 2026): this is the brand mark reading, not a generic spinner,
						// and it is scoped to this single state.
						<div
							className={styles.readChase}
							role="status"
							aria-label="Reading the lines"
						>
							<BrandMark variant="chase" size={48} />
						</div>
					) : explanation ? (
						<ExplainAnswer result={explanation} lines={explainedLines} />
					) : (
						<p className={styles.readText}>{explainError}</p>
					)}
				</div>
			)}
		</section>
	);
}

/** Thousands separated: the window routinely holds five figures, and "22691"
 *  in the middle of a sentence has to be re-read to be counted. */
function count(n: number): string {
	return n.toLocaleString("en-US");
}

/** "1 line" / "23 lines" — narrowing to a service routinely lands on one. */
function countOfLines(n: number): string {
	return `${count(n)} ${n === 1 ? "line" : "lines"}`;
}

/** "1 error" / "23 errors" — the visible-errors count in the no-selection button. */
function countOfErrors(n: number): string {
	return `${count(n)} ${n === 1 ? "error" : "errors"}`;
}

/** HH:MM:SS in the reader's own zone — the API sends RFC 3339 UTC. */
function formatTime(ts: string): string {
	const d = new Date(ts);
	if (Number.isNaN(d.getTime())) return "";
	const pad = (n: number) => String(n).padStart(2, "0");
	return `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
}
