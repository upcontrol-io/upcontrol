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
			bars: ["ok", "ok", "ok", "ok", "ok", "ok", "ok"],
			barSpanSec: 86400,
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
