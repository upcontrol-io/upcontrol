import { defineConfig, devices } from "@playwright/test";

// E2E runner. The dev server is reused when one is already running on 5199;
// otherwise Playwright starts it and stops it after the run. 5199 on purpose:
// other Vite servers on this machine grab 5173 (and vite bumps collisions to
// 5174+), and reusing the wrong dev server tests the wrong product —
// `--strictPort` makes that a loud failure, never a fallback.
export default defineConfig({
	testDir: "./e2e",
	fullyParallel: true,
	forbidOnly: !!process.env.CI,
	retries: process.env.CI ? 2 : 0,
	reporter: [["list"], ["html", { open: "never" }]],
	use: {
		baseURL: "http://localhost:5199",
		trace: "retain-on-failure",
		screenshot: "only-on-failure",
	},
	projects: [
		{
			name: "chromium",
			use: { ...devices["Desktop Chrome"] },
		},
		// The phone tier, and it earns its place: this app ships a bottom tab
		// bar, a More sheet and a picker that becomes a bottom sheet below
		// 700px, and none of that existed for the runner until now. The
		// commercial front's own mobile project caught a picker that rendered
		// off the left edge and could not be tapped at all (2026-08-20); this
		// app carried the identical component and had nothing to catch it with.
		// iPhone 12 metrics on Chromium: WebKit is not in the ms-playwright
		// cache here, and the breakpoints do not depend on the engine.
		{
			name: "mobile",
			use: { ...devices["iPhone 12"], browserName: "chromium" },
		},
	],
	webServer: {
		command: "npm run dev -- --port 5199 --strictPort",
		url: "http://localhost:5199",
		reuseExistingServer: !process.env.CI,
	},
});
