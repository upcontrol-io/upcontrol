import { test, expect } from "@playwright/test";
import { DOMAIN, stubApi } from "./fixtures/api";

test.beforeEach(async ({ page }) => {
	await stubApi(page);
});

test("the list renders the account's checks and nothing invented", async ({ page }) => {
	await page.goto("/monitors");
	await expect(page.getByRole("heading", { name: "Monitors" })).toBeVisible();
	await expect(page.getByText("2 checks")).toBeVisible();
	await expect(page.getByRole("link", { name: DOMAIN, exact: true })).toBeVisible();
});

test("the create form offers URL checks only — no Heartbeat anywhere (Decision 20)", async ({
	page,
}) => {
	await page.goto("/monitors");
	await page.getByRole("button", { name: "New monitor" }).click();

	await expect(page.getByText("Watch a website")).toBeVisible();
	await expect(page.getByText("Every minute is the fastest we run a check.")).toBeVisible();
	// The word may not appear as an option, a hint, or anything else.
	await expect(page.getByText("Heartbeat")).toHaveCount(0);
});

test("creating a check round-trips through the API and the re-read", async ({ page }) => {
	await page.goto("/monitors");
	await page.getByRole("button", { name: "New monitor" }).click();
	await page.getByPlaceholder("https://example.com").fill("https://new.example.com");
	await page.getByRole("button", { name: "Create monitor" }).click();

	await expect(page.getByRole("link", { name: "new.example.com" })).toBeVisible();
	await expect(page.getByText("3 checks")).toBeVisible();
});

// The /public/check answer the onboarding consumes: one live-probe row plus
// two sitemap pages, two of the three recommended — mirrors the web
// version's discovery shape exactly.
const CHECK = {
	groups: [
		{
			title: "Live probe",
			source: "from a real request",
			rows: [
				{ id: "https://example.com", name: "example.com", meta: "231 ms · HTTP 200", status: "ok", recommended: true },
			],
		},
		{
			title: "Pages",
			source: "from the sitemap",
			rows: [
				{ id: "https://example.com/pricing", name: "/pricing", meta: "180 ms · HTTP 200", status: "ok", recommended: true },
				{ id: "https://example.com/blog", name: "/blog", meta: "210 ms · HTTP 200", status: "ok", recommended: false },
			],
		},
	],
	networkChecks: [],
	probe: { ok: true, status_code: 200, error_class: "", total_ms: 231 },
	watchLimit: 1000,
};

test("an empty account opens on the scan onboarding, and Start watching creates the ticked checks", async ({
	page,
}) => {
	// A stateful list of this test's own, starting EMPTY — a later route
	// registration outranks the fixture's seeded one.
	const created: Array<Record<string, unknown>> = [];
	await page.route("**/v1/monitors", (route) => {
		if (route.request().method() === "POST") {
			const body = route.request().postDataJSON() as Record<string, unknown>;
			created.push(body);
			return route.fulfill({
				contentType: "application/json",
				body: JSON.stringify({ id: `mon_ob_${created.length}`, status: "nodata", ...body }),
			});
		}
		return route.fulfill({
			contentType: "application/json",
			body: JSON.stringify(created.map((body, i) => ({ id: `mon_ob_${i + 1}`, status: "nodata", ...body }))),
		});
	});
	let checkedHost = "";
	await page.route("**/public/check", (route) => {
		checkedHost = (route.request().postDataJSON() as { host: string }).host;
		return route.fulfill({ contentType: "application/json", body: JSON.stringify(CHECK) });
	});

	await page.goto("/monitors");
	await expect(page.getByRole("heading", { name: "Watch your first website" })).toBeVisible();

	// Any spelling works: the scheme and trailing slash never reach the scan.
	await page.getByLabel("Website address").fill("https://example.com/");
	await page.getByRole("button", { name: "Scan" }).click();

	await expect(page.getByText("Live probe")).toBeVisible();
	await expect(page.getByText("from the sitemap")).toBeVisible();
	expect(checkedHost).toBe("example.com");

	// The two recommended rows arrive pre-ticked; the button counts them, and
	// nothing was created before this click.
	await page.getByRole("button", { name: "Start watching 2 checks" }).click();

	// The screen flips to the table in place — the created checks ARE the list.
	await expect(page.getByText("2 checks")).toBeVisible();
	expect(created.map((body) => body.target)).toEqual(["https://example.com", "https://example.com/pricing"]);
});

test("the manual form accepts a bare domain — upcontrol.io and https://upcontrol.io/ are one address", async ({
	page,
}) => {
	// Observe, don't intercept: the fixture's stateful list keeps owning the
	// round trip, this only reads what actually travelled.
	let posted: Record<string, unknown> | null = null;
	page.on("request", (request) => {
		if (request.url().includes("/v1/monitors") && request.method() === "POST") {
			posted = request.postDataJSON() as Record<string, unknown>;
		}
	});

	await page.goto("/monitors");
	await page.getByRole("button", { name: "New monitor" }).click();
	await page.getByPlaceholder("https://example.com").fill("upcontrol.io/");
	await page.getByRole("button", { name: "Create monitor" }).click();

	await expect(page.getByRole("link", { name: "upcontrol.io" })).toBeVisible();
	expect(posted).not.toBeNull();
	expect((posted as unknown as { target: string }).target).toBe("https://upcontrol.io");
});

test("the header wears the animated brand lockup", async ({ page }) => {
	await page.goto("/monitors");
	await expect(page.getByRole("link", { name: "UpControl, home" })).toBeVisible();
});

test("delete asks first with the light inline confirm", async ({ page }) => {
	await page.goto("/monitors");
	await page.getByRole("button", { name: `Delete ${DOMAIN}`, exact: true }).click();
	await expect(page.getByText("Stop watching this, and delete its history?")).toBeVisible();
	await page.getByRole("button", { name: "Delete", exact: true }).click();
	await expect(page.getByText("1 checks")).toBeVisible();
});
