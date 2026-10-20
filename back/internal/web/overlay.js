// upcontrol overlay.js: the heatmap overlay uc.js loads, from the same origin,
// only when the page was opened through a heatmap link (#uc-heatmap=<token>).
// Visitors never download it. It draws the page's heatmap and collects
// nothing. uc.js hands it what the two share (h), so nothing is duplicated.
// Served verbatim: no build, no dependency, ES2017, one IIFE. ASCII only, as uc.js.
(function () {
	'use strict';

	window.__ucOverlay = function (token, h) {
		var base = h.base;
		var key = h.key;
		var norm = h.norm;
		var sel = h.sel;
		var INTERACTIVE = h.INTERACTIVE;
		var onHistory = h.onHistory;
		var docHeight = h.docHeight;
		// Empty until the first answer: the server picks the deepest range the plan reaches.
		var range = '';
		var segment = ''; // the filters, '' for all; kept across loads and routes
		var source = '';
		var split = ''; // the deploy compared around, its `at`; '' for no comparison
		var deploys = []; // the first look's list: later answers carry none
		var layers = { clicks: true, rage: false, dead: false, moves: false, scroll: false, forms: false };
		var hm = null;
		var stats = new Map(); // selector -> { clicks, rage, dead, hits }, sums of n
		var beforeStats = new Map(); // the same, of hm.before
		var formStats = new Map(); // selector -> its hm.forms entry
		var beforeForms = new Map(); // selector -> its hm.before.forms entry
		var tipSel = null;
		var tipEl = null;
		var tipEv = null;
		var tipTimer = 0;
		var tipFrame = 0;
		var resolved = new Map(); // selector -> element | null
		var lastResolve = 0;
		var retry = 0;
		var path = location.pathname;
		var raf = false;
		var pos = null; // where the header was dragged to, null while the panel sits in its corner
		var drag = null;
		var dpr = 1;
		var seq = 0; // the newest load(); an older answer arriving later is dropped

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
			'.head{cursor:move;touch-action:none;user-select:none;flex-wrap:nowrap}' +
			'.title{font-weight:600;white-space:nowrap}' +
			'.path{color:#9aa1ad;font:12px ui-monospace,monospace;flex:1 1 auto;min-width:0;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}' +
			'.status{color:#c8cdd6;display:flex;flex-direction:column;gap:2px}' +
			'.note{color:#e8b64d}' +
			'button,select{box-sizing:border-box;font:inherit;color:inherit;background:#23262e;border:1px solid #343842;border-radius:6px;padding:3px 9px;cursor:pointer}' +
			'button[aria-pressed="true"]{background:#e8e9ec;border-color:#e8e9ec;color:#16181d}' +
			'.x{flex:none;padding:0 7px;font-size:16px;line-height:1.3}' +
			'a{color:#7ab0ff}' +
			'.pill{position:fixed;z-index:2147483646;padding:3px 8px;border-radius:6px;background:rgba(22,24,29,.92);color:#f4f5f7;font:12px system-ui,sans-serif;white-space:nowrap;pointer-events:none}' +
			'.outline{position:fixed;z-index:2147483646;box-sizing:border-box;border:1px dashed rgba(255,255,255,.9);box-shadow:0 0 0 1px rgba(0,0,0,.6);pointer-events:none}' +
			'.badge{position:fixed;z-index:2147483646;transform:translateX(-100%);padding:2px 6px;border-radius:4px;background:rgba(22,24,29,.92);color:#f4f5f7;font:11px/1.35 system-ui,sans-serif;white-space:pre;pointer-events:none}' +
			'</style>' +
			'<div class="outline" hidden></div><div class="pill" hidden></div><div class="badges"></div>' +
			'<div class="bar">' +
			'<div class="row head"><span class="title">UpControl heatmap</span><span class="path"></span>' +
			'<button name="close" class="x" aria-label="Close">&#215;</button></div>' +
			'<div class="status"><div class="main"></div><div class="hint"></div></div>' +
			'<div class="note" hidden></div>' +
			'<div class="row">' +
			'<button name="clicks" aria-pressed="true">Clicks</button>' +
			'<button name="rage" aria-pressed="false">Rage</button>' +
			'<button name="dead" aria-pressed="false">Dead</button>' +
			'<button name="moves" aria-pressed="false">Moves</button>' +
			'<button name="scroll" aria-pressed="false">Scroll</button>' +
			'<button name="forms" aria-pressed="false">Forms</button>' +
			'<select><option value="24h">1 day</option><option value="7d" selected>7 days</option><option value="31d">31 days</option></select>' +
			'</div>' +
			'<div class="row">' +
			'<select name="segment" aria-label="Segment" hidden></select>' +
			'<select name="source" aria-label="Source"><option value="">All sources</option><option value="search">Search</option>' +
			'<option value="social">Social</option><option value="campaign">Campaign</option><option value="referral">Referral</option>' +
			'<option value="direct">Direct</option></select>' +
			'<select name="compare" aria-label="Compare" hidden></select>' +
			'</div>' +
			'</div>';
		var bar = shadow.querySelector('.bar');
		var pathEl = shadow.querySelector('.path');
		var mainEl = shadow.querySelector('.main');
		var hintEl = shadow.querySelector('.hint');
		var noteEl = shadow.querySelector('.note');
		var select = shadow.querySelector('select');
		var segSelect = shadow.querySelector('select[name=segment]');
		var srcSelect = shadow.querySelector('select[name=source]');
		var cmpSelect = shadow.querySelector('select[name=compare]');
		var head = shadow.querySelector('.head');
		var pill = shadow.querySelector('.pill');
		var outline = shadow.querySelector('.outline');
		var badges = shadow.querySelector('.badges');
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

		function filters() {
			return (segment ? ' \u00b7 ' + segment : '') + (source ? ' \u00b7 from ' + source : '');
		}

		// The project's segments, plus the chosen one should it have dropped out
		// of the list, so the select never hides a filter in force.
		function fillSegments() {
			var list = (hm.segments || []).slice();
			if (segment && list.indexOf(segment) < 0) list.push(segment);
			segSelect.hidden = !list.length;
			segSelect.textContent = '';
			[''].concat(list).forEach(function (s) {
				var o = document.createElement('option');
				o.value = s;
				o.textContent = s || 'All visitors';
				segSelect.appendChild(o);
			});
			segSelect.value = segment;
		}

		// The deploys to compare around, plus the chosen one should it have dropped out.
		function fillCompare() {
			// The server lists deploys on the first look only: keep that list for every later load.
			if (hm.deploys && hm.deploys.length) deploys = hm.deploys;
			var list = deploys.slice();
			if (split && !list.some(function (d) { return d.at === split; })) list.push({ at: split, label: 'deploy' });
			cmpSelect.hidden = !list.length;
			cmpSelect.textContent = '';
			[{ at: '', label: '' }].concat(list).forEach(function (d) {
				var o = document.createElement('option');
				o.value = d.at;
				o.textContent = d.at
					? 'Before / after ' + d.label + ' \u00b7 ' +
						new Date(d.at).toLocaleDateString('en-US', { month: 'short', day: 'numeric', timeZone: 'UTC' })
					: 'No comparison';
				cmpSelect.appendChild(o);
			});
			cmpSelect.value = split;
		}

		// Both windows have views: a difference of per-view rates means something.
		function comparable() {
			return hm.views > 0 && hm.before.views > 0;
		}

		function updateStatus() {
			if (!hm) return;
			var noHeat = !(hm.clicks || []).length && !(hm.rage || []).length &&
				!(hm.moves || []).length && !(hm.scroll && hm.scroll[0]);
			if (hm.heatPages != null && hm.views > 0 && noHeat) {
				statusMain('This plan keeps heatmaps for its ' + hm.heatPages + ' busiest pages; this page is not among them.');
			} else if (hm.before) {
				statusMain(comparable()
					? 'After: ' + fmt(hm.views) + ' views \u00b7 before: ' + fmt(hm.before.views) + ' views' + filters()
					: 'Not enough views on both sides to compare.');
			} else if (layers.forms && !hm.forms.length) {
				statusMain('No form activity recorded yet.');
			} else if (layers.scroll) {
				statusMain(hm.scroll && hm.scroll[0]
					? 'Scroll depth from ' + hm.scroll[0] + ' views' + filters()
					: 'No scroll depth reported yet.');
			} else {
				statusMain(hm.views.toLocaleString('en-US') + ' ' + hm.device + ' views \u00b7 ' + label(range) + filters());
			}
			hintEl.textContent = hm.device === 'mobile'
				? 'Widen the window past 1024px to see desktop visitors.'
				: 'This window is ' + innerWidth + 'px wide. Narrow it below 768px to see phones.';
		}

		function load() {
			hm = null;
			badges.textContent = '';
			statusMain('Loading\u2026');
			var my = ++seq;
			fetch(base + '/w/heatmap?token=' + encodeURIComponent(token) +
				'&path=' + encodeURIComponent(location.pathname) +
				'&key=' + encodeURIComponent(key) +
				'&w=' + innerWidth + (split ? '&split=' + encodeURIComponent(split) : range ? '&range=' + range : '') +
				(segment ? '&segment=' + encodeURIComponent(segment) : '') +
				(source ? '&source=' + source : ''), { credentials: 'omit' })
				.then(function (res) {
					if (my !== seq) return null;
					if (res.status === 401) {
						statusMain('This heatmap link has expired. Open it again from UpControl.');
						forget();
						return null;
					}
					if (res.status === 402) {
						return res.json().catch(function () { return null; }).then(function (j) {
							if (my !== seq) return null;
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
					if (my !== seq || !data) return;
					hm = data;
					hm.forms = hm.forms || [];
					// While comparing, the range is the reader's choice for later, not this answer's.
					if (!split) {
						range = data.range;
						select.value = range;
					}
					fillSegments();
					fillCompare();
					resolved = new Map();
					[hm, hm.before || {}].forEach(function (w) {
						['clicks', 'rage', 'dead', 'moves', 'forms'].forEach(function (k) {
							(w[k] || []).forEach(function (c) { resolved.set(c.selector, null); });
						});
					});
					formStats = new Map();
					hm.forms.forEach(function (f) { formStats.set(f.selector, f); });
					beforeForms = new Map();
					((hm.before && hm.before.forms) || []).forEach(function (f) { beforeForms.set(f.selector, f); });
					stats = tally(hm);
					beforeStats = tally(hm.before || {});
					lastResolve = 0;
					updateStatus();
					schedule();
				})
				.catch(function () {
					if (my !== seq) return;
					// A rejected fetch is either the network or a CORS refusal,
					// and the page cannot tell which.
					statusMain("Could not reach UpControl, or this page's address is not listed on the project's website key.");
				});
		}

		// A window's per-selector sums of n.
		function tally(w) {
			var m = new Map();
			['clicks', 'rage', 'dead', 'hits'].forEach(function (k) {
				(w[k] || []).forEach(function (c) {
					var st = m.get(c.selector);
					if (!st) { st = { clicks: 0, rage: 0, dead: 0, hits: 0 }; m.set(c.selector, st); }
					st[k] += c.n;
				});
			});
			return m;
		}

		// 256 steps between the stops.
		function ramp(stops) {
			var pal = [];
			for (var i = 0; i < 256; i++) {
				var t = i / 255 * (stops.length - 1);
				var k = Math.min(Math.floor(t), stops.length - 2);
				var u = t - k;
				pal.push([
					Math.round(stops[k][0] + (stops[k + 1][0] - stops[k][0]) * u),
					Math.round(stops[k][1] + (stops[k + 1][1] - stops[k][1]) * u),
					Math.round(stops[k][2] + (stops[k + 1][2] - stops[k][2]) * u)
				]);
			}
			return pal;
		}
		// transparent -> blue -> cyan -> lime -> yellow -> red: the heat, and what grew.
		var PAL = ramp([[0, 0, 0], [0, 0, 255], [0, 255, 255], [0, 255, 0], [255, 255, 0], [255, 0, 0]]);
		// transparent -> navy -> blue -> cyan: what shrank.
		var COOL = ramp([[0, 0, 0], [0, 0, 128], [0, 0, 255], [0, 255, 255]]);

		// The on click layers' cells, as after-rate minus before-rate per (selector, x, y):
		// [grown, shrunk], each with n the size of its difference.
		function diffCells() {
			var m = new Map();
			[[hm, 1], [hm.before, -1]].forEach(function (w) {
				['clicks', 'rage', 'dead'].forEach(function (k) {
					if (!layers[k]) return;
					(w[0][k] || []).forEach(function (c) {
						var id = c.selector + ' ' + c.x + ' ' + c.y;
						var e = m.get(id);
						if (!e) { e = { selector: c.selector, x: c.x, y: c.y, n: 0 }; m.set(id, e); }
						e.n += w[1] * c.n / w[0].views;
					});
				});
			});
			var up = [], down = [];
			m.forEach(function (e) {
				if (e.n > 0) up.push(e);
				else if (e.n < 0) down.push({ selector: e.selector, x: e.x, y: e.y, n: -e.n });
			});
			return [up, down];
		}

		// Returns how many of the cells' hits have no element with a box here.
		// An element's box, or null when it is missing, display:none or
		// detached: a zero box would paint at the top-left.
		function box(sel) {
			var el = resolved.get(sel);
			var r = el && el.getBoundingClientRect();
			return r && r.width && r.height ? r : null;
		}

		function drawHeat(cells, radius, pal) {
			pal = pal || PAL;
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
				var p = pal[a];
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
			badges.textContent = '';
			if (!hm) return;
			if (hm.before && !comparable()) { noteEl.hidden = true; return; }
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
			if (hm.before) {
				var diff = diffCells();
				if (diff[0].length) drawHeat(diff[0], 22);
				if (diff[1].length) drawHeat(diff[1], 22, COOL);
			} else {
				var heat = [];
				if (layers.clicks) heat = heat.concat(hm.clicks || []);
				if (layers.rage) heat = heat.concat(hm.rage || []);
				if (layers.dead) heat = heat.concat(hm.dead || []);
				if (heat.length) drawHeat(heat, 22);
			}
			if (layers.moves) drawHeat(hm.moves || [], 28);
			if (layers.forms) {
				hm.forms.forEach(function (f) {
					var r = box(f.selector);
					if (!r) return;
					var parts = formParts(f);
					var b = document.createElement('div');
					b.className = 'badge';
					b.textContent = f.submit > 0 ? parts[0] : parts.slice(0, 2).join(' \u00b7 ') + (parts[2] ? '\n' + parts[2] : '');
					b.style.left = r.right + 'px';
					b.style.top = r.top + 'px';
					badges.appendChild(b);
				});
			}
			// Counted from one layer: a rage or dead click is a click too.
			var lost = 0;
			var layer = layers.clicks ? 'clicks' : layers.rage ? 'rage' : layers.dead ? 'dead' : '';
			((layer && hm[layer]) || []).forEach(function (c) {
				if (!box(c.selector)) lost += c.n;
			});
			noteEl.hidden = !lost;
			noteEl.textContent = lost + (layer === 'clicks' ? ' clicks' : layer === 'rage' ? ' rage clicks' : ' dead clicks') +
				' landed on elements not on this page now.';
		}

		function schedule() {
			if (!raf) { raf = true; requestAnimationFrame(draw); }
		}

		function fmt(n) {
			return n.toLocaleString('en-US');
		}

		// n of the views (hm.views by default), as a whole percent; '<1%' for anything above 0 under 1.
		function share(n, views) {
			if (views == null) views = hm.views;
			var p = views > 0 ? n / views * 100 : 0;
			return p > 0 && p < 1 ? '<1%' : Math.round(p) + '%';
		}

		// A form's line, or a field's focus, fill and (when any) left-here parts;
		// while comparing, each number reads before -> after.
		function formParts(f) {
			var b = hm.before && (beforeForms.get(f.selector) || { focus: 0, fill: 0, submit: 0, left: 0 });
			function s(k) { return b ? share(b[k], hm.before.views) + ' -> ' + share(f[k]) : share(f[k]); }
			if (f.submit > 0) return ['submitted ' + s('submit')];
			var parts = ['focus ' + s('focus'), 'fill ' + s('fill')];
			if (f.left > 0) parts.push('left here ' + s('left'));
			return parts;
		}

		// The element's numbers in both windows, before -> after.
		function compareParts() {
			var z = { clicks: 0, rage: 0, dead: 0, hits: 0 };
			var a = stats.get(tipSel) || z, b = beforeStats.get(tipSel) || z;
			var bv = hm.before.views, parts = [];
			if (a.hits || b.hits) {
				var pa = Math.round(a.hits / hm.views * 100), pb = Math.round(b.hits / bv * 100);
				parts.push('CTR ' + pb + '% -> ' + pa + '% (' + (pa > pb ? '+' : '') + (pa - pb) + ' pts)');
			}
			if (a.clicks || b.clicks) parts.push('clicks ' + (b.clicks / bv).toFixed(2) + ' -> ' + (a.clicks / hm.views).toFixed(2) + ' per view');
			if (a.rage || b.rage) parts.push('rage ' + fmt(b.rage) + ' -> ' + fmt(a.rage));
			if (a.dead || b.dead) parts.push('dead ' + fmt(b.dead) + ' -> ' + fmt(a.dead));
			return parts;
		}

		function tipText(y) {
			if (!hm) return '';
			if (hm.before && !comparable()) return '';
			if (layers.scroll) {
				var sc = hm.scroll || [];
				if (!sc[0]) return '';
				var band = Math.floor((scrollY + y) / (docHeight() / 20));
				var bs = (hm.before && hm.before.scroll) || [];
				return (bs[0] ? Math.round((bs[band] || 0) / bs[0] * 100) + '% -> ' : '') +
					Math.round((sc[band] || 0) / sc[0] * 100) + '% of views reached here';
			}
			if (hm.before) {
				var cp = compareParts();
				var cf = formStats.get(tipSel);
				if (cf) cp = cp.concat(formParts(cf));
				return cp.length ? cp.join(' \u00b7 ') : tipEl.closest(INTERACTIVE) ? 'No clicks before or after' : '';
			}
			var st = stats.get(tipSel);
			var parts = [];
			if (st) {
				if (st.hits > 0 && hm.views > 0) parts.push(fmt(st.hits) + ' views clicked', share(st.hits));
				if (st.clicks > 0) parts.push(fmt(st.clicks) + ' clicks');
				if (st.rage > 0) parts.push(fmt(st.rage) + ' rage');
				if (st.dead > 0) parts.push(fmt(st.dead) + ' dead');
			}
			var fe = formStats.get(tipSel);
			if (fe) parts = parts.concat(formParts(fe));
			if (parts.length) return parts.join(' \u00b7 ');
			return tipEl.closest(INTERACTIVE) ? 'No clicks \u00b7 ' + label(range) : '';
		}

		function hideTip() {
			clearTimeout(tipTimer);
			tipTimer = 0;
			pill.hidden = true;
			outline.hidden = true;
		}

		function showTip() {
			tipTimer = 0;
			var text = tipText(tipEv.clientY);
			if (!text) { hideTip(); return; }
			pill.textContent = text;
			pill.hidden = false;
			// The scroll share is the cursor's depth, not the element's.
			outline.hidden = layers.scroll;
			var r = tipEl.getBoundingClientRect();
			outline.style.left = r.left + 'px';
			outline.style.top = r.top + 'px';
			outline.style.width = r.width + 'px';
			outline.style.height = r.height + 'px';
			var x = tipEv.clientX + 14, y = tipEv.clientY + 14;
			if (x + pill.offsetWidth > innerWidth) x = tipEv.clientX - 14 - pill.offsetWidth;
			if (y + pill.offsetHeight > innerHeight) y = tipEv.clientY - 14 - pill.offsetHeight;
			pill.style.left = Math.max(0, x) + 'px';
			pill.style.top = Math.max(0, y) + 'px';
		}

		function hover() {
			tipFrame = 0;
			if (tipEv.target === host) { tipSel = null; hideTip(); return; }
			var el = norm(tipEv.target);
			var s = el ? sel(el) : '';
			if (s !== tipSel) {
				tipSel = s;
				tipEl = el;
				hideTip();
				if (s) tipTimer = setTimeout(showTip, 150);
			} else if (!pill.hidden) {
				showTip();
			}
		}

		function onHover(e) {
			tipEv = e;
			if (!tipFrame) tipFrame = requestAnimationFrame(hover);
		}

		function onScrollTip() {
			tipSel = null;
			hideTip();
		}

		function onResize() {
			sizeCanvas();
			if (pos) place(pos.x, pos.y);
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
			removeEventListener('scroll', onScrollTip, { capture: true });
			document.removeEventListener('mousemove', onHover, { capture: true });
			clearTimeout(tipTimer);
			cancelAnimationFrame(tipFrame);
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
		// Dragged by its header: the map is read against the page under it, and the corner
		// the panel opens in may be the part worth looking at. Kept inside the viewport, so
		// narrowing the window to reach the phone map can never strand it off screen.
		function place(x, y) {
			pos = {
				x: Math.max(0, Math.min(x, innerWidth - bar.offsetWidth)),
				y: Math.max(0, Math.min(y, innerHeight - bar.offsetHeight))
			};
			bar.style.left = pos.x + 'px';
			bar.style.top = pos.y + 'px';
			bar.style.right = 'auto';
		}
		head.addEventListener('pointerdown', function (e) {
			if (e.target.closest('button')) return;
			var r = bar.getBoundingClientRect();
			drag = { dx: e.clientX - r.left, dy: e.clientY - r.top };
			head.setPointerCapture(e.pointerId);
			e.preventDefault();
		});
		head.addEventListener('pointermove', function (e) {
			if (drag) place(e.clientX - drag.dx, e.clientY - drag.dy);
		});
		head.addEventListener('pointerup', function () { drag = null; });
		head.addEventListener('pointercancel', function () { drag = null; });
		select.addEventListener('change', function () {
			range = select.value;
			load();
		});
		segSelect.addEventListener('change', function () {
			segment = segSelect.value;
			load();
		});
		srcSelect.addEventListener('change', function () {
			source = srcSelect.value;
			load();
		});
		cmpSelect.addEventListener('change', function () {
			split = cmpSelect.value;
			select.disabled = !!split;
			load();
		});
		// Capture: an app shell scrolls an inner container, not the window.
		addEventListener('scroll', schedule, { capture: true, passive: true });
		addEventListener('scroll', onScrollTip, { capture: true, passive: true });
		if (matchMedia('(hover: hover)').matches) {
			document.addEventListener('mousemove', onHover, { capture: true, passive: true });
		}
		addEventListener('resize', onResize);
		addEventListener('popstate', onRoute);

		sizeCanvas();
		// On <html>, not <body>: a router that swaps the body keeps the overlay.
		document.documentElement.appendChild(canvas);
		document.documentElement.appendChild(host);
		load();
	};
})();
