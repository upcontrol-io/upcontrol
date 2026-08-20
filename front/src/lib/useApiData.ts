/**
 * useApiData — generic fetch hook for entity-level data (monitors, sources,
 * channels, etc.). Each call fetches from the API on mount and caches the
 * result so multiple components sharing the same entity don't refetch.
 *
 * Degradation signal: a fetch that fails (backend unreachable) is NOT silent —
 * `useDegradation()` lets the shell show a banner. Without this, a dev proxy
 * aimed at the wrong port looks "working" for half a day.
 *
 * `/app` carries no sample data any more: a caller that omits `fallback` gets
 * `data: undefined` and `failed: true` once the request has settled without an
 * answer, and renders an explicit error state. That is deliberately NOT the same
 * shape as a live empty answer — "we could not ask" and "you have none" are
 * different facts, and a screen that draws the second when it means the first is
 * telling the reader their account is empty on the strength of a network error.
 *
 * The in-flight promise lives at MODULE level, not in a ref, and the effect
 * carries no "already fetched" guard. That is what makes the hook survive
 * StrictMode: React mounts, tears down and remounts every component in dev, so
 * an effect that guards on a ref starts the request on the first pass, marks
 * itself cancelled on the teardown, and then skips the second pass — the
 * response arrives with nobody left to receive it, and the screen keeps
 * rendering the mock fallback for good, silently (neither `.then` nor `.catch`
 * reaches state, so not even the degradation banner fires). Subscribing to a
 * shared promise instead means the remounted effect re-attaches to the same
 * request and applies the result. `useAccount` was immune for this reason,
 * which is why the sidebar showed live data while every other panel did not.
 *
 * Invalidation is a broadcast, not a cache drop (Aug 14, 2026). `invalidateApiData`
 * used to delete the entry and stop there, so nothing on screen knew: a source
 * connected on /app/sources appeared only after a reload, and deleting one of
 * three checks left the sidebar's "HTTP checks 3 / 3" standing. A cache every
 * reader has to be told about by hand is not a cache, it is a second copy of the
 * truth — so every mounted reader of an invalidated key refetches at once.
 */
import { useState, useEffect, useRef, useSyncExternalStore } from "react";

const cache = new Map<string, { data: unknown; ts: number }>();
const inflight = new Map<string, Promise<boolean>>();
// The last failure per key, for callers that branch on WHY the read failed
// (`isOffline`): a refusal is a different fact from "nobody answered". A
// successful read clears it.
const errors = new Map<string, unknown>();
const TTL = 30_000; // 30 seconds — short enough for live data, long enough for tab switches

// --- degradation store (module-level, reactive via useSyncExternalStore) ---
const degraded = new Set<string>();
const listeners = new Set<() => void>();
function emit() {
	listeners.forEach((l) => l());
}
function subscribe(cb: () => void) {
	listeners.add(cb);
	return () => {
		listeners.delete(cb);
	};
}
/** True when a read failed because the backend was unreachable. No args:
 *  any degraded key anywhere (the account shell's banner). With keys: only
 *  those keys — the public status page is key-scoped because the poller
 *  re-reads only mounted readers, so a degraded key left behind by an
 *  unmounted page would never clear on its own. */
export function useDegradation(...keys: string[]) {
	return useSyncExternalStore(
		subscribe,
		() =>
			keys.length === 0
				? degraded.size > 0
				: keys.some((key) => degraded.has(key)),
		() => false, // SSR: assume live (no fetches ran)
	);
}

function markLive(key: string) {
	if (degraded.delete(key)) emit();
}

function markDegraded(key: string) {
	if (!degraded.has(key)) {
		degraded.add(key);
		emit();
	}
}

// --- per-key invalidation store ---
// Who is reading this key right now. A mutation invalidates the key and every
// reader re-reads; nobody has to remember which screens share an entity.
const readers = new Map<string, Set<() => void>>();
// Bumped on every invalidation. A response that started before the write it
// raced carries the older generation and is dropped rather than cached: the
// answer predates the change, so caching it would put the deleted monitor back
// for a whole TTL.
const generation = new Map<string, number>();

