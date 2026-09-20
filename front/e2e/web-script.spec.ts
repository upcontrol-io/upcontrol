import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { expect, test, type Page } from "@playwright/test";

// uc.js against a routed-only world: no app server, no backend. Every URL is
// intercepted; the script is the real file the API serves. Shapes:
// ../back/api/openapi.yaml (WebBeacon, Heatmap, HeatCell).

const SCRIPT = readFileSync(
	fileURLToPath(new URL("../../back/internal/web/uc.js", import.meta.url)),
	"utf8",
);

type ClickCell = [string, number, number, number, number]; // [sel, fx, fy, clicks, rage]
type MoveCell = [string, number, number, number]; // [sel, fx, fy, samples]
type Beacon = {
	v: number;
	t: "view" | "heat";
	p: string;
	w: number;
	r?: string;
	q?: string;
	s?: number;
	c?: ClickCell[];
	m?: MoveCell[];
};

const site = (body: string) =>
	`<!doctype html><html><head>` +
	`<script defer src="https://api.test/uc.js" data-key="uc_pub_test"></script>` +
	`</head><body>${body}</body></html>`;

// Serve the script, capture every /w beacon, and host the given page body on
// https://site.test/. Returns the beacon list the tests poll.
async function serve(page: Page, body: string) {
	const beacons: Beacon[] = [];
	await page.route("https://api.test/uc.js", (route) =>
		route.fulfill({ contentType: "text/javascript", body: SCRIPT }));
	await page.route(/api\.test\/w\?/, (route) => {
		beacons.push(JSON.parse(route.request().postData() ?? "{}") as Beacon);
		return route.fulfill({ status: 204 });
	});
	await page.route("https://site.test/**", (route) =>
		route.fulfill({ contentType: "text/html", body: site(body) }));
	return beacons;
}

// A synthetic tab hide: the one moment uc.js flushes at.
const hide = (page: Page) =>
	page.evaluate(() => {
		Object.defineProperty(document, "visibilityState", {
			value: "hidden",
			configurable: true,
		});
		document.dispatchEvent(new Event("visibilitychange"));
	});

const watch = (page: Page) => {
	const errors: string[] = [];
	page.on("pageerror", (e) => errors.push(String(e)));
	return errors;
};

test("a page view lands on load", async ({ page }) => {
	const errors = watch(page);
	const beacons = await serve(page, "<h1>Hello</h1>");
	await page.goto("https://site.test/");
	await expect.poll(() => beacons.filter((b) => b.t === "view")).toHaveLength(1);
	const view = beacons[0];
	expect(view.p).toBe("/");
	expect(view.w).toBe(await page.evaluate(() => innerWidth));
	expect(errors).toEqual([]);
});

test("a click is addressed to its element without classes", async ({ page }) => {
	const errors = watch(page);
	const beacons = await serve(
		page,
		'<main><section>…</section><section><div><a href="#x">A</a><a href="#y">B</a></div></section></main>',
	);
	await page.goto("https://site.test/");
	await page.getByRole("link", { name: "B" }).click();
	await hide(page);
	await expect.poll(() => beacons.filter((b) => b.t === "heat" && b.c?.length)).toHaveLength(1);
	const c = beacons.find((b) => b.t === "heat")!.c![0];
	expect(c[3]).toBe(1); // clicks
	expect(c[4]).toBe(0); // rage
	expect(c[1]).toBeGreaterThanOrEqual(0);
	expect(c[1]).toBeLessThanOrEqual(63);
	expect(c[2]).toBeGreaterThanOrEqual(0);
	expect(c[2]).toBeLessThanOrEqual(63);
	// The selector must resolve back to the very element that was clicked.
	const text = await page.evaluate((s) => document.querySelector(s)?.textContent, c[0]);
	expect(text).toBe("B");
	expect(errors).toEqual([]);
});

test("three fast clicks on one spot count as rage", async ({ page }) => {
	const errors = watch(page);
	const beacons = await serve(page, '<button id="go">Go</button>');
	await page.goto("https://site.test/");
	for (let i = 0; i < 3; i++) {
		await page.getByRole("button", { name: "Go" }).click();
		await page.waitForTimeout(60);
	}
	await hide(page);
	await expect.poll(() => beacons.filter((b) => b.t === "heat" && b.c?.length)).toHaveLength(1);
	const c = beacons.find((b) => b.t === "heat")!.c![0];
	expect(c[3]).toBe(3); // clicks
	expect(c[4]).toBe(3); // rage: every click of the group, once
	expect(errors).toEqual([]);
});

