/** API client: one function per endpoint, shapes 1:1 with the generated api.d.ts. */

const BASE = ""; // relative — Vite proxy or Caddy handles the host

/** True when the request never got an answer (fetch TypeError), not a refusal. */
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
		// Session is gone: hard-redirect to /signin unless already there.
		if (!window.location.pathname.startsWith("/signin")) {
			window.location.assign("/signin");
		}
		throw new Error("unauthorized");
	}
	if (!resp.ok) {
		// A 402/429 renders as the server's own message, like any other refusal.
		const body = await resp
			.json()
			.catch(() => ({ error: { message: resp.statusText } }));
		throw new Error(body.error?.message || `HTTP ${resp.status}`);
	}
	if (resp.status === 204) return undefined as T;
	return resp.json();
}

import type { components } from "./api";

export type { components };

export const auth = {
	magicLink: (email: string, token?: string) =>
		fetchJSON("/v1/auth/magic-link", {
			method: "POST",
			body: JSON.stringify({ email, token }),
		}),
	logout: () => fetchJSON("/v1/auth/logout", { method: "POST" }),
};

export const me = () =>
	fetchJSON<components["schemas"]["MeResponse"]>("/v1/me");

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
// One-time install token: the key never travels here; the CLI redeems the
// token server-side and writes .env itself.
export const installToken = () =>
	fetchJSON<{ token: string; command: string; expiresAt: string }>(
		"/v1/install/token",
		{ method: "POST" },
	);

export const sourcesWrite = {
	// `activate` promotes the hidden draft into the visible card; a plain
	// connect only fetches the hook URL.
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

export const channels = () =>
	fetchJSON<components["schemas"]["ChannelsResponse"]>("/v1/channels");

// A channel is a destination: one field to add, one test per row.
export const channelsWrite = {
	create: (kind: string, target: string) =>
		fetchJSON<components["schemas"]["AlertChannel"]>("/v1/channels", {
			method: "POST",
			body: JSON.stringify({ kind, target }),
		}),
	delete: (id: string) => fetchJSON(`/v1/channels/${id}`, { method: "DELETE" }),
	// Queues a real delivery and answers with its queue id; the outcome comes
	// from delivery(id) below.
	test: (id: string) =>
		fetchJSON<components["schemas"]["DeliveryStatusResponse"]>(
			`/v1/channels/${id}/test`,
			{ method: "POST" },
		),
};

/** One queue row's truth: `sent` only when the pipeline says so, `dead` with
 *  the reason it died, anything else = the outcome is not known yet. */
export const delivery = (id: string) =>
	fetchJSON<components["schemas"]["DeliveryStatusResponse"]>(
		`/v1/deliveries/${id}`,
	);

export const incidents = () =>
	fetchJSON<{ items: components["schemas"]["Incident"][] }>("/v1/incidents");
export const incident = (id: string) =>
	fetchJSON<components["schemas"]["Incident"]>(`/v1/incidents/${id}`);

/** Filters apply server-side before the stream limit, so lines and `total` stay
 *  consistent; `service` repeats because a service may be the empty string. */
export const logs = (
	services?: readonly string[],
	levels?: readonly string[],
	range?: { from: number; to: number } | null,
	bucketSeconds?: number,
) => {
	const params = new URLSearchParams();
	for (const service of services ?? []) params.append("service", service);
	for (const level of levels ?? []) params.append("level", level);
	if (range) {
		params.set("from", new Date(range.from).toISOString());
		params.set("to", new Date(range.to).toISOString());
		// Sent only with a range; the answer may be coarser and carries the
		// width the server actually used.
		if (bucketSeconds && bucketSeconds > 0) {
			params.set("bucketSeconds", String(bucketSeconds));
		}
	}
	const query = params.toString();
	return fetchJSON<components["schemas"]["LogsResponse"]>(
		query ? `/v1/logs?${query}` : "/v1/logs",
	);
};

// Write-only by design: values are sealed server-side. Presence is read
// through the channels screen's telegram surface, never read back from here.
export const instance = {
	putTelegramBot: (token: string, username: string) =>
		fetchJSON<undefined>("/v1/instance/telegram-bot", {
			method: "PUT",
			body: JSON.stringify({ token, username }),
		}),
	putSMTP: (values: { host?: string; port?: string; username?: string; password?: string; from?: string }) =>
		fetchJSON<undefined>("/v1/instance/smtp", {
			method: "PUT",
			body: JSON.stringify(values),
		}),
	deleteSMTP: () => fetchJSON<undefined>("/v1/instance/smtp", { method: "DELETE" }),
};

export const statusPage = {
	get: () =>
		fetchJSON<components["schemas"]["StatusPageResponse"]>("/v1/status-page"),
	// Only the owner's decisions are sent: components and uptime are measured
	// server-side.
	put: (data: components["schemas"]["StatusPageUpdate"]) =>
		fetchJSON<components["schemas"]["StatusPageResponse"]>("/v1/status-page", {
			method: "PUT",
			body: JSON.stringify(data),
		}),
};

/** Nothing is created: the server probes the host and answers with pick-list
 *  groups; monitors exist only once Start watching posts them. */
export const publicCheck = (host: string) =>
	fetchJSON<components["schemas"]["CheckResponse"]>("/public/check", {
		method: "POST",
		body: JSON.stringify({ host }),
	});

export const publicStatus = (slug: string) =>
	fetchJSON<components["schemas"]["PublicStatusResponse"]>(
		`/public/status/${slug}`,
	);
