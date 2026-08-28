import type { Page, Route } from "@playwright/test";

/** A stand-in backend for the specs: the values are the ones they assert on,
 *  so changing a fixture fails the test that names it. Shapes: openapi.yaml. */

export const EMAIL = "anna@example.com";
export const DOMAIN = "example.com";
export const DEV_TOKEN = "devtok123";

const json = (route: Route, body: unknown, status = 200) =>
	route.fulfill({
		status,
		contentType: "application/json",
		body: JSON.stringify(body),
	});

const parseBody = (route: Route): Record<string, any> => {
	try {
		return JSON.parse(route.request().postData() ?? "{}");
	} catch {
		return {};
	}
};

const ME = {
	account: {
		id: "acc_1",
		name: "Anna Petrova",
		email: EMAIL,
		initials: "AP",
		plan: "Self-hosted",
		billing: "annual",
	},
	project: { id: "prj_1", domain: DOMAIN, createdAt: "2026-06-01T09:00:00Z" },
};

// Website only: the OSS create form offers URL checks and nothing else;
// the fixture list must not smuggle a Heartbeat back in.
const MONITORS = [
	{
		id: "mon_1",
		type: "Website",
		name: DOMAIN,
		target: `https://${DOMAIN}`,
		status: "ok",
		interval: "5m",
		// mon_2 carries neither half: an absent half means "not looked yet",
		// and beside mon_1 that proves the screen omits it, not placeholders it.
		keyword: "Checkout",
		expiry: { ssl: "SSL Nov 2, in 84 days", domain: "domain Mar 14, in 217 days" },
	},
	{
		id: "mon_2",
		type: "Website",
		name: `${DOMAIN}/checkout`,
		target: `https://${DOMAIN}/checkout`,
		status: "ok",
		interval: "5m",
	},
];

const bars = (n: number) => Array.from({ length: n }, () => "ok" as const);

const SOURCES = {
	sources: [
		{
			// The marks are the backend's, verbatim (read_api.go), so SourceIcon
			// resolves them and an icon regression can fail a test.
			id: "src_checks",
			mark: "URL",
			name: "Site checks",
			status: "ok",
			lastSignal: "1 min ago",
			paused: false,
		},
		{
			id: "src_logs",
			mark: "LOG",
			name: "App logs",
			status: "ok",
			lastSignal: "4s ago",
			paused: false,
		},
	],
	connectableSources: [
		{
			key: "installer",
			name: "Add it to your code",
			setupTime: "1 command",
			installer: true,
		},
		{ key: "deployhooks", name: "Deploy hooks", setupTime: "2 min" },
	],
};

const CHANNELS_BASE = {
	channels: [{ id: "ch_mail", kind: "email", target: EMAIL }],
	connectableChannels: [
		{
			kind: "email",
			name: "Email",
			field: "Email address",
			placeholder: "you@company.com",
			hint: "Alerts by mail, through this instance's SMTP.",
		},
		{
			kind: "telegram",
			name: "Telegram",
			field: "Chat",
			link: "https://t.me/upcontrol_test_bot?start=fixture",
			hint: "Press Start and the bot binds the chat when it hears from you.",
		},
	],
	undelivered: 0,
};

const INCIDENTS = { items: [] as unknown[] };

const LOGS = { lines: [], volume: [], total: 0, services: [] };

/** Serve the app's reads, signed in. Routes are per-page state: a write
 *  survives the re-read that follows, so specs test the round trip. */
