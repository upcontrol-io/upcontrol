/**
 * API client — the single fetch layer between the front and the backend.
 * The shapes match the OpenAPI spec (src/lib/api.d.ts, generated) 1:1.
 *
 * Everything the self-hosted app serves is here, one function per endpoint —
 * no calls to surfaces this server does not mount.
 * In dev the Vite proxy forwards /v1/* and /public/* to the
 * Caddy edge; in production Caddy reverse-proxies directly.
 */

const BASE = ""; // relative — Vite proxy or Caddy handles the host

/**
 * True when the failure was "there is no backend" rather than "the backend
 * said no". fetch rejects with a TypeError only when the request never got an
 * answer; anything else is a real refusal and has to reach the reader.
 */
export function isOffline(err: unknown): boolean {
	return err instanceof TypeError;
}

async function fetchJSON<T>(path: string, init?: RequestInit): Promise<T> {
	const resp = await fetch(BASE + path, {
		credentials: "include", // send the uc_session cookie
		headers: { "Content-Type": "application/json", ...(init?.headers || {}) },
		...init,
	});
	if (resp.status === 401) {
		// The session is gone. With UC_AUTH=none this never fires — every
		// request carries the boot identity. One spelling: /signin.
		if (!window.location.pathname.startsWith("/signin")) {
			window.location.assign("/signin");
		}
		throw new Error("unauthorized");
	}
	if (!resp.ok) {
		// No paid wall in OSS: a 402/429 renders as the server's own words,
		// like any other refusal (public-first-split, Decision 8).
		const body = await resp
			.json()
			.catch(() => ({ error: { message: resp.statusText } }));
		throw new Error(body.error?.message || `HTTP ${resp.status}`);
	}
	if (resp.status === 204) return undefined as T;
	return resp.json();
}

// --- types from the generated OpenAPI spec (api.d.ts) ---
import type { components } from "./api";

// Re-export for convenience.
export type { components };

// --- auth ---
export const auth = {
	magicLink: (email: string, token?: string) =>
		fetchJSON("/v1/auth/magic-link", {
			method: "POST",
			body: JSON.stringify({ email, token }),
		}),
	logout: () => fetchJSON("/v1/auth/logout", { method: "POST" }),
};

// --- account ---
export const me = () =>
	fetchJSON<components["schemas"]["MeResponse"]>("/v1/me");

// --- dashboard ---
export const overview = () =>
	fetchJSON<components["schemas"]["OverviewResponse"]>("/v1/overview");

// --- monitors ---
export const monitors = {
	list: () => fetchJSON<components["schemas"]["Monitor"][]>("/v1/monitors"),
	create: (data: components["schemas"]["MonitorCreate"]) =>
		fetchJSON<components["schemas"]["Monitor"]>("/v1/monitors", {
			method: "POST",
			body: JSON.stringify(data),
		}),
	patch: (id: string, data: components["schemas"]["MonitorPatch"]) =>
		fetchJSON<components["schemas"]["Monitor"]>(`/v1/monitors/${id}`, {
			method: "PATCH",
			body: JSON.stringify(data),
		}),
	delete: (id: string) => fetchJSON(`/v1/monitors/${id}`, { method: "DELETE" }),
};

// --- sources + keys ---
export const sources = () =>
	fetchJSON<components["schemas"]["SourcesResponse"]>("/v1/sources");
export const keys = () =>
	fetchJSON<components["schemas"]["KeysResponse"]>("/v1/keys");
// Rotate returns the FULL key exactly once. The caller shows it once and
// never stores it.
export const rotateKey = () =>
	fetchJSON<{ id: string; value: string; createdAt: string }>(
		"/v1/keys/rotate",
		{ method: "POST" },
	);
// One-time install token for the Settings screen's command. The response
// carries the ready command; the KEY never travels here — the CLI redeems
// the token server-side and writes .env itself.
export const installToken = () =>
	fetchJSON<{ token: string; command: string; expiresAt: string }>(
		"/v1/install/token",
		{ method: "POST" },
	);

// --- sources (connect / pause / disconnect) ---
export const sourcesWrite = {
	// `activate` promotes the hidden draft into the visible card — it is the
	// copy button speaking, while a plain connect only fetches the hook URL.
	connect: (kind: string, activate = false) =>
		fetchJSON<components["schemas"]["Source"]>(`/v1/sources/${kind}/connect`, {
			method: "POST",
			headers: { "Content-Type": "application/json" },
			body: JSON.stringify({ activate }),
		}),
	setPaused: (id: string, paused: boolean) =>
		fetchJSON(`/v1/sources/${id}`, {
			method: "PATCH",
			body: JSON.stringify({ paused }),
		}),
	delete: (id: string) => fetchJSON(`/v1/sources/${id}`, { method: "DELETE" }),
};

// --- alerts ---
export const channels = () =>
	fetchJSON<components["schemas"]["ChannelsResponse"]>("/v1/channels");

// A channel is a destination and nothing else: one field to add, one test
// per row, no per-monitor rule matrix.
export const channelsWrite = {
	create: (kind: string, target: string) =>
		fetchJSON<components["schemas"]["AlertChannel"]>("/v1/channels", {
			method: "POST",
			body: JSON.stringify({ kind, target }),
		}),
	delete: (id: string) => fetchJSON(`/v1/channels/${id}`, { method: "DELETE" }),
	// Queues a REAL delivery and answers with its queue id — never a claim
	// that anything was sent. The outcome comes from `delivery(id)` below.
	test: (id: string) =>
		fetchJSON<components["schemas"]["DeliveryStatusResponse"]>(
			`/v1/channels/${id}/test`,
			{ method: "POST" },
		),
};

