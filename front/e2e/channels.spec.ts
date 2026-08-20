import { test, expect } from "@playwright/test";
import { EMAIL, stubApi } from "./fixtures/api";

test.beforeEach(async ({ page }) => {
	await stubApi(page);
});

test("a channel is a destination: one row, one test, one delete", async ({ page }) => {
	await page.goto("/channels");
	await expect(page.getByRole("heading", { name: "Channels" })).toBeVisible();
	await expect(page.getByText("A channel is a destination. Every alert goes to all of them.")).toBeVisible();
	await expect(page.getByText(EMAIL)).toBeVisible();
});

test("Send test reports the queue's own outcome, never claiming sent", async ({ page }) => {
	await page.goto("/channels");
	await page.getByRole("button", { name: "Send test" }).click();

	// Queued first — the outcome is not known yet.
	await expect(page.getByText("queued, waiting for the outcome")).toBeVisible();
	// Then the queue's verdict, in the server's words: this stack has no mailer.
	await expect(page.getByText("no sender for kind email")).toBeVisible({ timeout: 10_000 });
});

test("adding a destination is one field, and telegram is a deep link", async ({ page }) => {
	await page.goto("/channels");

	await page.getByRole("button", { name: /^Email/ }).click();
	await page.getByLabel("Email address").fill("ops@example.com");
	await page.getByRole("button", { name: "Add", exact: true }).click();
	await expect(page.getByText("ops@example.com")).toBeVisible();

	// A chat id cannot be typed: the telegram tile offers the bot's own door.
	await page.getByRole("button", { name: /^Telegram/ }).click();
	await expect(page.getByRole("link", { name: "Open Telegram" })).toHaveAttribute(
		"href",
		/t\.me\/upcontrol_test_bot/,
	);
});

test("delete asks first, inline", async ({ page }) => {
	await page.goto("/channels");
	await page.getByRole("button", { name: `Remove ${EMAIL}` }).click();
	await expect(page.getByText("Remove this destination?")).toBeVisible();
	await page.getByRole("button", { name: "Delete", exact: true }).click();
	await expect(page.getByText(EMAIL)).toHaveCount(0);
});