test("mouse moves sample into cells on a fine pointer", async ({ page }) => {
	test.skip(test.info().project.name === "mobile", "the phone project has no fine pointer");
	const errors = watch(page);
	const beacons = await serve(
		page,
		'<div id="pad" style="width:400px;height:300px">pad</div>',
	);
	await page.goto("https://site.test/");
	const box = (await page.locator("#pad").boundingBox())!;
	for (let i = 0; i < 5; i++) {
		await page.mouse.move(box.x + 50 + i * 40, box.y + 100);
		await page.waitForTimeout(120);
	}
	await hide(page);
	const samples = () =>
		beacons
			.filter((b) => b.t === "heat")
			.reduce((sum, b) => sum + (b.m ?? []).reduce((a, e) => a + e[3], 0), 0);
	await expect.poll(samples).toBeGreaterThanOrEqual(3);
	expect(errors).toEqual([]);
});

test("no move cells without a fine pointer", async ({ page }) => {
	test.skip(test.info().project.name === "chromium", "the desktop project has a fine pointer");
	const errors = watch(page);
	const beacons = await serve(
		page,
		'<div id="pad" style="width:400px;height:300px">pad</div>',
	);
	await page.goto("https://site.test/");
	const box = (await page.locator("#pad").boundingBox())!;
	for (let i = 0; i < 5; i++) {
		await page.mouse.move(box.x + 50 + i * 40, box.y + 100);
		await page.waitForTimeout(120);
	}
	await hide(page);
	await expect.poll(() => beacons.filter((b) => b.t === "heat")).toHaveLength(1);
	expect(beacons.filter((b) => b.m?.length)).toEqual([]);
	expect(errors).toEqual([]);
});

test("scroll depth reports once per page view", async ({ page }) => {
	const errors = watch(page);
	const beacons = await serve(page, '<div style="height:3000px">tall</div>');
	await page.goto("https://site.test/");
	await page.evaluate(() => scrollTo(0, 2000));
	await page.evaluate(() => new Promise((r) => requestAnimationFrame(r)));
	await hide(page);
	await expect
		.poll(() => beacons.filter((b) => b.t === "heat" && b.s !== undefined && b.s > 0.5))
		.toHaveLength(1);
	await hide(page);
	await page.waitForTimeout(100);
	expect(beacons.filter((b) => b.s !== undefined)).toHaveLength(1);
	expect(errors).toEqual([]);
});

test("scroll depth measures the page a client renders, not the one before it", async ({ page }) => {
	const errors = watch(page);
	const beacons = await serve(page, '<div id="app"></div>');
	await page.goto("https://site.test/");
	await expect.poll(() => beacons.filter((b) => b.t === "view")).toHaveLength(1);
	// The app renders after the script ran; the reader leaves "/" at the top.
	await page.evaluate(() => {
		document.getElementById("app")!.style.height = "6000px";
	});
	await page.evaluate(() => history.pushState({}, "", "/a"));
	// "/a" is read to the bottom, then a router opens "/b" and scrolls to its top.
	await page.evaluate(() => scrollTo(0, document.documentElement.scrollHeight));
	await page.evaluate(() => {
		history.pushState({}, "", "/b");
		scrollTo(0, 0);
	});
	await hide(page);
	const depth = (p: string) =>
		beacons.find((b) => b.t === "heat" && b.p === p && b.s !== undefined)?.s;
	await expect.poll(() => ["/", "/a", "/b"].every((p) => depth(p) !== undefined)).toBe(true);
	expect(depth("/")).toBeLessThan(0.5);
	expect(depth("/a")).toBe(1);
	expect(depth("/b")).toBeLessThan(0.5);
	expect(errors).toEqual([]);
});

