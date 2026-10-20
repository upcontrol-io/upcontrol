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
const OVERLAY = readFileSync(
	fileURLToPath(new URL("../../back/internal/web/overlay.js", import.meta.url)),
	"utf8",
);

type ClickCell = [string, number, number, number, number]; // [sel, fx, fy, clicks, rage]
type MoveCell = [string, number, number, number]; // [sel, fx, fy, samples]
type Beacon = {
	v: number;
	t: "view" | "heat" | "event";
	p: string;
	w: number;
	n?: string;
	l?: Record<string, string>;
	r?: string;
	q?: string;
	f?: string;
	g?: string;
	o?: string;
	e?: number;
	a?: number[];
	h?: string[];
	d?: MoveCell[]; // [sel, fx, fy, clicks]
	s?: number;
	c?: ClickCell[];
	m?: MoveCell[];
	fm?: [string, string][]; // [kind, selector]
};

// attrs go on the script tag; head is markup before it.
type SiteOpts = { attrs?: string; head?: string };

const site = (body: string, { attrs = "", head = "" }: SiteOpts = {}) =>
	`<!doctype html><html><head>${head}` +
	`<script defer src="https://api.test/uc.js" data-key="uc_pub_test"${attrs}></script>` +
	`</head><body>${body}</body></html>`;

