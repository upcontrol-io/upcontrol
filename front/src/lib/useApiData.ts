/** Shared-promise fetch cache: the in-flight promise lives at module level so
 *  StrictMode remounts re-attach; invalidation broadcasts to every reader. */
import { useState, useEffect, useRef, useSyncExternalStore } from "react";

const cache = new Map<string, { data: unknown; ts: number }>();
const inflight = new Map<string, Promise<boolean>>();
// Last failure per key, for callers that branch on why the read failed
// (isOffline): a refusal is a different fact from "nobody answered".
const errors = new Map<string, unknown>();
const TTL = 30_000; // 30 seconds — short enough for live data, long enough for tab switches

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
/** True when a read failed as unreachable. No args: any key anywhere (the
 *  shell banner); with keys: only those, so an unmounted page cannot stick. */
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

// Who is reading this key right now: a mutation makes every reader re-read,
// so no screen has to know which others share the entity.
const readers = new Map<string, Set<() => void>>();
// Bumped on every invalidation: a response that raced a write carries the
// older generation and is dropped, not cached.
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

/** Start (or join) the request for `key`; resolves to whether the data is
 *  live (cached) or the fetch failed and the caller keeps its fallback. */
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
	/** "Nothing has answered yet", NOT "a request is in flight". */
	loading: boolean;
	live: boolean;
	/** Settled with nothing live to show; a failed background refetch keeps the
	 *  last live answer and stays false. Distinct from a live empty answer. */
	failed: boolean;
	/** The error behind `failed`, for callers that branch on its kind via
	 * isOffline(err); undefined whenever the read is live. */
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
	// A different entity is a different question: adjust during render, cheaper
	// than an effect, which would paint the old entity's data once first.
	if (state.key !== key) setState(initial(key));

	// The fetcher is a new function every render (inline arrows); the ref keeps
	// the latest one without re-running the effect.
	const fetcherRef = useRef(fetcher);
	fetcherRef.current = fetcher;

	// `nonce` carries the broadcast into the effect below, so a refetch runs
	// the same path as the first read, shared promise included.
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
				// A failed background refetch keeps the live answer already on screen;
				// the degradation banner is the signal, not this.
				if (!live && current.live && current.data !== undefined) {
					return current;
				}
				return {
					key,
					// A failed fetch keeps what the caller had (fallback or undefined),
					// so the screen renders its error state, not an invented empty list.
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
		// "Nothing has answered yet", not "a request is in flight": a refetch
		// keeps the rows on screen and swaps them when the answer lands.
		loading: !state.settled,
		live: state.live,
		// Settled with nothing to show: draw a load error, never an empty state
		// that reports a network failure as "you have none".
		failed: state.settled && !state.live,
		error: state.error,
	};
}

/** Invalidate keys after a mutation and tell every mounted reader to re-read
 *  them; variadic because one write touches several entities. */
export function invalidateApiData(...keys: string[]) {
	for (const key of keys) {
		cache.delete(key);
		inflight.delete(key);
		generation.set(key, genOf(key) + 1);
		readers.get(key)?.forEach((notify) => notify());
	}
}

// Every mounted reader re-reads its key every 5 s while the tab is visible,
// through the same broadcast a write uses; ticks skip keys already in flight.
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