test("an SPA path change flushes the old page and opens the new one", async ({ page }) => {
	const errors = watch(page);
	const beacons = await serve(page, '<button id="next">Next</button>');
	await page.goto("https://site.test/");
	await page.getByRole("button", { name: "Next" }).click();
	await page.evaluate(() => history.pushState({}, "", "/next"));
	await expect.poll(() => beacons.filter((b) => b.t === "view" && b.p === "/next")).toHaveLength(1);
	const heat = beacons.find((b) => b.t === "heat")!;
	expect(heat.p).toBe("/"); // flushed under the path it happened on
	expect(heat.c?.length).toBe(1);
	const iHeat = beacons.indexOf(heat);
	const iView = beacons.findIndex((b) => b.t === "view" && b.p === "/next");
	expect(iHeat).toBeGreaterThanOrEqual(0);
	expect(iHeat).toBeLessThan(iView);
	expect(beacons[iView].r).toBe(""); // the referrer is the first view's business
	expect(errors).toEqual([]);
});

test("a heatmap link draws the overlay and collects nothing", async ({ page }) => {
	const errors = watch(page);
	const beacons = await serve(page, '<button id="buy">Buy</button>');
	const heatUrls: string[] = [];
	await page.route(/api\.test\/w\/heatmap/, (route) => {
		heatUrls.push(route.request().url());
		return route.fulfill({
			contentType: "application/json",
			headers: { "Access-Control-Allow-Origin": "https://site.test" },
			body: JSON.stringify({
				path: "/",
				device: "desktop",
				range: "7d",
				views: 1234,
				clicks: [{ selector: "#buy", x: 32, y: 32, n: 10 }],
				rage: [],
				moves: [],
				scroll: Array.from({ length: 21 }, (_, i) => 1000 - i * 40),
			}),
		});
	});
	await page.goto("https://site.test/#uc-heatmap=uch_test");
	await expect.poll(() => heatUrls).toHaveLength(1);
	expect(heatUrls[0]).toContain("token=uch_test");
	expect(heatUrls[0]).toContain("path=%2F");
	expect(heatUrls[0]).toContain("key=uc_pub_test");
	await expect(page.getByText("1,234 desktop views · 7 days")).toBeVisible();
	await expect(page.locator("canvas")).toHaveCount(1);
	expect(await page.evaluate(() => location.hash)).not.toContain("uc-heatmap");
	await page.getByRole("button", { name: "Scroll" }).click();
	await page.getByRole("button", { name: "Moves" }).click();
	// The panel is dragged by its header, and the map underneath is what it uncovers.
	const bar = page.locator("[data-uc-heatmap] .bar");
	const before = (await bar.boundingBox())!;
	await page.mouse.move(before.x + 40, before.y + 10);
	await page.mouse.down();
	await page.mouse.move(before.x - 260, before.y + 180, { steps: 8 });
	await page.mouse.up();
	const after = (await bar.boundingBox())!;
	expect(Math.round(after.x)).toBe(Math.round(before.x) - 300);
	expect(Math.round(after.y)).toBe(Math.round(before.y) + 170);
	// The cross is the only way out, and it takes the canvas with it.
	await page.getByRole("button", { name: "Close" }).click();
	await expect(page.locator("canvas")).toHaveCount(0);
	await page.waitForTimeout(100);
	expect(beacons).toEqual([]);
	expect(errors).toEqual([]);
});

test("a fragment that is not a heatmap token changes nothing", async ({ page }) => {
	const errors = watch(page);
	const beacons = await serve(page, '<button id="buy">Buy</button>');
	await page.goto("https://site.test/#uc-heatmap=x");
	await expect.poll(() => beacons.filter((b) => b.t === "view")).toHaveLength(1);
	await expect(page.locator("canvas")).toHaveCount(0);
	expect(await page.evaluate(() => location.hash)).toBe("#uc-heatmap=x");
	expect(errors).toEqual([]);
});

test("an expired heatmap link says so", async ({ page }) => {
	const errors = watch(page);
	const beacons = await serve(page, '<button id="buy">Buy</button>');
	await page.route(/api\.test\/w\/heatmap/, (route) =>
		route.fulfill({
			status: 401,
			contentType: "application/json",
			headers: { "Access-Control-Allow-Origin": "https://site.test" },
			body: JSON.stringify({ error: { code: "bad_token", message: "x" } }),
		}));
	await page.goto("https://site.test/#uc-heatmap=uch_dead");
	await expect(page.getByText(/expired/)).toBeVisible();
	expect(beacons).toEqual([]);
	expect(errors).toEqual([]);
});
