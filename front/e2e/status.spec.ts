import { test, expect } from "@playwright/test";
import { DOMAIN, servePublicStatus, stubApi } from "./fixtures/api";

const PUBLIC_PAGE = {
	slug: "example-com",
	title: DOMAIN,
	components: [
		{
			key: "mon_1",
			name: DOMAIN,
			shown: true,
			uptime: "99.98%",
			// A fresh target's first rung: 96 x 900 s.
			bars: Array.from({ length: 96 }, () => "ok" as const),
			barSpanSec: 900,
		},
	],
	incidents: [],
	network: [],
	updatedAt: new Date().toISOString(),
	poweredBy: true,
};

test("the config screen owns the URL and the Powered by switch", async ({ page }) => {
	await stubApi(page);
	await page.goto("/status");

	await expect(page.getByRole("heading", { name: "Status page" })).toBeVisible();
	await expect(page.getByText("Public URL")).toBeVisible();
	await expect(page.getByText(/\/status\/example-com/)).toBeVisible();

	// The OSS difference: "Powered by" is a real switch, default on.
	const poweredBy = page.getByRole("switch", { name: /Powered by UpControl/ });
	await expect(poweredBy).toHaveAttribute("aria-checked", "true");
	await poweredBy.click();
	await expect(poweredBy).toHaveAttribute("aria-checked", "false");
});

test("the public page renders the banner, the bars and the footer", async ({ page }) => {
	await servePublicStatus(page, "example-com", PUBLIC_PAGE);
	await page.route("**/public/track", (route) => route.fulfill({ status: 204, body: "" }));
	await page.goto("/status/example-com");

	await expect(page.getByRole("heading", { name: "All systems operational" })).toBeVisible();
	await expect(page.getByText(`${DOMAIN} status`)).toBeVisible();
	await expect(page.getByText("No incidents recorded.", { exact: false })).toBeVisible();
	// Default on: the footer links back.
	await expect(page.getByText("Powered by")).toBeVisible();
});

test("poweredBy off removes the footer — removed means gone, not empty", async ({ page }) => {
	await servePublicStatus(page, "example-com", { ...PUBLIC_PAGE, poweredBy: false });
	await page.route("**/public/track", (route) => route.fulfill({ status: 204, body: "" }));
	await page.goto("/status/example-com");

	await expect(page.getByRole("heading", { name: "All systems operational" })).toBeVisible();
	await expect(page.getByText("Powered by")).toHaveCount(0);
	// The incident-history assertion that used to ride along here pinned the
	// showIncidents switch, which is deleted: the section is no longer something
	// an owner can turn off, so there is nothing left to assert about it.
});

// The contract's whole ladder: the bucket widens as a target's history grows
// while the window stays 24 h/48 h/7 days/30 days. Each rung gets its own
// strip so a regression in one (say the 48 h rung, which a days-first label
// would print "2 days") cannot hide behind the others passing. No bucket is
// a day wide any more, so the axis always ends "now" — the old day-bucketed
// 30-day rung ending "today" is gone. Two entries are cadence-floor shapes
// (a coarse-interval target's bucket stepped up past the rung's base
// bucket): the floor still has to land on the same window/label as the
// un-floored rung it replaces.
const RUNGS = [
	{ spanSec: 900, count: 96, spanText: "24 h" },
	{ spanSec: 1800, count: 96, spanText: "48 h" },
	{ spanSec: 7200, count: 84, spanText: "7 days" },
	{ spanSec: 43200, count: 60, spanText: "30 days" },
	// Cadence floor: a 3600 s-interval target on the 24 h rung steps up to
	// 2 h buckets (2 × 3600 s), 12 of them.
	{ spanSec: 7200, count: 12, spanText: "24 h" },
	// Cadence floor: any target coarse enough to step all the way to the
	// 43200 s bucket on the 7-day rung, 14 of them.
	{ spanSec: 43200, count: 14, spanText: "7 days" },
];