export async function stubApi(page: Page, opts?: { monitors?: Record<string, unknown>[] }) {
	const monitors = (opts?.monitors ?? MONITORS).map((m) => ({ ...m })) as Record<string, unknown>[];
	const channels = CHANNELS_BASE.channels.map((c) => ({ ...c }) as Record<string, unknown>);
	let created = 0;
	const statusPage = {
		slug: "example-com",
		title: DOMAIN,
		domain: "",
		showNetwork: true,
		showIncidents: true,
		showPoweredBy: true,
	};

	// Anything not named below is a 404 rather than a hang: a spec that starts
	// depending on a new endpoint should fail loudly, not time out.
	await page.route("**/v1/**", (route) => json(route, { error: { message: "not stubbed" } }, 404));
	await page.route("**/public/track", (route) => route.fulfill({ status: 204, body: "" }));

	await page.route("**/v1/me", (route) => json(route, ME));
	await page.route("**/v1/overview", (route) =>
		json(route, {
			sources: SOURCES.sources,
			monitors,
			metrics: [],
			ladder: [],
			uptime: { value: "99.98%", note: "last 24 h" },
		}),
	);
	await page.route("**/v1/incidents", (route) => json(route, INCIDENTS));
	await page.route("**/v1/sources", (route) => json(route, SOURCES));
	await page.route("**/v1/sources/*/connect", (route) =>
		json(route, {
			id: "src_hook_1",
			kind: route.request().url().split("/").slice(-2)[0],
			name: "Deploy hooks",
			status: "nodata",
			lastSignal: "",
			paused: false,
			hookToken: "htok123",
		}),
	);
	await page.route("**/v1/keys", (route) =>
		json(route, {
			key: {
				id: "key_1",
				prefix: "uc_live_a1b2c3d4e5f6",
				createdAt: "2026-06-01T09:00:00Z",
			},
			usage: [],
		}),
	);
	await page.route("**/v1/install/token", (route) =>
		json(route, {
			token: "tok_fixture",
			command: "npx upcontrol init --token tok_fixture --endpoint http://localhost:5199",
			expiresAt: "2026-09-01T00:00:00Z",
		}),
	);
	await page.route("**/v1/channels", (route) => {
		if (route.request().method() === "POST") {
			const body = parseBody(route);
			const row = { id: `ch_new_${++created}`, kind: body.kind, target: body.target };
			channels.push(row);
			return json(route, row, 201);
		}
		return json(route, { ...CHANNELS_BASE, channels });
	});
	await page.route("**/v1/channels/*", (route) => {
		const id = route.request().url().split("/").pop() ?? "";
		if (route.request().method() === "DELETE") {
			const at = channels.findIndex((c) => c.id === id);
			if (at >= 0) channels.splice(at, 1);
			return json(route, {}, 204);
		}
		return json(route, channels.find((c) => c.id === id) ?? {});
	});
	// Send test queues a real delivery; the default fixture answers `dead`
	// with the reason a stack without a mailer produces.
	await page.route("**/v1/channels/*/test", (route) =>
		json(route, { id: "dlv_test_1", state: "pending" }, 202),
	);
	await page.route("**/v1/deliveries/*", (route) =>
		json(route, {
			id: "dlv_test_1",
			state: "dead",
			deadReason: "no sender for kind email",
		}),
	);
	await page.route("**/v1/logs", (route) => json(route, LOGS));
	await page.route("**/v1/status-page", (route) => {
		if (route.request().method() === "PUT") {
			const body = parseBody(route);
			statusPage.showNetwork = body.showNetwork ?? statusPage.showNetwork;
			statusPage.showIncidents = body.showIncidents ?? statusPage.showIncidents;
			statusPage.showPoweredBy = body.showPoweredBy ?? statusPage.showPoweredBy;
			statusPage.title = body.title ?? statusPage.title;
		}
		return json(route, {
			...statusPage,
			components: monitors.map((monitor) => ({
				key: (monitor as any).id,
				name: (monitor as any).name,
				shown: true,
				uptime: "99.98%",
				bars: bars(7),
				barSpanSec: 86400,
			})),
		});
	});

	await page.route("**/v1/monitors", (route) => {
		if (route.request().method() === "POST") {
			const body = parseBody(route);
			const row = { id: `mon_new_${++created}`, status: "nodata", ...body };
			monitors.push(row);
			return json(route, row);
		}
		return json(route, monitors);
	});
	await page.route("**/v1/monitors/*", (route) => {
		const id = route.request().url().split("/").pop() ?? "";
		if (route.request().method() === "DELETE") {
			const at = monitors.findIndex((monitor) => monitor.id === id);
			if (at >= 0) monitors.splice(at, 1);
			return json(route, {});
		}
		if (route.request().method() === "PATCH") {
			const body = parseBody(route);
			const row = monitors.find((monitor) => monitor.id === id);
			if (row) Object.assign(row, body);
			return json(route, row ?? {});
		}
		return json(route, monitors.find((monitor) => monitor.id === id) ?? {});
	});
}

/** The signed-out posture for the SignIn spec: /v1/me answers 401 until the
 *  magic-link redeem flips the fixture to authenticated. */
export async function stubSignedOut(page: Page) {
	let authenticated = false;
	await page.route("**/v1/**", (route) => json(route, { error: { message: "not stubbed" } }, 404));
	await page.route("**/public/track", (route) => route.fulfill({ status: 204, body: "" }));
	await page.route("**/v1/me", (route) =>
		authenticated ? json(route, ME) : json(route, { error: { message: "unauthorized" } }, 401),
	);
	await page.route("**/v1/auth/magic-link", (route) => {
		const body = parseBody(route);
		if (body.token) {
			authenticated = true;
			return json(route, { id: "acc_1", email: body.email, plan: "Self-hosted" });
		}
		return json(route, { dev_token: DEV_TOKEN }, 202);
	});
	// The app shell mounts these once the redeem lands.
	await page.route("**/v1/monitors", (route) =>
		authenticated ? json(route, MONITORS) : json(route, { error: { message: "unauthorized" } }, 401),
	);
	await page.route("**/v1/overview", (route) =>
		json(route, { sources: [], monitors: [], metrics: [], ladder: [], uptime: { value: "", note: "" } }),
	);
	await page.route("**/v1/incidents", (route) => json(route, INCIDENTS));
}

/** Serve GET /v1/incidents with these rows — a later registration wins. */
export async function serveIncidents(page: Page, items: unknown[]) {
	await page.route("**/v1/incidents", (route) => json(route, { items }));
}

/** Serve GET /v1/incidents/{id} — the detail page's own read. */
export async function serveIncident(page: Page, incident: { id: string }) {
	await page.route(`**/v1/incidents/${incident.id}`, (route) => json(route, incident));
}

/** Serve GET /public/status/{slug} for the public renderer's spec. */
export async function servePublicStatus(page: Page, slug: string, body: unknown) {
	await page.route(`**/public/status/${slug}`, (route) => json(route, body));
}