function genOf(key: string) {
	return generation.get(key) ?? 0;
}

function subscribeKey(key: string, cb: () => void) {
	let set = readers.get(key);
	if (!set) {
		set = new Set();
		readers.set(key, set);
	}
	set.add(cb);
	return () => {
		set.delete(cb);
		if (set.size === 0) readers.delete(key);
	};
}

/** A cache entry that is still inside the TTL, or null. */
function fresh(key: string) {
	const entry = cache.get(key);
	return entry && Date.now() - entry.ts < TTL ? entry : null;
}

/**
 * Start (or join) the request for `key`. Resolves to whether the data is live:
 * true means `cache` holds a fresh response, false means the fetch failed and
 * the caller keeps its fallback.
 */
function load(key: string, fetcher: () => Promise<unknown>): Promise<boolean> {
	const existing = inflight.get(key);
	if (existing) return existing;

	const startedAt = genOf(key);
	const request: Promise<boolean> = fetcher()
		.then((result) => {
			// Invalidated while this was on the wire: it describes the state before
			// the write, so it is not an answer to the question now being asked.
			if (genOf(key) !== startedAt) return false;
			cache.set(key, { data: result, ts: Date.now() });
			errors.delete(key);
			markLive(key);
			return true;
		})
		.catch((err: unknown) => {
			markDegraded(key);
			errors.set(key, err);
			return false;
		})
		.finally(() => {
			// Only if it is still ours: an invalidation drops the entry mid-flight and
			// a newer request takes the slot, which this `finally` must not delete.
			if (inflight.get(key) === request) inflight.delete(key);
		});

	inflight.set(key, request);
	return request;
}

type State<T> = {
	key: string;
	data: T;
	/** The first answer (live or failed) has arrived. */
	settled: boolean;
	live: boolean;
	error: unknown;
};

export type ApiDataResult<T> = {
	data: T;
	/** "Nothing has answered yet", NOT "a request is in flight" — see below. */
	loading: boolean;
	live: boolean;
	/** Settled with nothing live to show — a failed *background* refetch keeps
	 *  the last live answer and stays false. Distinct from a live empty answer. */
	failed: boolean;
	/** The error behind `failed`, for callers that branch on its kind
	 * (`isOffline(err)`): undefined whenever the read is live. */
	error: unknown;
};