// --- deliveries (the outcome half of Send test) ---
/** One queue row's truth: `sent` only when the pipeline says so, `dead` with
 *  the reason it died, anything else = the outcome is not known yet. */
export const delivery = (id: string) =>
	fetchJSON<components["schemas"]["DeliveryStatusResponse"]>(
		`/v1/deliveries/${id}`,
	);

// --- incidents ---
export const incidents = () =>
	fetchJSON<{ items: components["schemas"]["Incident"][] }>("/v1/incidents");
export const incident = (id: string) =>
	fetchJSON<components["schemas"]["Incident"]>(`/v1/incidents/${id}`);

// --- logs (the live ring window) ---
/**
 * The window, optionally narrowed to a set of services and level buckets, and
 * optionally bounded to the range picked on the timeline. Every narrowing is
 * the server's: they apply before the stream limit, so the lines and the
 * `total` beside them keep describing the same question. The filter params
 * repeat (`?service=api&service=web`) — a service may legitimately be the
 * empty string (the unlabelled service), which a joined form could not carry.
 */
export const logs = (
	services?: readonly string[],
	levels?: readonly string[],
	range?: { from: number; to: number } | null,
) => {
	const params = new URLSearchParams();
	for (const service of services ?? []) params.append("service", service);
	for (const level of levels ?? []) params.append("level", level);
	if (range) {
		params.set("from", new Date(range.from).toISOString());
		params.set("to", new Date(range.to).toISOString());
	}
	const query = params.toString();
	return fetchJSON<components["schemas"]["LogsResponse"]>(
		query ? `/v1/logs?${query}` : "/v1/logs",
	);
};

/** The triage shape behind Explain: problem as fact, cause as a graded guess. */
export type ExplainResult = components["schemas"]["ExplainResponse"];

/** The AI read of a set of log lines. With no API key configured anywhere
 *  the server answers 503 ai_not_configured. Render its message rather than
 *  a local string: on a self-host it names Settings, the door that takes a
 *  key, and on a hosted instance it does not, because that door is not the
 *  caller's to open. */
export const explainLogs = (lines: string[]) =>
	fetchJSON<ExplainResult>("/v1/logs/explain", {
		method: "POST",
		body: JSON.stringify({ lines }),
	});

// --- instance settings (self-host only doors) ---
// Write-only by design: the values are sealed server-side and never travel
// back out. Presence is read through explainPreview's `model` (the AI key)
// and the channels screen's telegram surface (the bot).
export const instance = {
	// Each field optional: send what the operator filled, the server stores
	// only that — a model change never demands re-pasting the key.
	putAI: (values: { key?: string; model?: string; baseUrl?: string }) =>
		fetchJSON<undefined>("/v1/instance/ai", {
			method: "PUT",
			body: JSON.stringify(values),
		}),
	deleteAI: () => fetchJSON<undefined>("/v1/instance/ai", { method: "DELETE" }),
	putTelegramBot: (token: string, username: string) =>
		fetchJSON<undefined>("/v1/instance/telegram-bot", {
			method: "PUT",
			body: JSON.stringify({ token, username }),
		}),
	deleteTelegramBot: () =>
		fetchJSON<undefined>("/v1/instance/telegram-bot", { method: "DELETE" }),
	putSMTP: (values: { host?: string; port?: string; username?: string; password?: string; from?: string }) =>
		fetchJSON<undefined>("/v1/instance/smtp", {
			method: "PUT",
			body: JSON.stringify(values),
		}),
	deleteSMTP: () => fetchJSON<undefined>("/v1/instance/smtp", { method: "DELETE" }),
};

/**
 * The AI read of one incident. The server assembles the evidence itself
 * (incident facts, timeline, frozen log slice) from what it already has, so
 * the request carries only the id; `severity`/`area` are set only by this
 * scenario (null from the log-selection explain).
 */
export const explainIncident = (id: string) =>
	fetchJSON<ExplainResult>(`/v1/incidents/${id}/explain`, {
		method: "POST",
	});

/**
 * The exact prompt an Explain with these lines would send — composed, not
 * executed: no model is consulted and no quota is spent. Settings reads the
 * wired brain's identity (`model`) from it, which is the one place the
 * contract names the brain.
 */
export const explainPreview = (lines: string[]) =>
	fetchJSON<components["schemas"]["ExplainPreviewResponse"]>(
		"/v1/logs/explain/preview",
		{
			method: "POST",
			body: JSON.stringify({ lines }),
		},
	);

// --- status page ---
export const statusPage = {
	get: () =>
		fetchJSON<components["schemas"]["StatusPageResponse"]>("/v1/status-page"),
	// Only the owner's decisions are sent: the components and their uptime are
	// measured server-side and would be ignored anyway.
	put: (data: components["schemas"]["StatusPageUpdate"]) =>
		fetchJSON<components["schemas"]["StatusPageResponse"]>("/v1/status-page", {
			method: "PUT",
			body: JSON.stringify(data),
		}),
};

// --- public ---
/** The public discovery check — the onboarding's scan. Nothing is created:
 *  the server probes the host (depth zero, its own request cap) and answers
 *  with pick-list groups; monitors exist only once Start watching posts them. */
export const publicCheck = (host: string) =>
	fetchJSON<components["schemas"]["CheckResponse"]>("/public/check", {
		method: "POST",
		body: JSON.stringify({ host }),
	});

export const publicStatus = (slug: string) =>
	fetchJSON<components["schemas"]["PublicStatusResponse"]>(
		`/public/status/${slug}`,
	);
