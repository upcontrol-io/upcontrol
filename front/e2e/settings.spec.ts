import { test, expect } from "@playwright/test";
import { stubApi } from "./fixtures/api";

test.beforeEach(async ({ page }) => {
	await stubApi(page);
});

test("the key is a prefix, and the command carries a token — never the key", async ({ page }) => {
	await page.goto("/settings");

	await expect(page.getByRole("heading", { name: "Settings" })).toBeVisible();
	await expect(page.getByText("uc_live_a1b2c3d4e5f6")).toBeVisible();
	await expect(page.getByText("the full key is shown once, when you rotate it", { exact: false })).toBeVisible();

	// Created by an explicit click, never on render.
	await page.getByRole("button", { name: "Generate install command" }).click();
	await expect(page.getByText(/npx upcontrol init --token tok_fixture/)).toBeVisible();
});

test("the Telegram bot block saves a token and username and reports the bot live", async ({ page }) => {
	// Start with no telegram surface: the server offers it only with a bot.
	let botSaved = false;
	await page.route("**/v1/channels", (route) => {
		if (route.request().method() !== "GET") return route.fallback();
		return route.fulfill({
			contentType: "application/json",
			body: JSON.stringify({
				channels: [],
				connectableChannels: botSaved
					? [{ kind: "telegram", name: "Telegram", field: "Chat", hint: "" }]
					: [{ kind: "email", name: "Email", field: "Email address", hint: "" }],
			}),
		});
	});
	let saved: { token: string; username: string } | null = null;
	await page.route("**/v1/instance/telegram-bot", (route) => {
		saved = route.request().postDataJSON() as { token: string; username: string };
		botSaved = true;
		return route.fulfill({ status: 204, body: "" });
	});

	await page.goto("/settings");
	await expect(page.getByText("No bot yet.", { exact: false })).toBeVisible();

	await page.getByLabel("Telegram bot token").fill("123456789:AAtesttoken_testtoken_testtoken");
	await page.getByLabel("Telegram bot username").fill("my_alerts_bot");
	await page.getByRole("button", { name: "Save bot" }).click();

	await expect(page.getByText("Saved. Alerts and invites work now", { exact: false })).toBeVisible();
	await expect(page.getByText("A bot is connected", { exact: false })).toBeVisible();
	expect(saved).toEqual({ token: "123456789:AAtesttoken_testtoken_testtoken", username: "my_alerts_bot" });
});