/** With a fallback (the public pages): `data` is always present. */
export function useApiData<T>(
	key: string,
	fetcher: () => Promise<T>,
	fallback: T,
): ApiDataResult<T>;
/** Without one (`/app`): `data` is undefined until the read succeeds. */
export function useApiData<T>(
	key: string,
	fetcher: () => Promise<T>,
): ApiDataResult<T | undefined>;
export function useApiData<T>(
	key: string,
	fetcher: () => Promise<T>,
	fallback?: T,
): ApiDataResult<T | undefined> {
	const initial = (forKey: string): State<T | undefined> => {
		const entry = fresh(forKey);
		return entry
			? {
					key: forKey,
					data: entry.data as T,
					settled: true,
					live: true,
					error: undefined,
				}
			: {
					key: forKey,
					data: fallback,
					settled: false,
					live: false,
					error: undefined,
				};
	};
	const [state, setState] = useState<State<T | undefined>>(() => initial(key));
	// A different entity is a different question: its answer has not arrived, so
	// the caller must not be told the previous one has settled. React's own
	// "adjust state during render" pattern — cheaper than an effect, which would
	// paint the old entity's data once first.
	if (state.key !== key) setState(initial(key));

	// Callers pass an inline arrow, so the fetcher is a new function every render
	// and cannot be an effect dependency. The ref keeps the latest one without
	// re-running the effect.
	const fetcherRef = useRef(fetcher);
	fetcherRef.current = fetcher;

	// Re-read on invalidation. `nonce` is what carries the broadcast into the
	// effect below, so the refetch runs through exactly the same path as the
	// first read — including the StrictMode-safe shared promise.
	const [nonce, setNonce] = useState(0);
	useEffect(
		() => subscribeKey(key, () => setNonce((current) => current + 1)),
		[key],
	);

	useEffect(() => {
		let cancelled = false;

		const entry = fresh(key);
		if (entry) {
			setState({
				key,
				data: entry.data as T,
				settled: true,
				live: true,
				error: undefined,
			});
			return;
		}

		load(key, () => fetcherRef.current()).then((live) => {
			if (cancelled) return;
			const loaded = cache.get(key);
			setState((current) => {
				// A failed BACKGROUND refetch keeps the answer already on screen:
				// the reader holds a live one, and flipping to `failed` here would
				// blank a working panel over a hiccup — the degradation banner
				// (`markDegraded`, in `load`'s catch) is the designed signal, not
				// this. The first answer path is unchanged: a read that fails with
				// nothing live to keep still settles as failed and renders the
				// error state.
				if (!live && current.live && current.data !== undefined) {
					return current;
				}
				return {
					key,
					// A failed fetch keeps whatever the caller already had: the fallback
					// for a public page, `undefined` for `/app` — which is what makes the
					// screen render its error state rather than an invented empty list.
					data: live && loaded ? (loaded.data as T) : current.data,
					settled: true,
					live,
					error: live ? undefined : errors.get(key),
				};
			});
		});

		return () => {
			cancelled = true;
		};
	}, [key, nonce]);

	return {
		data: state.data,
		// "Nothing has answered yet", NOT "a request is in flight". A refetch after
		// a write keeps the rows on screen and swaps them when the answer lands;
		// treating it as loading would blank the panel on every toggle.
		loading: !state.settled,
		live: state.live,
		// Settled with nothing to show. The caller draws a load error — never an
		// empty state, which would report a network failure as "you have none".
		failed: state.settled && !state.live,
		error: state.error,
	};
}

/**
 * Invalidate cache keys after a mutation, and tell every mounted reader to
 * re-read them. Variadic because one write is rarely one entity: a deleted
 * monitor changes the monitor list, the dashboard, the status page's components
 * and the plan's usage count, and a screen that names only its own key is how
 * the count kept saying 3 of 3.
 */
export function invalidateApiData(...keys: string[]) {
	for (const key of keys) {
		cache.delete(key);
		inflight.delete(key);
		generation.set(key, genOf(key) + 1);
		readers.get(key)?.forEach((notify) => notify());
	}
}

/** Invalidate all cached data (e.g. after sign-out). */
export function invalidateAllApiData() {
	invalidateApiData(...new Set([...cache.keys(), ...readers.keys()]));
}

// --- auto-refresh poller (module-level, outside React on purpose) ---
// Every mounted reader re-reads its key every 5 s while the tab is visible,
// through the same invalidation broadcast a write uses, so server-side change
// reaches the screen with no reload. StrictMode mounts and remounts components,
// but the tab owns this timer, not React: no cleanup, it lives for the life of
// the page. A hidden tab skips its ticks and refreshes once, immediately, when
// it becomes visible again instead of waiting for the next one.
// A tick skips keys with a request already in flight: a tick that dropped one
// would starve the key once latency exceeds POLL_MS (the generation guard
// drops the older answer), so the poll adapts to latency instead. Write
// invalidations are NOT filtered — they must still supersede racing reads.
const POLL_MS = 5_000;

function pollTick() {
	if (document.visibilityState === "visible" && readers.size > 0) {
		invalidateApiData(
			...[...readers.keys()].filter((key) => !inflight.has(key)),
		);
	}
}

if (typeof document !== "undefined") {
	setInterval(pollTick, POLL_MS);
	document.addEventListener("visibilitychange", () => {
		if (document.visibilityState === "visible") pollTick();
	});
}
