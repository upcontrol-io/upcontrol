import { test, expect } from "@playwright/test";
import { serveIncidents, stubApi } from "./fixtures/api";

test.beforeEach(async ({ page }) => {
	await stubApi(page);
});

test("live and empty is its own fact", async ({ page }) => {
	await page.goto("/incidents");
	await expect(page.getByRole("heading", { name: "Incidents" })).toBeVisible();
	await expect(page.getByText("No incidents recorded.")).toBeVisible();
});

test("the list is the latest 20, newest first, no pagination controls", async ({ page }) => {
	await serveIncidents(page, [
		{
			id: "inc_1",
			title: "Error rate spike on example.com",
			status: "down",
			since: "09:31",
			durationMinutes: 4,
			ongoing: true,
			timeline: [],
			logSlice: [],
			affectedCount: 0,
		},
	]);
	await page.goto("/incidents");

	await expect(page.getByText("Error rate spike on example.com")).toBeVisible();
	await expect(page.getByText("since 09:31 · 4 minutes · ongoing")).toBeVisible();
	await expect(page.getByText("The latest 20 incidents.")).toBeVisible();
	// The contract has no pagination parameters, so no pager may render.
	await expect(page.getByRole("button", { name: /next|prev|page/i })).toHaveCount(0);
});
