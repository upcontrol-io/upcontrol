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

/** The live half of the logs surface: the ring window straight from
 *  /v1/logs, and nothing the backend does not serve. */

// The timeline range is a lens over what already arrived, never a second API
// question.
const LEVEL_OPTIONS = [
	{ value: "error", label: "Errors" },
	{ value: "warn", label: "Warnings" },
	{ value: "info", label: "Info & debug" },
];

/** The server's three input caps, mirrored from scenario.go (ExplainLogs):
 *  a JSON line runs to hundreds of bytes, so bytes bind before line count. */
const EXPLAIN_MAX_LINES = 100;
const EXPLAIN_MAX_LINE_BYTES = 2000;
const EXPLAIN_MAX_TOTAL_BYTES = 32768;

/** The bytes the server counts: UTF-8, not UTF-16 code units. */

/** Roughly how many columns a strip has room for; the server snaps the ask
 *  to a width it can answer, so this is an opening bid. */
const DETAIL_COLUMNS = 120;

/** How fine a histogram to ask for over the picked range, or 0 for none;
 *  below a minute is the only place the per-minute strip has no answer. */
function detailWidthFor(range: { from: number; to: number } | null): number {
	if (!range) return 0;
	const seconds = (range.to - range.from) / 1000;
	if (seconds <= 0 || seconds > 3600) return 0;
	return Math.max(1, Math.floor(seconds / DETAIL_COLUMNS));
}

const wireBytes = (text: string) => new TextEncoder().encode(text).length;

/** The exact string sent per line, and what the caps count: raw server ts
 *  (ISO, not locale-rendered) and service, so the cache identity holds. */
const wireLine = (line: { ts: string; service?: string; level: string; message: string }) =>
	`${line.ts} ${line.service ?? "app"} ${line.level} ${line.message}`;

/** The cap this selection breaks, or null when inside all three; the same
 *  arithmetic the handler runs, before any money is spent. */
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

