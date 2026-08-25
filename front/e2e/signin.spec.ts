import { test, expect } from "@playwright/test";
import { DEV_TOKEN, EMAIL, stubApi, stubSignedOut } from "./fixtures/api";

// Two auth modes: signed out, the two-step form signs you in; signed in
// (UC_AUTH=none), /signin just forwards into the app.

test("the two-step form signs you in: email, then the code", async ({ page }) => {
	await stubSignedOut(page);
	await page.goto("/signin");

	await expect(page.getByRole("heading", { name: "Sign in to UpControl" })).toBeVisible();
	await expect(page.getByText("No password. A one-time code is issued for your email.")).toBeVisible();

	await page.getByLabel("Email").fill(EMAIL);
	await page.getByRole("button", { name: "Send code" }).click();

	// Step two: the code field, prefilled from the dev token the request
	// answered with, and the honest sentence about where a code lands.
	await expect(page.getByLabel("Code")).toHaveValue(DEV_TOKEN);
	await expect(page.getByText("without it, it is in the API log")).toBeVisible();

	await page.getByRole("button", { name: "Sign in" }).click();

	// The redeem flipped the fixture to authenticated: the app opens.
	await expect(page).toHaveURL(/\/monitors$/);
	await expect(page.getByRole("heading", { name: "Monitors" })).toBeVisible();
});

test("with /v1/me answering, /signin forwards straight into the app (UC_AUTH=none)", async ({
	page,
}) => {
	await stubApi(page);
	await page.goto("/signin");
	await expect(page).toHaveURL(/\/monitors$/);
});
