import { test, expect, type Page } from "@playwright/test";
import { serveIncident, stubApi } from "./fixtures/api";

// Explain's three promises (no paid wall in this app, Decision 8): the card
// shows the
// MODEL'S read whole and nothing derived, the severity badge is the model's
// word and never the page's, and a read that never answers prints nothing
// rather than a sentence this page invented.

const ONGOING_INCIDENT = {
	id: "inc_1",
	title: "Error rate spike on example.com",
	status: "down",
	since: "09:31",
	durationMinutes: 4,
	ongoing: true,
	timeline: [
		{
			time: "09:31",
			ago: "4 min ago",
			kind: "error",
			text: "error rate 22.9% (z=12.6)",
		},
	],
	logSlice: ["09:31:30  GET /users/:id 500 1640 ms"],
	affectedCount: 0,
};

const EXPLAINED = {
	problem: "the checkout route answered 500 for 4 minutes",
	cause: "the upstream timed out",
	confidence: "high",
	fix: "raise the upstream timeout to 30s and redeploy",
	investigate: [{ step: "Check the upstream's own status page.", command: null }],
	cached: false,
	used: 1,
	limit: 0,
	prompt: "",
	severity: "critical",
	area: "API",
};

// The phrases the card is forbidden to ever print: derived verdicts and
// derived facts. A regression here is the whole reason this list is explicit.
const BANNED = [
	"This can wait until morning",
	"Worth getting up for",
	"Already over",
	"Nobody hit it",
	"No deploy went out near the start",
	"It came back on its own",
	"It has not recovered on its own",
	"Best guess",
];

async function serveExplain(page: Page, status: number, body: unknown) {
	await page.route("**/v1/incidents/*/explain", (route) =>
		route.fulfill({
			status,
			contentType: "application/json",
			body: JSON.stringify(body),
		}),
	);
}

test.beforeEach(async ({ page }) => {
	await stubApi(page);
	await serveIncident(page, ONGOING_INCIDENT);
});

test("the answer is the model's, whole, and carries none of the old prose", async ({ page }) => {
	await serveExplain(page, 200, EXPLAINED);
	await page.goto("/incidents/inc_1");

	await page.getByRole("button", { name: "Explain" }).click();

	await expect(page.getByText(EXPLAINED.problem)).toBeVisible();
	await expect(page.getByText(EXPLAINED.cause)).toBeVisible();
	await expect(page.getByText("high confidence")).toBeVisible();
	await expect(page.getByText(EXPLAINED.fix)).toBeVisible();
	await expect(page.getByText("Check the upstream's own status page.")).toBeVisible();
	// The model's grade, as the badge.
	await expect(page.getByText("Critical · API")).toBeVisible();

	for (const phrase of BANNED) {
		await expect(page.getByText(phrase)).toHaveCount(0);
	}
});

test("while Explain reads, the brand mark's corner-chase shows — same as the web version", async ({ page }) => {
	// A slow answer: the reading state must be the chase, never blank space
	// or a generic spinner.
	await page.route("**/v1/incidents/*/explain", async (route) => {
		await new Promise((resolve) => setTimeout(resolve, 1200));
		await route.fulfill({ contentType: "application/json", body: JSON.stringify(EXPLAINED) });
	});
	await page.goto("/incidents/inc_1");

	await page.getByRole("button", { name: "Explain" }).click();

	await expect(page.getByRole("status", { name: "Reading the incident" })).toBeVisible();
	await expect(page.getByText(EXPLAINED.cause)).toBeVisible();
	await expect(page.getByRole("status", { name: "Reading the incident" })).toHaveCount(0);
});

test("no severity, no badge", async ({ page }) => {
	await serveExplain(page, 200, { ...EXPLAINED, severity: null, area: null });
	await page.goto("/incidents/inc_1");

	await page.getByRole("button", { name: "Explain" }).click();

	await expect(page.getByText(EXPLAINED.cause)).toBeVisible();
	await expect(page.getByText(/^(Critical|Major|Minor)( · .+)?$/)).toHaveCount(0);
});

test("a read that never answers prints nothing rather than filler", async ({ page }) => {
	await page.route("**/v1/incidents/*/explain", (route) => route.abort());
	await page.goto("/incidents/inc_1");

	await page.getByRole("button", { name: "Explain" }).click();

	// The button returns to rest — the read is over, and nothing was invented.
	await expect(page.getByRole("button", { name: "Explain" })).toBeVisible();
	for (const phrase of BANNED) {
		await expect(page.getByText(phrase)).toHaveCount(0);
	}
});

test("a throttle's own words render as the note (there is no paid surface here)", async ({
	page,
}) => {
	await serveExplain(page, 429, {
		error: { message: "Too many Explain requests. Try again in a minute." },
	});
	await page.goto("/incidents/inc_1");

	await page.getByRole("button", { name: "Explain" }).click();

	// The server's sentence, verbatim — never a dialog, never invented prose.
	await expect(page.getByText("Too many Explain requests. Try again in a minute.")).toBeVisible();
	await expect(page.getByRole("dialog")).toHaveCount(0);
});