// Serve the script, capture every /w beacon, and host the given page body on
// https://site.test/. Returns the beacon list the tests poll.
async function serve(page: Page, body: string, opts?: SiteOpts) {
	const beacons: Beacon[] = [];
	await page.route("https://api.test/uc.js", (route) =>
		route.fulfill({ contentType: "text/javascript", body: SCRIPT }));
	await page.route("https://api.test/w/overlay.js", (route) =>
		route.fulfill({ contentType: "text/javascript", body: OVERLAY }));
	await page.route(/api\.test\/w\?/, (route) => {
		beacons.push(JSON.parse(route.request().postData() ?? "{}") as Beacon);
		return route.fulfill({ status: 204 });
	});
	await page.route("https://site.test/**", (route) =>
		route.fulfill({ contentType: "text/html", body: site(body, opts) }));
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

const heats = (beacons: Beacon[]) => beacons.filter((b) => b.t === "heat");

test("an SPA view names the path it came from", async ({ page }) => {
	const errors = watch(page);
	const beacons = await serve(page, "<h1>Hello</h1>");
	await page.goto("https://site.test/");
	await expect.poll(() => beacons.filter((b) => b.t === "view")).toHaveLength(1);
	expect(beacons[0].f).toBeUndefined(); // no same-origin referrer on a fresh load
	await page.evaluate(() => history.pushState({}, "", "/pricing"));
	await expect
		.poll(() => beacons.find((b) => b.t === "view" && b.p === "/pricing")?.f)
		.toBe("/");
	expect(errors).toEqual([]);
});

test("engaged time counts the seconds input backs, and its threshold once", async ({ page }) => {
	const errors = watch(page);
	await page.clock.install();
	const beacons = await serve(page, "<h1>Read me</h1>");
	await page.goto("https://site.test/");
	await expect.poll(() => beacons.filter((b) => b.t === "view")).toHaveLength(1);
	for (let i = 0; i < 4; i++) {
		await page.clock.runFor(3000);
		await page.keyboard.press("Shift");
	}
	await hide(page);
	await expect.poll(() => heats(beacons)).toHaveLength(1);
	const heat = heats(beacons)[0];
	expect(heat.e).toBeGreaterThanOrEqual(10000);
	expect(heat.e).toBeLessThanOrEqual(14000);
	expect(heat.a).toContain(0);
	expect(errors).toEqual([]);
});

test("an idle visible tab earns at most 30 s of engaged time", async ({ page }) => {
	const errors = watch(page);
	await page.clock.install();
	const beacons = await serve(page, "<h1>Read me</h1>");
	await page.goto("https://site.test/");
	await expect.poll(() => beacons.filter((b) => b.t === "view")).toHaveLength(1);
	await page.clock.runFor(60000);
	await hide(page);
	await expect.poll(() => heats(beacons)).toHaveLength(1);
	expect(heats(beacons)[0].e).toBeLessThanOrEqual(30000);
	expect(errors).toEqual([]);
});

test("each element a view clicks is a hit once", async ({ page }) => {
	const errors = watch(page);
	const beacons = await serve(page, '<button id="a">A</button> <button id="b">B</button>');
	await page.goto("https://site.test/");
	await page.locator("#a").click();
	await page.locator("#a").click();
	await page.locator("#b").click();
	await hide(page);
	await expect.poll(() => heats(beacons)).toHaveLength(1);
	expect([...heats(beacons)[0].h!].sort()).toEqual(["#a", "#b"]);
	await page.locator("#a").click();
	await hide(page);
	await expect.poll(() => heats(beacons)).toHaveLength(2);
	const second = heats(beacons)[1];
	expect(second.c?.length).toBe(1); // the click still counts
	expect(second.h ?? []).not.toContain("#a");
	expect(errors).toEqual([]);
});

test("a click after which nothing changes is dead, and only that one", async ({ page }) => {
	const errors = watch(page);
	const beacons = await serve(
		page,
		'<p id="plain">Plain text</p>' +
			`<div id="live" onclick="this.textContent = 'changed'">Live</div>` +
			'<button id="btn">Button</button>',
	);
	await page.goto("https://site.test/");
	for (const id of ["#plain", "#live", "#btn"]) {
		await page.locator(id).click();
		await page.waitForTimeout(700);
	}
	await hide(page);
	await expect.poll(() => heats(beacons)).toHaveLength(1);
	const d = heats(beacons)[0].d!;
	expect(d.map((cell) => cell[0])).toEqual(["#plain"]);
	expect(d[0][3]).toBe(1);
	expect(errors).toEqual([]);
});

test("hovering under the overlay names an element's numbers", async ({ page }) => {
	test.skip(test.info().project.name === "mobile", "a phone has no hover");
	const errors = watch(page);
	const beacons = await serve(page, '<button id="buy">Buy</button> <button id="other">Other</button>');
	await page.route(/api\.test\/w\/heatmap/, (route) =>
		route.fulfill({
			contentType: "application/json",
			headers: { "Access-Control-Allow-Origin": "https://site.test" },
			body: JSON.stringify({
				path: "/",
				device: "desktop",
				range: "7d",
				views: 1234,
				clicks: [{ selector: "#buy", x: 32, y: 32, n: 1028 }],
				rage: [{ selector: "#buy", x: 32, y: 32, n: 4 }],
				moves: [],
				hits: [{ selector: "#buy", x: 0, y: 0, n: 412 }],
				dead: [],
				scroll: Array.from({ length: 21 }, (_, i) => 1000 - i * 40),
			}),
		}));
	await page.goto("https://site.test/#uc-heatmap=uch_test");
	await expect(page.getByText("1,234 desktop views · 7 days")).toBeVisible();
	await page.locator("#buy").hover();
	await expect(page.getByText("412 views clicked · 33% · 1,028 clicks · 4 rage")).toBeVisible();
	await page.locator("#other").hover();
	await expect(page.getByText("No clicks · 7 days")).toBeVisible();
	const dead = page.getByRole("button", { name: "Dead" });
	await dead.click();
	await expect(dead).toHaveAttribute("aria-pressed", "true");
	await dead.click();
	await expect(dead).toHaveAttribute("aria-pressed", "false");
	await page.getByRole("button", { name: "Scroll" }).click();
	await page.locator("#buy").hover();
	await expect(page.getByText(/% of views reached here/)).toBeVisible();
	expect(beacons).toEqual([]);
	expect(errors).toEqual([]);
});

test("data-segment on the tag marks the view", async ({ page }) => {
	const errors = watch(page);
	const beacons = await serve(page, "<h1>Hello</h1>", { attrs: ' data-segment="member"' });
	await page.goto("https://site.test/");
	await expect.poll(() => beacons.filter((b) => b.t === "view")).toHaveLength(1);
	expect(beacons[0].g).toBe("member");
	expect(errors).toEqual([]);
});

test("a segment queued before the script loads marks the first view", async ({ page }) => {
	const errors = watch(page);
	const beacons = await serve(page, "<h1>Hello</h1>", {
		head:
			"<script>window.upcontrol=window.upcontrol||function(){(upcontrol.q=upcontrol.q||[]).push(arguments)};" +
			"upcontrol('nonsense', 1);upcontrol('segment','trial');</script>",
	});
	await page.goto("https://site.test/");
	await expect.poll(() => beacons.filter((b) => b.t === "view")).toHaveLength(1);
	expect(beacons[0].g).toBe("trial");
	expect(errors).toEqual([]);
});

test("a segment set mid-view applies from the next view", async ({ page }) => {
	const errors = watch(page);
	const beacons = await serve(page, '<button id="a">A</button> <button id="b">B</button>');
	await page.goto("https://site.test/");
	await expect.poll(() => beacons.filter((b) => b.t === "view")).toHaveLength(1);
	await page.locator("#a").click();
	await page.evaluate(() =>
		(window as unknown as { upcontrol: (c: string, a: string) => void }).upcontrol("segment", "member"));
	await page.locator("#b").click();
	await page.waitForTimeout(100);
	expect(heats(beacons)).toEqual([]); // no flush of its own
	await page.evaluate(() => history.pushState({}, "", "/next"));
	await expect.poll(() => beacons.filter((b) => b.t === "view" && b.p === "/next")).toHaveLength(1);
	const heat = heats(beacons);
	expect(heat).toHaveLength(1);
	expect(heat[0].p).toBe("/");
	expect(heat[0].g).toBeUndefined(); // the segment the view was sent with
	expect(heat[0].c!.map((c) => c[0]).sort()).toEqual(["#a", "#b"]);
	expect(beacons.find((b) => b.t === "view" && b.p === "/next")!.g).toBe("member");
	expect(errors).toEqual([]);
});

test("an invalid segment is ignored with one warning", async ({ page }) => {
	const errors = watch(page);
	const warnings: string[] = [];
	page.on("console", (m) => { if (m.type() === "warning") warnings.push(m.text()); });
	const beacons = await serve(page, '<button id="a">A</button>', { attrs: ' data-segment="member"' });
	await page.goto("https://site.test/");
	await expect.poll(() => beacons.filter((b) => b.t === "view")).toHaveLength(1);
	await page.evaluate(() => {
		const uc = (window as unknown as { upcontrol: (c: string, a: string) => void }).upcontrol;
		uc("segment", "User_12345");
		uc("segment", "Member");
	});
	await page.locator("#a").click();
	await hide(page);
	await expect.poll(() => heats(beacons)).toHaveLength(1);
	expect(heats(beacons)[0].g).toBe("member");
	expect(warnings).toEqual(["[upcontrol] segment must be 1-24 of a-z, 0-9, _ and -: User_12345"]);
	expect(errors).toEqual([]);
});

for (const [name, url, referer, o] of [
	["a search engine", "https://site.test/", "https://www.google.com/", "search"],
	["a UTM tag", "https://site.test/?utm_source=x", "https://www.google.com/", "campaign"],
	["no referrer", "https://site.test/", undefined, "direct"],
	["another site", "https://site.test/", "https://news.example/", "referral"],
] as const) {
	test(`the source of a load from ${name} is ${o}, on views and heat`, async ({ page }) => {
		const errors = watch(page);
		const beacons = await serve(page, '<button id="a">A</button>');
		await page.goto(url, { referer });
		await expect.poll(() => beacons.filter((b) => b.t === "view")).toHaveLength(1);
		expect(beacons[0].o).toBe(o);
		await page.locator("#a").click();
		await hide(page);
		await expect.poll(() => heats(beacons)).toHaveLength(1);
		expect(heats(beacons)[0].o).toBe(o);
		expect(errors).toEqual([]);
	});
}

async function heatmapWith(page: Page, segments: string[]) {
	const urls: string[] = [];
	await page.route(/api\.test\/w\/heatmap/, (route) => {
		urls.push(route.request().url());
		return route.fulfill({
			contentType: "application/json",
			headers: { "Access-Control-Allow-Origin": "https://site.test" },
			body: JSON.stringify({
				path: "/", device: "desktop", range: "7d", views: 1234,
				clicks: [], rage: [], moves: [], hits: [], dead: [], segments,
				scroll: Array.from({ length: 21 }, () => 0),
			}),
		});
	});
	return urls;
}

test("the overlay filters by segment and source", async ({ page }) => {
	const errors = watch(page);
	const beacons = await serve(page, '<button id="buy">Buy</button>');
	const urls = await heatmapWith(page, ["member", "anon"]);
	await page.goto("https://site.test/#uc-heatmap=uch_test");
	await expect(page.getByText("1,234 desktop views · 7 days")).toBeVisible();
	expect(urls[0]).not.toContain("segment=");
	expect(urls[0]).not.toContain("source=");
	await page.getByRole("combobox", { name: "Segment" }).selectOption("member");
	await expect(page.getByText("1,234 desktop views · 7 days · member")).toBeVisible();
	expect(urls.at(-1)).toContain("segment=member");
	await page.getByRole("combobox", { name: "Source" }).selectOption("campaign");
	await expect(page.getByText("1,234 desktop views · 7 days · member · from campaign")).toBeVisible();
	expect(urls.at(-1)).toContain("segment=member");
	expect(urls.at(-1)).toContain("source=campaign");
	// A route change keeps both filters.
	await page.evaluate(() => history.pushState({}, "", "/next"));
	await expect.poll(() => urls.at(-1)).toContain("path=%2Fnext");
	expect(urls.at(-1)).toContain("segment=member");
	expect(urls.at(-1)).toContain("source=campaign");
	expect(beacons).toEqual([]);
	expect(errors).toEqual([]);
});

test("the overlay offers no segment filter for a site that sets none", async ({ page }) => {
	const errors = watch(page);
	await serve(page, '<button id="buy">Buy</button>');
	await heatmapWith(page, []);
	await page.goto("https://site.test/#uc-heatmap=uch_test");
	await expect(page.getByText("1,234 desktop views · 7 days")).toBeVisible();
	await expect(page.getByRole("combobox", { name: "Segment" })).toHaveCount(0);
	await expect(page.getByRole("combobox", { name: "Source" })).toBeVisible();
	await page.evaluate(() =>
		(window as unknown as { upcontrol: (c: string, a: string) => void }).upcontrol("segment", "member"));
	expect(errors).toEqual([]);
});

test("a visitor never downloads the overlay; a heatmap link does, and it draws", async ({ page }) => {
	const errors = watch(page);
	const overlayLoads: string[] = [];
	page.on("request", (r) => { if (r.url().endsWith("/w/overlay.js")) overlayLoads.push(r.url()); });
	const beacons = await serve(page, '<button id="buy">Buy</button>');
	await heatmapWith(page, []);
	await page.goto("https://site.test/");
	await expect.poll(() => beacons.filter((b) => b.t === "view")).toHaveLength(1);
	await page.waitForTimeout(100);
	expect(overlayLoads).toEqual([]);
	// Another path: a hash-only goto would not load the page again.
	await page.goto("https://site.test/map#uc-heatmap=uch_test");
	await expect(page.getByText("1,234 desktop views · 7 days")).toBeVisible();
	await expect(page.locator("canvas")).toHaveCount(1);
	expect(overlayLoads).toEqual(["https://api.test/w/overlay.js"]);
	expect(beacons.filter((b) => b.t === "view")).toHaveLength(1);
	expect(errors).toEqual([]);
});

test("under the overlay, upcontrol('source') hands back an empty object", async ({ page }) => {
	const errors = watch(page);
	await serve(page, '<button id="buy">Buy</button>');
	await heatmapWith(page, []);
	await page.goto("https://site.test/#uc-heatmap=uch_test");
	await expect(page.getByText("1,234 desktop views · 7 days")).toBeVisible();
	expect(await page.evaluate(() =>
		(window as unknown as { upcontrol: (c: string) => unknown }).upcontrol("source"))).toEqual({});
	expect(errors).toEqual([]);
});

test("a slower, older heatmap answer never overwrites a newer one", async ({ page }) => {
	const errors = watch(page);
	await serve(page, '<button id="buy">Buy</button>');
	let slowAnswered = false;
	await page.route(/api\.test\/w\/heatmap/, async (route) => {
		const slow = route.request().url().includes("source=search");
		if (slow) await new Promise((r) => setTimeout(r, 1000));
		await route.fulfill({
			contentType: "application/json",
			headers: { "Access-Control-Allow-Origin": "https://site.test" },
			body: JSON.stringify({
				path: "/", device: "desktop", range: "7d", views: slow ? 111 : 222,
				clicks: [], rage: [], moves: [], hits: [], dead: [], segments: [],
				scroll: Array.from({ length: 21 }, () => 0),
			}),
		});
		if (slow) slowAnswered = true;
	});
	await page.goto("https://site.test/#uc-heatmap=uch_test");
	await expect(page.getByText("222 desktop views · 7 days")).toBeVisible();
	const src = page.getByRole("combobox", { name: "Source" });
	await src.selectOption("search");
	await src.selectOption("social");
	await expect(page.getByText("222 desktop views · 7 days · from social")).toBeVisible();
	await expect.poll(() => slowAnswered).toBe(true);
	await page.waitForTimeout(200);
	await expect(page.getByText("222 desktop views · 7 days · from social")).toBeVisible();
	await expect(page.getByText(/111 desktop views/)).toHaveCount(0);
	expect(errors).toEqual([]);
});

type Uc = (cmd: string, ...args: unknown[]) => unknown;
const uc = (page: Page, cmd: string, ...args: unknown[]) =>
	page.evaluate(([c, a]) => (window as unknown as { upcontrol: Uc }).upcontrol(c as string, ...(a as unknown[])),
		[cmd, args] as const);
const events = (beacons: Beacon[]) => beacons.filter((b) => b.t === "event");
const views = (beacons: Beacon[]) => beacons.filter((b) => b.t === "view");

test("track sends a named event with its fields, path and marks", async ({ page }) => {
	const errors = watch(page);
	const beacons = await serve(page, "<h1>Hello</h1>", { attrs: ' data-segment="member"' });
	await page.goto("https://site.test/");
	await expect.poll(() => views(beacons)).toHaveLength(1);
	await uc(page, "track", "signup_click", { plan: "pro", n: 3 });
	await expect.poll(() => events(beacons)).toHaveLength(1);
	const ev = events(beacons)[0];
	expect(ev).toMatchObject({ v: 1, p: "/", n: "signup_click", l: { plan: "pro", n: "3" }, g: "member", o: "direct" });
	expect(ev.w).toBe(await page.evaluate(() => innerWidth));
	expect(errors).toEqual([]);
});

test("an invalid event name sends nothing and warns once", async ({ page }) => {
	const errors = watch(page);
	const warnings: string[] = [];
	page.on("console", (m) => { if (m.type() === "warning") warnings.push(m.text()); });
	const beacons = await serve(page, "<h1>Hello</h1>");
	await page.goto("https://site.test/");
	await expect.poll(() => views(beacons)).toHaveLength(1);
	await uc(page, "track", "");
	await uc(page, "track", "uc.x");
	await uc(page, "track", "UC.pageview");
	await uc(page, "track", "a".repeat(41));
	await uc(page, "track", "ok");
	await expect.poll(() => events(beacons)).toHaveLength(1);
	expect(events(beacons)[0].n).toBe("ok");
	expect(warnings).toEqual(["[upcontrol] track needs an event name of 1-40 characters, not starting uc."]);
	expect(errors).toEqual([]);
});

test("a track queued before the script loads is sent after the first view, in order", async ({ page }) => {
	const errors = watch(page);
	const beacons = await serve(page, "<h1>Hello</h1>", {
		head:
			"<script>window.upcontrol=window.upcontrol||function(){(upcontrol.q=upcontrol.q||[]).push(arguments)};" +
			"upcontrol('track','first');upcontrol('track','second',{x:1});</script>",
	});
	await page.goto("https://site.test/");
	await expect.poll(() => beacons).toHaveLength(3);
	expect(beacons.map((b) => b.n ?? b.t)).toEqual(["view", "first", "second"]);
	expect(beacons[2].l).toEqual({ x: "1" });
	expect(errors).toEqual([]);
});

test("a press on a data-uc-event element sends it, by pointer or keyboard", async ({ page }) => {
	const errors = watch(page);
	const beacons = await serve(page,
		'<button id="cta" data-uc-event="cta" data-uc-plan="pro"><span id="inner">Start</span> now</button>');
	await page.goto("https://site.test/");
	await expect.poll(() => views(beacons)).toHaveLength(1);
	await page.locator("#cta").click({ position: { x: 2, y: 2 } });
	await page.locator("#inner").click();
	await page.locator("#cta").focus();
	await page.keyboard.press("Enter");
	await expect.poll(() => events(beacons)).toHaveLength(3);
	for (const ev of events(beacons)) {
		expect(ev.n).toBe("cta");
		expect(ev.l).toEqual({ plan: "pro" });
	}
	expect(errors).toEqual([]);
});

test("the 201st event on one page is dropped", async ({ page }) => {
	const errors = watch(page);
	const beacons = await serve(page, "<h1>Hello</h1>");
	await page.goto("https://site.test/");
	await expect.poll(() => views(beacons)).toHaveLength(1);
	await page.evaluate(() => {
		const up = (window as unknown as { upcontrol: Uc }).upcontrol;
		for (let i = 0; i < 201; i++) up("track", "e" + i);
	});
	await expect.poll(() => events(beacons)).toHaveLength(200);
	await page.waitForTimeout(300);
	expect(events(beacons)).toHaveLength(200);
	expect(events(beacons).some((b) => b.n === "e200")).toBe(false);
	expect(errors).toEqual([]);
});

for (const [url, want] of [
	["https://site.test/?utm_source=news&utm_campaign=oct",
		{ source: "campaign", utm_source: "news", utm_campaign: "oct", landing: "/" }],
	["https://site.test/", { source: "direct", landing: "/" }],
] as const) {
	test(`upcontrol('source') hands the page its visit's source: ${want.source}`, async ({ page }) => {
		const errors = watch(page);
		const beacons = await serve(page, "<h1>Hello</h1>");
		await page.goto(url);
		await expect.poll(() => views(beacons)).toHaveLength(1);
		expect(await uc(page, "source")).toEqual(want);
		expect(errors).toEqual([]);
	});
}

const fm = (beacons: Beacon[]) => heats(beacons).flatMap((b) => b.fm ?? []);
const pagehide = (page: Page) => page.evaluate(() => dispatchEvent(new Event("pagehide")));

test("form fields report focus and fill, and never their value", async ({ page }) => {
	const errors = watch(page);
	const beacons = await serve(page, '<form id="f"><input id="name"><input id="mail"></form>');
	const raw: string[] = [];
	page.on("request", (r) => { if (/api\.test\/w\?/.test(r.url())) raw.push(r.postData() ?? ""); });
	await page.goto("https://site.test/");
	await expect.poll(() => views(beacons)).toHaveLength(1);
	await page.locator("#name").click();
	await page.keyboard.type("s3cret-Ada");
	await page.locator("#mail").click();
	await hide(page);
	await expect.poll(() => heats(beacons)).toHaveLength(1);
	// A tab hide is not abandonment: no left.
	expect(fm(beacons)).toEqual([["focus", "#name"], ["fill", "#name"], ["focus", "#mail"]]);
	expect(raw.length).toBeGreaterThanOrEqual(2);
	for (const body of raw) expect(body).not.toContain("s3cret");
	expect(errors).toEqual([]);
});

test("a submitted form is reported and never counted as left", async ({ page }) => {
	const errors = watch(page);
	const beacons = await serve(page,
		'<form id="f" onsubmit="event.preventDefault()"><input id="q"><button id="go">Go</button></form>');
	await page.goto("https://site.test/");
	await expect.poll(() => views(beacons)).toHaveLength(1);
	await page.locator("#q").click();
	await page.keyboard.type("x");
	await page.locator("#go").click();
	await pagehide(page);
	await expect.poll(() => heats(beacons)).toHaveLength(1);
	expect(fm(beacons)).toContainEqual(["submit", "#f"]);
	expect(fm(beacons).filter((e) => e[0] === "left")).toEqual([]);
	expect(errors).toEqual([]);
});

test("a route change from an unsubmitted form reports the field it was left from", async ({ page }) => {
	const errors = watch(page);
	const beacons = await serve(page, '<form id="f"><input id="a"><input id="b"></form>');
	await page.goto("https://site.test/");
	await expect.poll(() => views(beacons)).toHaveLength(1);
	await page.locator("#a").click();
	await page.locator("#b").click();
	await page.evaluate(() => history.pushState({}, "", "/next"));
	await expect.poll(() => views(beacons).filter((b) => b.p === "/next")).toHaveLength(1);
	const left = heats(beacons).filter((b) => b.p === "/").flatMap((b) => b.fm ?? []).filter((e) => e[0] === "left");
	expect(left).toEqual([["left", "#b"]]);
	expect(errors).toEqual([]);
});

test("a checked checkbox is filled, and a hidden input is never reported", async ({ page }) => {
	const errors = watch(page);
	const beacons = await serve(page, '<form id="f"><input type="hidden" id="h" value="x"><input type="checkbox" id="c"></form>');
	await page.goto("https://site.test/");
	await expect.poll(() => views(beacons)).toHaveLength(1);
	await page.locator("#c").check();
	await page.evaluate(() => {
		const h = document.getElementById("h")!;
		for (const t of ["focusin", "change", "focusout"]) h.dispatchEvent(new Event(t, { bubbles: true }));
	});
	await hide(page);
	await expect.poll(() => heats(beacons)).toHaveLength(1);
	expect(fm(beacons)).toContainEqual(["fill", "#c"]);
	expect(fm(beacons).filter((e) => e[1] === "#h")).toEqual([]);
	expect(errors).toEqual([]);
});

test("the Forms layer badges a field and its form, and the pill carries the field's numbers", async ({ page }) => {
	const errors = watch(page);
	const beacons = await serve(page,
		'<form id="f" style="padding:24px"><input id="email"> <button id="go" type="button">Go</button></form>');
	await page.route(/api\.test\/w\/heatmap/, (route) =>
		route.fulfill({
			contentType: "application/json",
			headers: { "Access-Control-Allow-Origin": "https://site.test" },
			body: JSON.stringify({
				path: "/", device: "desktop", range: "7d", views: 1000,
				clicks: [], rage: [], moves: [], hits: [], dead: [],
				scroll: Array.from({ length: 21 }, () => 0),
				forms: [
					{ selector: "#email", focus: 800, fill: 600, submit: 0, left: 340 },
					{ selector: "#f", focus: 0, fill: 0, submit: 200, left: 0 },
				],
			}),
		}));
	await page.goto("https://site.test/#uc-heatmap=uch_test");
	await expect(page.getByText("1,000 desktop views · 7 days")).toBeVisible();
	const badges = page.locator("[data-uc-heatmap] .badge");
	await expect(badges).toHaveCount(0);
	const forms = page.getByRole("button", { name: "Forms" });
	await forms.click();
	await expect(badges).toHaveCount(2);
	await expect(badges.filter({ hasText: "focus 80% · fill 60%" })).toContainText("left here 34%");
	await expect(badges.filter({ hasText: "submitted 20%" })).toHaveCount(1);
	await forms.click();
	await expect(badges).toHaveCount(0);
	if (test.info().project.name !== "mobile") {
		await page.locator("#email").hover();
		await expect(page.locator("[data-uc-heatmap] .pill")).toHaveText("focus 80% · fill 60% · left here 34%");
	}
	expect(beacons).toEqual([]);
	expect(errors).toEqual([]);
});

test("the Forms layer says so when no form activity is recorded", async ({ page }) => {
	const errors = watch(page);
	await serve(page, '<form id="f"><input id="email"></form>');
	await heatmapWith(page, []);
	await page.goto("https://site.test/#uc-heatmap=uch_test");
	await expect(page.getByText("1,234 desktop views · 7 days")).toBeVisible();
	await page.getByRole("button", { name: "Forms" }).click();
	await expect(page.getByText("No form activity recorded yet.")).toBeVisible();
	await expect(page.locator("[data-uc-heatmap] .badge")).toHaveCount(0);
	expect(errors).toEqual([]);
});

// A heatmap answer offering one deploy; with split= it carries the before window.
async function heatmapCompare(page: Page, beforeViews = 1000) {
	const urls: string[] = [];
	await page.route(/api\.test\/w\/heatmap/, (route) => {
		const url = route.request().url();
		urls.push(url);
		const split = url.includes("split=");
		return route.fulfill({
			contentType: "application/json",
			headers: { "Access-Control-Allow-Origin": "https://site.test" },
			body: JSON.stringify({
				path: "/", device: "desktop", range: "7d", views: 1000,
				clicks: [{ selector: "#buy", x: 32, y: 32, n: 300 }, { selector: "#other", x: 32, y: 32, n: 10 }],
				rage: [], moves: [], dead: [], segments: [], forms: [],
				hits: [{ selector: "#buy", x: 0, y: 0, n: 120 }],
				scroll: Array.from({ length: 21 }, (_, i) => 1000 - i * 40),
				// As the server answers: the list rides the first look only.
				deploys: split || url.includes("range=") ? [] : [{ at: "2026-10-17T14:03:00Z", label: "v1.4.2" }],
				...(split ? {
					from: "2026-10-17",
					before: {
						from: "2026-10-15", to: "2026-10-17", views: beforeViews,
						clicks: [{ selector: "#buy", x: 32, y: 32, n: 200 }, { selector: "#other", x: 32, y: 32, n: 90 }],
						rage: [], dead: [], forms: [],
						hits: [{ selector: "#buy", x: 0, y: 0, n: 90 }],
						scroll: Array.from({ length: 21 }, (_, i) => 1000 - i * 45),
					},
				} : {}),
			}),
		});
	});
	return urls;
}

test("the overlay offers each deploy to compare around", async ({ page }) => {
	const errors = watch(page);
	await serve(page, '<button id="buy">Buy</button> <button id="other">Other</button>');
	await heatmapCompare(page);
	await page.goto("https://site.test/#uc-heatmap=uch_test");
	const cmp = page.getByRole("combobox", { name: "Compare" });
	await expect(cmp.locator("option")).toHaveText(["No comparison", "Before / after v1.4.2 · Oct 17"]);
	expect(errors).toEqual([]);
});

test("the overlay offers no comparison for a project with no deploys", async ({ page }) => {
	const errors = watch(page);
	await serve(page, '<button id="buy">Buy</button>');
	await heatmapWith(page, []);
	await page.goto("https://site.test/#uc-heatmap=uch_test");
	await expect(page.getByText("1,234 desktop views · 7 days")).toBeVisible();
	await expect(page.getByRole("combobox", { name: "Compare" })).toHaveCount(0);
	expect(errors).toEqual([]);
});

test("comparing re-reads around the deploy, and No comparison goes back to the range", async ({ page }) => {
	const errors = watch(page);
	await serve(page, '<button id="buy">Buy</button> <button id="other">Other</button>');
	const urls = await heatmapCompare(page);
	await page.goto("https://site.test/#uc-heatmap=uch_test");
	await expect(page.getByText("1,000 desktop views · 7 days")).toBeVisible();
	const cmp = page.getByRole("combobox", { name: "Compare" });
	const range = page.locator("[data-uc-heatmap] select").first();
	await cmp.selectOption("2026-10-17T14:03:00Z");
	await expect(page.getByText("After: 1,000 views · before: 1,000 views")).toBeVisible();
	expect(urls.at(-1)).toContain("split=" + encodeURIComponent("2026-10-17T14:03:00Z"));
	expect(urls.at(-1)).not.toContain("range=");
	await expect(range).toBeDisabled();
	// A route change keeps the comparison.
	await page.evaluate(() => history.pushState({}, "", "/next"));
	await expect.poll(() => urls.at(-1)).toContain("path=%2Fnext");
	expect(urls.at(-1)).toContain("split=");
	await cmp.selectOption("");
	await expect(page.getByText("1,000 desktop views · 7 days")).toBeVisible();
	expect(urls.at(-1)).not.toContain("split=");
	expect(urls.at(-1)).toContain("range=7d");
	await expect(range).toBeEnabled();
	// Later answers carry no deploys; the first look's list stays on offer.
	await expect(cmp.locator("option")).toHaveText(["No comparison", "Before / after v1.4.2 · Oct 17"]);
	expect(errors).toEqual([]);
});

test("a comparison with no views before it says so", async ({ page }) => {
	const errors = watch(page);
	await serve(page, '<button id="buy">Buy</button>');
	await heatmapCompare(page, 0);
	await page.goto("https://site.test/#uc-heatmap=uch_test");
	await page.getByRole("combobox", { name: "Compare" }).selectOption("2026-10-17T14:03:00Z");
	await expect(page.getByText("Not enough views on both sides to compare.")).toBeVisible();
	expect(errors).toEqual([]);
});

test("in compare mode the pill reads before -> after, and every click layer draws", async ({ page }) => {
	test.skip(test.info().project.name === "mobile", "a phone has no hover");
	const errors = watch(page);
	await serve(page, '<button id="buy">Buy</button> <button id="other">Other</button>');
	await heatmapCompare(page);
	await page.goto("https://site.test/#uc-heatmap=uch_test");
	await page.getByRole("combobox", { name: "Compare" }).selectOption("2026-10-17T14:03:00Z");
	await expect(page.getByText("After: 1,000 views · before: 1,000 views")).toBeVisible();
	await page.locator("#buy").hover();
	await expect(page.locator("[data-uc-heatmap] .pill"))
		.toHaveText("CTR 9% -> 12% (+3 pts) · clicks 0.20 -> 0.30 per view");
	for (const name of ["Rage", "Dead", "Moves", "Scroll", "Forms"]) await page.getByRole("button", { name }).click();
	await page.locator("#other").hover();
	await expect(page.getByText(/^\d+% -> \d+% of views reached here$/)).toBeVisible();
	await page.waitForTimeout(300);
	expect(errors).toEqual([]);
});
