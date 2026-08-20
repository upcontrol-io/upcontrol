import { test, expect } from "@playwright/test";
import { stubApi } from "./fixtures/api";

test.beforeEach(async ({ page }) => {
	await stubApi(page);
});

test("an empty window says so and points at the installer, never inventing lines", async ({
	page,
}) => {
	await page.goto("/logs");
	await expect(page.getByRole("heading", { name: "Logs" })).toBeVisible();
	await expect(page.getByText("No log lines yet")).toBeVisible();
	// The door to the screen that carries the install command.
	await expect(page.getByRole("link", { name: "Add it to your code" })).toBeVisible();
});

test("lines render as the stream, with the window's own count", async ({ page }) => {
	await page.route("**/v1/logs", (route) =>
		route.fulfill({
			status: 200,
			contentType: "application/json",
			body: JSON.stringify({
				lines: [
					{
						seq: "1",
						ts: "2026-08-20T09:31:30Z",
						level: "error",
						service: "api",
						message: "GET /users/:id 500 1640 ms",
					},
				],
				volume: [{ minute: "2026-08-20T09:31:00Z", level: "error", lines: 1 }],
				total: 1,
				services: [{ name: "api", lines: 1 }],
			}),
		}),
	);
	await page.goto("/logs");
	await expect(page.getByText("GET /users/:id 500 1640 ms")).toBeVisible();
});
