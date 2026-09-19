// upcontrol uc.js: the script behind
// <script defer src=".../uc.js" data-key="uc_pub_..."></script>.
// It records page views and interaction heat for the public key in data-key,
// or, opened through a heatmap link (#uc-heatmap=<token>), it records nothing
// and draws the page's heatmap over the page instead. Served verbatim:
// no build, no dependency, ES2017, one IIFE. ASCII only: a page or a CDN that
// serves it without a charset decodes it as Latin-1, so the rest are escapes.
(function () {
	'use strict';

	var me = document.currentScript;
	if (!me) return;
	var base = new URL(me.src).origin;
	var key = me.getAttribute('data-key');

	// A heatmap token in the hash is saved for the tab and scrubbed from the
	// URL, so a refresh or an SPA hop keeps drawing without re-opening the
	// link. A token means draw, never collect: the owner's own browsing is
	// not a visitor.
	var token = '';
	var m = location.hash.match(/(?:^|[#&])uc-heatmap=(uch_[A-Za-z0-9_-]+)/);
	if (m) {
		token = m[1];
		try { sessionStorage.setItem('uc-heatmap', token); } catch (e) {}
		var kept = location.hash.replace(/^#/, '').split('&').filter(function (p) {
			return p && p.indexOf('uc-heatmap=') !== 0;
		}).join('&');
		history.replaceState(history.state, '',
			location.pathname + location.search + (kept ? '#' + kept : ''));
	} else {
		try { token = sessionStorage.getItem('uc-heatmap') || ''; } catch (e) {}
	}

	if (!key) {
		console.warn('[upcontrol] uc.js needs a data-key attribute');
		return;
	}
	// A secret key in a page is public the moment the page loads. The server
	// refuses it anyway; saying so here is how the owner finds out.
	if (key.indexOf('uc_live_') === 0) {
		console.error('[upcontrol] data-key holds a SECRET key. Revoke it in the app now and use the public key from npx upcontrol web.');
		return;
	}
	if (token) { overlay(token); return; }

	// --- collector ---------------------------------------------------------

	var path = location.pathname;
	var clicks = new Map(); // cell key -> [sel, fx, fy, clicks, rage]
	var moves = new Map(); // cell key -> [sel, fx, fy, samples]
	var recent = []; // the last 700 ms of clicks, for rage detection
	var maxReach = 0;
	var scrollSent = false;
	var firstView = true;

	function post(json) {
		fetch(base + '/w?key=' + encodeURIComponent(key), {
			method: 'POST',
			body: json,
			keepalive: true,
			credentials: 'omit'
			// No headers on purpose: a string body is text/plain, which a
			// browser sends without a preflight.
		}).catch(function () {});
	}

	function view() {
		post(JSON.stringify({
			v: 1, t: 'view', p: path, w: innerWidth,
			r: firstView ? document.referrer : '',
			q: location.search
		}));
		firstView = false;
	}

	// The element a position belongs to: the nearest interactive ancestor,
	// or the element holding the svg the click landed in.
	function norm(t) {
		if (!(t instanceof Element)) return null;
		var el = t.closest('a,button,input,select,textarea,label,summary,[role=button],[role=link]');
		if (el) return el;
		var svg = t.closest('svg');
		return svg ? svg.parentElement : t;
	}

	// A CSS path of ids and tags. A clean id stops the walk; a tag carries
	// :nth-of-type whenever the parent has more than one of it. Never class
	// names: a build re-hashes them. Invariant: querySelector(sel(el)) === el.
	function sel(el) {
		var parts = [];
		var node = el;
		var stop = '';
		while (node) {
			if (node === document.body) { stop = 'body'; break; }
			if (node === document.documentElement) break; // reached <html> without a body
			var id = node.id;
			if (id && /^[A-Za-z][\w-]*$/.test(id) && id.length <= 64 && !/\d{4}/.test(id)) {
				parts.unshift('#' + id);
				stop = 'id';
				break;
			}
			var tag = node.localName;
			if (!/^[a-z][a-z0-9-]*$/.test(tag)) break;
			var part = tag;
			var parent = node.parentElement;
			if (parent) {
				var same = 0, at = 0;
				for (var i = 0; i < parent.children.length; i++) {
					var c = parent.children[i];
					if (c.localName === tag) { same++; if (c === node) at = same; }
				}
				if (same > 1) part += ':nth-of-type(' + at + ')';
			}
			parts.unshift(part);
			node = parent;
		}
		if (!stop) return '';
		if (stop === 'body') parts.unshift('body');
		var s = parts.join('>');
		return s.length <= 256 ? s : '';
	}

	// Where inside the element the pointer is, in 64ths. null when the
	// element has no box.
	function cell(el, x, y) {
		var r = el.getBoundingClientRect();
		if (r.width <= 0 || r.height <= 0) return null;
		var fx = Math.floor((x - r.left) / r.width * 64);
		var fy = Math.floor((y - r.top) / r.height * 64);
		return [Math.min(63, Math.max(0, fx)), Math.min(63, Math.max(0, fy))];
	}

	function rage(r) {
		if (!r.raged) { r.raged = true; r.entry[4]++; }
	}

	document.addEventListener('click', function (e) {
		if (e.detail === 0) return; // keyboard activation carries no position
		var el = norm(e.target);
		if (!el) return;
		var s = sel(el);
		if (!s) return;
		var c = cell(el, e.clientX, e.clientY);
		if (!c) return;
		var k = s + ':' + c[0] + ':' + c[1];
		var entry = clicks.get(k);
		if (!entry) {
			if (clicks.size >= 300) return; // full: new cells ignored, old ones still count
			entry = [s, c[0], c[1], 0, 0];
			clicks.set(k, entry);
		}
		entry[3]++;
		// Rage: >= 3 clicks within 700 ms and 24 px of each other. Every
		// click of such a group adds one rage to its own cell, once.
		var rec = { t: e.timeStamp, x: e.clientX, y: e.clientY, entry: entry, raged: false };
		var group = [];
		var live = [];
		for (var i = 0; i < recent.length; i++) {
			var r = recent[i];
			if (rec.t - r.t > 700) continue;
			live.push(r);
			var dx = r.x - rec.x, dy = r.y - rec.y;
			if (dx * dx + dy * dy <= 576) group.push(r);
		}
		if (group.length >= 2) {
			rage(rec);
			for (var j = 0; j < group.length; j++) rage(group[j]);
		}
		live.push(rec);
		recent = live;
	}, { capture: true, passive: true });

	var lastMove = 0;
	if (matchMedia('(hover: hover) and (pointer: fine)').matches) {
		document.addEventListener('mousemove', function (e) {
			if (e.timeStamp - lastMove < 100) return;
			lastMove = e.timeStamp;
			var el = norm(e.target);
			if (!el) return;
			var s = sel(el);
			if (!s) return;
			var c = cell(el, e.clientX, e.clientY);
			if (!c) return;
			var k = s + ':' + c[0] + ':' + c[1];
			var entry = moves.get(k);
			if (entry) entry[3]++;
			else if (moves.size < 600) moves.set(k, [s, c[0], c[1], 1]);
		}, { passive: true });
	}

	function docHeight() {
		var b = document.body;
		return Math.max(document.documentElement.scrollHeight, b ? b.scrollHeight : 0, 1);
	}

	var raf = false;
	function measure() {
		raf = false;
		maxReach = Math.max(maxReach, Math.min(1, (scrollY + innerHeight) / docHeight()));
	}
	document.addEventListener('scroll', function () {
		if (!raf) { raf = true; requestAnimationFrame(measure); }
	}, { passive: true });

	// One depth per page view, or the histogram would count it twice.
	function flush() {
		measure();
		if (clicks.size === 0 && moves.size === 0 && scrollSent) return;
		var body = { v: 1, t: 'heat', p: path, w: innerWidth };
		if (!scrollSent) {
			scrollSent = true;
			body.s = Math.round(maxReach * 1000) / 1000;
		}
		var cArr = [], mArr = [];
		clicks.forEach(function (v) { cArr.push(v); });
		moves.forEach(function (v) { mArr.push(v); });
		if (cArr.length) { cArr.sort(function (a, b) { return b[3] - a[3]; }); body.c = cArr; }
		if (mArr.length) { mArr.sort(function (a, b) { return b[3] - a[3]; }); body.m = mArr; }
		// The keepalive budget is 64 KB for the whole page, the host's own
		// beacons included; keep to half of it by halving the richest lists
		// first until the body fits.
		var json = JSON.stringify(body);
		while (json.length > 30000) {
			if (body.m && body.m.length > 1) body.m = body.m.slice(0, Math.ceil(body.m.length / 2));
			else if (body.c && body.c.length > 1) body.c = body.c.slice(0, Math.ceil(body.c.length / 2));
			else break;
			json = JSON.stringify(body);
		}
		post(json);
		clicks.clear();
		moves.clear();
	}

	document.addEventListener('visibilitychange', function () {
		if (document.visibilityState === 'hidden') flush();
	});
	addEventListener('pagehide', flush);

	function onRoute() {
		if (location.pathname === path) return;
		flush();
		path = location.pathname;
		scrollSent = false;
		maxReach = 0;
		view();
	}
	onHistory(onRoute);
	addEventListener('popstate', onRoute);

	view();

	// Calls fn after every pushState and replaceState; returns the undo, which
	// puts the originals back.
	function onHistory(fn) {
		var saved = ['pushState', 'replaceState'].map(function (name) {
			var orig = history[name];
			history[name] = function () {
				var out = orig.apply(this, arguments);
				fn();
				return out;
			};
			return { name: name, orig: orig };
		});
		return function () {
			saved.forEach(function (w) { history[w.name] = w.orig; });
		};
	}

	// --- overlay: draw the heatmap, never collect ---------------------------

	function overlay(token) {
		// Empty until the first answer: the server picks the deepest range the plan reaches.
		var range = '';
		var layers = { clicks: true, rage: false, moves: false, scroll: false };
		var hm = null;
		var resolved = new Map(); // selector -> element | null
		var lastResolve = 0;
		var retry = 0;
		var path = location.pathname;
		var raf = false;
		var dpr = 1;

		function forget() {
			try { sessionStorage.removeItem('uc-heatmap'); } catch (e) {}
		}

		var host = document.createElement('div');
		host.setAttribute('data-uc-heatmap', '');
		var shadow = host.attachShadow({ mode: 'open' });
		shadow.innerHTML =
			'<style>' +
			':host{all:initial}' +
			'.bar{position:fixed;top:12px;right:12px;z-index:2147483647;box-sizing:border-box;display:flex;flex-direction:column;gap:8px;max-width:340px;padding:10px 12px;background:#16181d;color:#f4f5f7;border-radius:8px;font:13px/1.45 system-ui,sans-serif;box-shadow:0 6px 24px rgba(0,0,0,.4)}' +
			'.row{display:flex;align-items:center;gap:6px;flex-wrap:wrap}' +
			'.title{font-weight:600;white-space:nowrap}' +
			'.path{color:#9aa1ad;font:12px ui-monospace,monospace;overflow:hidden;text-overflow:ellipsis}' +
			'.status{color:#c8cdd6;display:flex;flex-direction:column;gap:2px}' +
			'.note{color:#e8b64d}' +
			'button,select{box-sizing:border-box;font:inherit;color:inherit;background:#23262e;border:1px solid #343842;border-radius:6px;padding:3px 9px;cursor:pointer}' +
			'button[aria-pressed="true"]{background:#e8e9ec;border-color:#e8e9ec;color:#16181d}' +
			'a{color:#7ab0ff}' +
			'</style>' +
			'<div class="bar">' +
			'<div class="row"><span class="title">UpControl heatmap</span><span class="path"></span></div>' +
			'<div class="status"><div class="main"></div><div class="hint"></div></div>' +
			'<div class="note" hidden></div>' +
			'<div class="row">' +
			'<button name="clicks" aria-pressed="true">Clicks</button>' +
			'<button name="rage" aria-pressed="false">Rage</button>' +
			'<button name="moves" aria-pressed="false">Moves</button>' +
			'<button name="scroll" aria-pressed="false">Scroll</button>' +
			'<select><option value="24h">1 day</option><option value="7d" selected>7 days</option><option value="31d">31 days</option></select>' +
			'<button name="close">Close</button>' +
			'</div>' +
			'</div>';
		var bar = shadow.querySelector('.bar');
		var pathEl = shadow.querySelector('.path');
		var mainEl = shadow.querySelector('.main');
		var hintEl = shadow.querySelector('.hint');
		var noteEl = shadow.querySelector('.note');
		var select = shadow.querySelector('select');
		pathEl.textContent = path;

		var canvas = document.createElement('canvas');
		canvas.setAttribute('aria-hidden', 'true');
		var ctx = canvas.getContext('2d');
		var off = document.createElement('canvas');
		var offCtx = off.getContext('2d');

		function sizeCanvas() {
			dpr = Math.min(devicePixelRatio || 1, 2);
			canvas.width = Math.round(innerWidth * dpr);
			canvas.height = Math.round(innerHeight * dpr);
			canvas.style.cssText = 'position:fixed;inset:0;width:100vw;height:100vh;pointer-events:none;z-index:2147483646';
		}

		function statusMain(msg, upgrade) {
			mainEl.textContent = msg;
			if (upgrade) {
				var a = document.createElement('a');
				a.href = base + '/app/plan';
				a.target = '_blank';
				a.rel = 'noopener';
				a.textContent = 'Upgrade';
				mainEl.appendChild(document.createTextNode(' '));
				mainEl.appendChild(a);
			}
		}

		function label(r) {
			return r === '24h' ? '1 day' : r === '31d' ? '31 days' : '7 days';
		}

		// Mirrors deviceFor in core/back/internal/api/web.go.
		function deviceFor(w) {
			return w < 768 ? 'mobile' : w < 1024 ? 'tablet' : 'desktop';
		}

		function updateStatus() {
			if (!hm) return;
			if (layers.scroll) {
				statusMain(hm.scroll && hm.scroll[0]
					? 'Scroll depth from ' + hm.scroll[0] + ' views'
					: 'No scroll depth reported yet.');
			} else {
				statusMain(hm.views.toLocaleString('en-US') + ' ' + hm.device + ' views \u00b7 ' + label(range));
			}
			hintEl.textContent = hm.device === 'mobile'
				? 'Widen the window past 1024px to see desktop visitors.'
				: 'This window is ' + innerWidth + 'px wide. Narrow it below 768px to see phones.';
		}

		function load() {
			hm = null;
			statusMain('Loading\u2026');
			fetch(base + '/w/heatmap?token=' + encodeURIComponent(token) +
				'&path=' + encodeURIComponent(location.pathname) +
				'&key=' + encodeURIComponent(key) +
				'&w=' + innerWidth + (range ? '&range=' + range : ''), { credentials: 'omit' })
				.then(function (res) {
					if (res.status === 401) {
						statusMain('This heatmap link has expired. Open it again from UpControl.');
						forget();
						return null;
					}
					if (res.status === 402) {
						return res.json().catch(function () { return null; }).then(function (j) {
							statusMain((j && j.error && j.error.message) || 'Could not load the heatmap.', true);
							return null;
						});
					}
					if (!res.ok) {
						statusMain('Could not load the heatmap.');
						return null;
					}
					return res.json();
				})
				.then(function (data) {
					if (!data) return;
					hm = data;
					range = data.range;
					select.value = range;
					resolved = new Map();
					['clicks', 'rage', 'moves'].forEach(function (k) {
						(hm[k] || []).forEach(function (c) { resolved.set(c.selector, null); });
					});
					lastResolve = 0;
					updateStatus();
					schedule();
				})
				.catch(function () {
					// A rejected fetch is either the network or a CORS refusal,
					// and the page cannot tell which.
					statusMain("Could not reach UpControl, or this page's address is not listed on the project's website key.");
				});
		}

		// 256 steps: transparent -> blue -> cyan -> lime -> yellow -> red.
		var PAL = [];
		(function () {
			var stops = [[0, 0, 0], [0, 0, 255], [0, 255, 255], [0, 255, 0], [255, 255, 0], [255, 0, 0]];
			for (var i = 0; i < 256; i++) {
				var t = i / 255 * (stops.length - 1);
				var k = Math.min(Math.floor(t), stops.length - 2);
				var u = t - k;
				PAL.push([
					Math.round(stops[k][0] + (stops[k + 1][0] - stops[k][0]) * u),
					Math.round(stops[k][1] + (stops[k + 1][1] - stops[k][1]) * u),
					Math.round(stops[k][2] + (stops[k + 1][2] - stops[k][2]) * u)
				]);
			}
		})();

		// Returns how many of the cells' hits have no element with a box here.
		// An element's box, or null when it is missing, display:none or
		// detached: a zero box would paint at the top-left.
		function box(sel) {
			var el = resolved.get(sel);
			var r = el && el.getBoundingClientRect();
			return r && r.width && r.height ? r : null;
		}

		function drawHeat(cells, radius) {
			var pts = [];
			var maxN = 0;
			cells.forEach(function (c) {
				var r = box(c.selector);
				if (!r) return;
				// The scale is the whole page's, so scrolling never re-colours a spot.
				if (c.n > maxN) maxN = c.n;
				// Viewport coordinates, so the fixed canvas needs no scroll
				// arithmetic.
				var x = r.left + (c.x + 0.5) / 64 * r.width;
				var y = r.top + (c.y + 0.5) / 64 * r.height;
				if (x < -radius || x > innerWidth + radius || y < -radius || y > innerHeight + radius) return;
				pts.push([x, y, c.n]);
			});
			if (!pts.length) return;
			off.width = canvas.width;
			off.height = canvas.height;
			offCtx.setTransform(dpr, 0, 0, dpr, 0, 0);
			offCtx.clearRect(0, 0, innerWidth, innerHeight);
			pts.forEach(function (p) {
				var a = Math.max(0.08, p[2] / maxN);
				var g = offCtx.createRadialGradient(p[0], p[1], 0, p[0], p[1], radius);
				g.addColorStop(0, 'rgba(0,0,0,' + a + ')');
				g.addColorStop(1, 'rgba(0,0,0,0)');
				offCtx.fillStyle = g;
				offCtx.fillRect(p[0] - radius, p[1] - radius, radius * 2, radius * 2);
			});
			var img = offCtx.getImageData(0, 0, off.width, off.height);
			var d = img.data;
			for (var i = 0; i < d.length; i += 4) {
				var a = d[i + 3];
				if (!a) continue;
				var p = PAL[a];
				d[i] = p[0];
				d[i + 1] = p[1];
				d[i + 2] = p[2];
				d[i + 3] = Math.round(a * 0.75);
			}
			offCtx.putImageData(img, 0, 0);
			ctx.save();
			ctx.setTransform(1, 0, 0, 1, 0, 0);
			ctx.drawImage(off, 0, 0);
			ctx.restore();
		}

		function drawLine(y, text) {
			// The threshold itself, across the page, then its label on the left.
			ctx.fillStyle = 'rgba(255,255,255,0.9)';
			ctx.fillRect(0, Math.round(y) - 1, innerWidth, 2);
			ctx.font = '12px system-ui, sans-serif';
			ctx.textBaseline = 'middle';
			var w = ctx.measureText(text).width;
			ctx.fillStyle = 'rgba(10,12,16,0.85)';
			ctx.fillRect(0, y - 10, w + 16, 20);
			ctx.fillStyle = '#fff';
			ctx.fillText(text, 8, y);
		}

		function drawScroll() {
			var sc = hm.scroll || [];
			var total = sc[0] || 0;
			if (!total) return;
			var doc = docHeight();
			var band = doc / 20;
			for (var i = 0; i < 20; i++) {
				var share = (sc[i] || 0) / total;
				ctx.fillStyle = 'rgba(' + Math.round(share * 255) + ',0,' + Math.round((1 - share) * 255) + ',0.35)';
				// Whole pixels on both edges: overlapping bands double their alpha into a seam.
				var top = Math.round(i * band - scrollY);
				ctx.fillRect(0, top, innerWidth, Math.round((i + 1) * band - scrollY) - top);
			}
			// A line where the share first drops under 75, 50 and 25 percent, labelled with the
			// share actually measured there: two thresholds crossed in one band are one line.
			var drawn = {};
			[0.75, 0.5, 0.25].forEach(function (mark) {
				for (var b = 0; b < 20; b++) {
					var share = (sc[b] || 0) / total;
					if (share < mark) {
						if (!drawn[b]) {
							drawn[b] = true;
							drawLine(Math.round(b * band - scrollY), Math.round(share * 100) + '% of views reached here');
						}
						break;
					}
				}
			});
		}

		function draw() {
			raf = false;
			ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
			ctx.clearRect(0, 0, innerWidth, innerHeight);
			if (!hm) return;
			// Late-rendered and replaced nodes: look again for what is missing,
			// at most every 500 ms, and once more after the last throttled frame.
			var now = Date.now();
			if (now - lastResolve >= 500) {
				lastResolve = now;
				resolved.forEach(function (el, s) {
					if (el && el.isConnected) return;
					el = null;
					try { el = document.querySelector(s); } catch (e) {}
					resolved.set(s, el);
				});
			} else if (!retry) {
				retry = setTimeout(function () { retry = 0; schedule(); }, 500 - (now - lastResolve));
			}
			if (layers.scroll) drawScroll();
			var heat = [];
			if (layers.clicks) heat = heat.concat(hm.clicks || []);
			if (layers.rage) heat = heat.concat(hm.rage || []);
			if (heat.length) drawHeat(heat, 22);
			if (layers.moves) drawHeat(hm.moves || [], 28);
			// Counted from one layer: a rage click is a click too.
			var lost = 0;
			((layers.clicks ? hm.clicks : layers.rage ? hm.rage : null) || []).forEach(function (c) {
				if (!box(c.selector)) lost += c.n;
			});
			noteEl.hidden = !lost;
			noteEl.textContent = lost + (layers.clicks ? ' clicks' : ' rage clicks') + ' landed on elements not on this page now.';
		}

		function schedule() {
			if (!raf) { raf = true; requestAnimationFrame(draw); }
		}

		function onResize() {
			sizeCanvas();
			if (hm && deviceFor(innerWidth) !== hm.device) { load(); return; }
			schedule();
		}

		function onRoute() {
			if (location.pathname === path) return;
			path = location.pathname;
			pathEl.textContent = path;
			load();
		}

		var unwrap = onHistory(onRoute);

		function close() {
			removeEventListener('scroll', schedule, { capture: true });
			clearTimeout(retry);
			removeEventListener('resize', onResize);
			removeEventListener('popstate', onRoute);
			unwrap();
			forget();
			host.remove();
			canvas.remove();
		}

		bar.addEventListener('click', function (e) {
			var b = e.target.closest('button');
			if (!b) return;
			if (b.name === 'close') { close(); return; }
			if (Object.prototype.hasOwnProperty.call(layers, b.name)) {
				layers[b.name] = !layers[b.name];
				b.setAttribute('aria-pressed', String(layers[b.name]));
				updateStatus();
				schedule();
			}
		});
		select.addEventListener('change', function () {
			range = select.value;
			load();
		});
		// Capture: an app shell scrolls an inner container, not the window.
		addEventListener('scroll', schedule, { capture: true, passive: true });
		addEventListener('resize', onResize);
		addEventListener('popstate', onRoute);

		sizeCanvas();
		// On <html>, not <body>: a router that swaps the body keeps the overlay.
		document.documentElement.appendChild(canvas);
		document.documentElement.appendChild(host);
		load();
	}
})();