for (const rung of RUNGS) {
	test(`a ${rung.count}-bar ${rung.spanSec}s strip reads "${rung.spanText}" with axis ending "now"`, async ({
		page,
	}) => {
		await servePublicStatus(page, "example-com", {
			...PUBLIC_PAGE,
			components: [
				{
					...PUBLIC_PAGE.components[0],
					bars: Array.from({ length: rung.count }, () => "ok" as const),
					barSpanSec: rung.spanSec,
				},
			],
		});
		await page.route("**/public/track", (route) => route.fulfill({ status: 204, body: "" }));
		await page.goto("/status/example-com");

		await expect(page.getByText(`${rung.spanText} of history`)).toBeVisible();
		await expect(page.getByText(`99.98% · ${rung.spanText}`)).toBeVisible();
		await expect(page.getByText(`${rung.spanText} ago`)).toBeVisible();
		await expect(page.getByText("now", { exact: true })).toBeVisible();
	});
}

test.describe("bar tooltip alignment", () => {
	// Local-clock text (the day prefix, the hour range) must read the same
	// everywhere this suite runs, not just on a UTC machine.
	test.use({ timezoneId: "UTC" });

	test("a bar's tooltip is anchored to the server's updatedAt, not a skewed browser clock, and carries a date once the strip spans more than a day", async ({
		page,
	}) => {
		// Off the 2 h boundary by 47m12s: the floor (`Math.floor(nowMs / spanMs) * spanMs`
		// in statusBars.ts) has to bring this back down to 18:00 itself, or every
		// bucket below prints an offset range instead of a clean one.
		const updatedAt = "2026-09-20T18:47:12.000Z";
		// The visitor's own clock is stuck 15 h ahead, on the next day: if the
		// tooltip read it instead of `updatedAt`, every bucket would shift by
		// one and this assertion would fail.
		await page.clock.install({ time: new Date("2026-09-21T09:00:00.000Z") });
		await servePublicStatus(page, "example-com", {
			...PUBLIC_PAGE,
			updatedAt,
			components: [
				{
					...PUBLIC_PAGE.components[0],
					// 84 x 2 h = 7 days: the 7-day rung.
					bars: ["down", ...Array.from({ length: 83 }, () => "ok" as const)],
					barSpanSec: 7200,
				},
			],
		});
		await page.route("**/public/track", (route) => route.fulfill({ status: 204, body: "" }));
		await page.goto("/status/example-com");

		// Oldest bar (index 0 of 84): updatedAt floors to 2026-09-20T18:00Z, then
		// − 83×2 h = 2026-09-13T20:00Z. count × spanSec = 604800 s > a day, so
		// the range carries the date.
		const oldestBar = page.locator("[data-grow]").first();
		await oldestBar.hover();
		await expect(page.getByRole("tooltip")).toHaveText("Sep 13 20:00–22:00 · down");
	});

	test("a 96x900s strip is exactly a day (not more), so its tooltips carry no date prefix", async ({
		page,
	}) => {
		// 96 × 900 s = 86400 s, exactly DAY_SEC: the date prefix only starts
		// past a day, so this rung must stay bare "hh:mm–hh:mm" ranges.
		const updatedAt = "2026-09-20T14:37:00.000Z";
		await servePublicStatus(page, "example-com", { ...PUBLIC_PAGE, updatedAt });
		await page.route("**/public/track", (route) => route.fulfill({ status: 204, body: "" }));
		await page.goto("/status/example-com");

		// Oldest bar (index 0 of 96): updatedAt floors to 2026-09-20T14:30Z,
		// then − 95×15 min = 2026-09-19T14:45Z.
		const oldestBar = page.locator("[data-grow]").first();
		await oldestBar.hover();
		await expect(page.getByRole("tooltip")).toHaveText("14:45–15:00 · up");
	});
});
