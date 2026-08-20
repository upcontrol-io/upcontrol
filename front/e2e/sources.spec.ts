import { test, expect } from "@playwright/test";
import { stubApi } from "./fixtures/api";

test.beforeEach(async ({ page }) => {
	await stubApi(page);
});

test("connections render as cards, tiles come from the server", async ({ page }) => {
	await page.goto("/sources");
	await expect(page.getByRole("heading", { name: "Sources" })).toBeVisible();
	await expect(page.getByText("Site checks")).toBeVisible();
	await expect(page.getByText("App logs")).toBeVisible();
	await expect(page.getByText("Add a source")).toBeVisible();
});

test("looking creates nothing: the tile opens the URL panel, the receipt waits", async ({
	page,
}) => {
	await page.goto("/sources");
	await page.getByRole("button", { name: /Deploy hooks/ }).click();

	// The server-issued hook address, and the honest receipt under it.
	await expect(page.getByText(/\/hooks\/htok123/)).toBeVisible();
	await expect(page.getByText("Waiting for the first event…")).toBeVisible();
	// No new card appeared from browsing — copying is what creates it.
	await expect(page.getByText("Deploy hooks", { exact: true })).toHaveCount(1);
});

test("the installer tile is the door to Settings", async ({ page }) => {
	await page.goto("/sources");
	await page.getByRole("link", { name: /Add it to your code/ }).click();
	await expect(page).toHaveURL(/\/settings$/);
});
