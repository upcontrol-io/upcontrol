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
	Button,
	EmptyState,
	LinkButton,
	LoadError,
	MultiSelect,
	SkeletonPanel,
} from "@/components/primitives";
import { invalidateApiData, useApiData } from "@/lib/useApiData";
import { logs as logsApi } from "@/lib/client";
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
		invalidateApiData(cacheKey);
	}, [readSig, cacheKey]);
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
								data-loglevel={
									line.level === "error" || line.level === "warn"
										? line.level
										: undefined
								}
								className={[
									styles.row,
									line.level === "error" && styles.rowError,
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

/** HH:MM:SS in the reader's own zone — the API sends RFC 3339 UTC. */
function formatTime(ts: string): string {
	const d = new Date(ts);
	if (Number.isNaN(d.getTime())) return "";
	const pad = (n: number) => String(n).padStart(2, "0");
	return `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
}