/** The visible errors trimmed to what the server will read: newest lines
 *  kept, an oversized line skipped rather than fatal, output oldest-first. */
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
	// Both filters are the server's, so they apply before the stream limit;
	// an empty set means everything (the pickers cannot select nothing).
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
	// The committed range arrives on pointer-up, not per frame, so a drag
	// re-renders the chart and not this panel's several hundred rows.
	const [range, setRange] = useState<LogRange | null>(null);
	const { data, loading, failed } = useApiData(cacheKey, () =>
		logsApi([...pickedServices], [...pickedLevels], range, detailWidthFor(range)),
	);
	// The last settled answer, held across cache-key changes: until a new key
	// answers, data is undefined and the previous answer holds the layout.
	const lastAnswerRef = useRef<typeof data>(undefined);
	if (data !== undefined) lastAnswerRef.current = data;
	const answer = data ?? lastAnswerRef.current;
	const lines = answer?.lines ?? [];

	// A range picked over one set of bars means nothing over another: carrying
	// old bounds across a narrowing would hide lines the reader asked to see..
	useEffect(() => setRange(null), [cacheKey]);

	// A ranged read is real, but the range is NOT part of the cache key: keying
	// by it would blank the panel per pan and pile up unevicted entries.
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

	// In the gap between a pan and its answer this hides lines outside the
	// drawn range; the reversal keeps the pane terminal-ordered (newest last).
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

	// The stream opens at its tail and stays pinned across re-reads; scrolling
	// up releases the pin, returning to the bottom re-arms it. A ref, not state.
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

	// The overview ruler: positions are MEASURED from laid-out rows (messages
	// wrap, heights differ); a window that fits gets no ruler.
	const [rulerMarks, setRulerMarks] = useState<
		{ top: number; height: number; level: string }[]
	>([]);
	// The native gutter width: no z-index reaches the native thumb, so the
	// ruler takes its own lane just inside it (zero on overlay scrollbars).
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
	// Lines the read's own bounds hold before the stream limit..
	const total = answer?.total ?? 0;

	// The head's count leads with the rows actually on screen; `total` counts
	// the same bounds and filters, so the two numbers describe one question.
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

	// The picker outlives the read it came from: until a new key answers, data
	// is undefined and a response-built picker would vanish while in use..
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
	// Kept, not just counted: the context copy pastes the answer beside the
	// lines it was READ from, so the wire bytes are snapshotted at read time..
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
	// the cache identity server-side), oldest-first; built once, counted once..
	const wire = useMemo(() => picked.map(wireLine), [picked]);
	// Past any cap the read is a guaranteed 400: the button says so instead of
	// sending money on a refusal..
	const overCap = selectionOverCap(wire);

	// The errors behind the no-selection button, keyed by service filter only:
	// the committed range when drawn, else the server's window.
	const {
		data: errorsData,
		loading: errorsLoading,
		failed: errorsFailed,
	} = useApiData(errorsKey, () => logsApi([...pickedServices], ["error"], range));
	// The count is what will be sent (caps trim from the newest end); pending
	// or failed, the button is simply not there..
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
				// The read is metered: without re-reading the plan the sidebar's quota
				// count stays stale until a reload, though the server counted the use..
				invalidateApiData("plan");
			})
			.catch((err: unknown) => {
				// F12-first: err.message alone hides the HTTP status and any
				// attached response body, so the console carries the full failure..
				console.error("explain failed", err);
				// No upgrade wall here: show the server's own refusal words from
				// err.message; the machine-generated shapes keep the designed line..
				const message = err instanceof Error ? err.message : "";
				const fromServer = message !== "" && !message.startsWith("HTTP ");
				setExplainError(
					fromServer ? message : "The read did not come back. Nothing was spent.",
				);
			})
			.finally(() => setExplaining(false));
	}

	// One head for all three states: the pickers belong to the panel, so a
	// loading or failed narrow still leaves the reader a way back..
	const head = (extra?: ReactNode) => (
		<div className={styles.head}>
			{/* No heading of its own: the page carries the <h1>, and a second
			    heading with the same word is a duplicate for eye and reader. */}
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
							// An unlabelled line is a real row (prints as "—"); its value
							// is the empty string, a real name on the wire.
							value: entry.name,
							label: entry.name || "unlabelled",
							note: String(entry.lines),
						}))}
						selected={pickedServices}
						onChange={setPickedServices}
					/>
				)}
				{/* The level picker can always act once anything has arrived: even a
				    window with no errors answers "errors only" honestly. */}
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

	// Three states, all different: failed reads a load error (never "No log
	// lines yet", which says the app sent nothing), pending a skeleton..
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
	// The skeleton is for the FIRST answer only: a narrowed question still has
	// the previous answer in hand, and a skeleton would collapse the panel..
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

			{/* The volume strip, drawn only when there is volume: an empty window
			    shows its empty state, not an empty chart. */}
			<LogTimeline
				buckets={answer?.volume ?? []}
				detail={answer?.detail}
				onRangeChange={setRange}
			/>

			{range && visible.length === 0 && awaiting ? (
				// The pan is sent and unanswered: "no lines" is the one claim a
				// monitoring panel must never guess at..
				<SkeletonPanel rows={3} label="Loading the picked range" />
			) : range && visible.length === 0 ? (
				// Distinct from filter-empty and never-installed: the window carries
				// lines, just none between the two dragged points..
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
				// The reader's own filters emptied this, not their install; same
				// rule as failed-vs-empty..
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

			{/* The trigger sits under the stream, beside the lines it will read:
			    the reader picks rows at the bottom, where a head button is off-screen. */}
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
				// Nothing picked: the timeline's visible errors answer "what broke?"
				// through the same read; no errors in view, no button..
				errorsWire.length > 0 && (
					<div className={styles.selectBar}>
						<div className={styles.selectActions}>
							<button
								type="button"
								className={styles.explainButton}
								onClick={() => explain(errorsWire)}
								disabled={explaining}
							>
								{/* "last N" only when the timeline shows its live tail; a committed
								    range is a slice of the past, where "last" claims a recency it lacks. */}
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
						// The reading mark, centred where the answer will land: the one
						// exception to "never a spinner", scoped to this state..
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
