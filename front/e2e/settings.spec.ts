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

test("the AI row names the wired brain honestly", async ({ page }) => {
	await page.goto("/settings");
	await expect(
		page.getByText("Explain is answered by openai:https://api.openai.com/v1:gpt-4o-mini", { exact: false }),
	).toBeVisible();
	// The key field is always there — replacing a key is one paste — and the
	// comment pins the format promise.
	await expect(page.getByLabel("OpenAI-format API key")).toBeVisible();
	await expect(page.getByLabel("Chat model")).toBeVisible();
	await expect(page.getByLabel("API base URL")).toBeVisible();
	await expect(page.getByText("any OpenAI-compatible endpoint", { exact: false })).toBeVisible();
});

test("no key = Explain is off, and saving key+model+URL turns it on without a reload", async ({ page }) => {
	// Explain-off: the preview answers model: null until settings are saved.
	let configured = false;
	await page.route("**/v1/logs/explain/preview", (route) =>
		route.fulfill({
			contentType: "application/json",
			body: JSON.stringify({
				system: "",
				user: "",
				model: configured ? "openai:https://gateway.local/v1:my-model" : null,
				temperature: 0,
				max_output_tokens: 0,
			}),
		}),
	);
	let saved: Record<string, string> | null = null;
	await page.route("**/v1/instance/ai", (route) => {
		saved = route.request().postDataJSON() as Record<string, string>;
		configured = true;
		return route.fulfill({ status: 204, body: "" });
	});

	await page.goto("/settings");
	await expect(page.getByText("Explain is off — no API key is configured.")).toBeVisible();
	// The format promise is stated where the fields are.
	await expect(page.getByText("any OpenAI-compatible endpoint", { exact: false })).toBeVisible();

	await page.getByLabel("OpenAI-format API key").fill("sk-test-abcdef123456");
	await page.getByLabel("Chat model").fill("my-model");
	await page.getByLabel("API base URL").fill("https://gateway.local/v1");
	await page.getByRole("button", { name: "Save AI settings" }).click();

	await expect(page.getByText("Saved. Explain answers with these settings", { exact: false })).toBeVisible();
	await expect(
		page.getByText("Explain is answered by openai:https://gateway.local/v1:my-model", { exact: false }),
	).toBeVisible();
	// The inputs empty — no value lingers on screen after the save.
	await expect(page.getByLabel("OpenAI-format API key")).toHaveValue("");
	await expect(page.getByLabel("Chat model")).toHaveValue("");
	expect(saved).toEqual({ key: "sk-test-abcdef123456", model: "my-model", baseUrl: "https://gateway.local/v1" });
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
