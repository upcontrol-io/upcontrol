// upcontrol uc.js: the script behind
// <script defer src=".../uc.js" data-key="uc_pub_..."></script>.
// It records page views, interaction heat and the page's named events
// (upcontrol('track', ...), data-uc-event) for the public key in data-key,
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
	// What counts as interactive, for norm() and the dead-click check alike.
	// Above the overlay's start, which reads it through norm().
	var INTERACTIVE = 'a,button,input,select,textarea,label,summary,[role=button],[role=link]';

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
	if (token) {
		// The site's own upcontrol() calls must not throw while the map is drawn.
		window.upcontrol = function () {};
		overlay(token);
		return;
	}

	// --- collector ---------------------------------------------------------

	var path = location.pathname;
	var clicks = new Map(); // cell key -> [sel, fx, fy, clicks, rage]
	var moves = new Map(); // cell key -> [sel, fx, fy, samples]
	var recent = []; // the last 700 ms of clicks, for rage detection
	var maxReach = 0;
	var scrollSent = false;
	var firstView = true;
	var dead = new Map(); // cell key -> [sel, fx, fy, clicks]
	var check = null; // the one dead-click candidate being watched
	var hitSeen = new Set(); // selectors this view clicked
	var hitNew = []; // of those, the ones not yet reported
	var formSeen = new Set(); // 'kind selector' this view reported
	var formNew = []; // of those, the [kind, selector] pairs not yet reported
	var formLast = new Map(); // form -> the last field focused in it, this view
	var formDone = new Set(); // forms this view submitted
	var visible = document.visibilityState === 'visible';
	var lastInput = performance.now(); // the page load counts as input
	var engaged = 0; // engaged ms not yet reported
	var viewEngaged = 0; // engaged ms this view, for the cap and the thresholds
	var THRESHOLDS = [10000, 30000, 120000, 300000];
	var aSent = 0; // thresholds this view already reported
	var segment = ''; // what the site says about the visitor, for g
	var segWarned = false;
	var SEARCH = ['google.', 'bing.com', 'duckduckgo.com', 'yahoo.', 'yandex.', 'baidu.com', 'ecosia.org', 'search.brave.com'];
	var SOCIAL = ['facebook.com', 'instagram.com', 't.co', 'twitter.com', 'x.com', 'linkedin.com', 'lnkd.in',
		'reddit.com', 'youtube.com', 'tiktok.com', 'pinterest.', 'vk.com', 't.me', 'threads.net', 'bsky.app'];
	var source = sourceOf(); // the page load's, kept across SPA routes; '' is unknown
	var loadQuery = new URLSearchParams(location.search); // the page load's, for upcontrol('source')
	var landing = location.pathname;
	var tracked = 0; // events this page load sent, at most 200
	var trackWarned = false;

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

	function view(from) {
		var body = {
			v: 1, t: 'view', p: path, w: innerWidth,
			r: firstView ? document.referrer : '',
			q: location.search
		};
		if (from) body.f = from;
		mark(body);
		post(JSON.stringify(body));
		firstView = false;
	}

	function mark(body) {
		if (segment) body.g = segment;
		if (source) body.o = source;
	}

	// A named event: sent at once, never batched with the heat. Same name and
	// field rules as the server's, so a refused event is never sent.
	function track(name, fields) {
		if (typeof name === 'string') name = name.trim();
		if (typeof name !== 'string' || !name || name.length > 40 || name.slice(0, 3).toLowerCase() === 'uc.') {
			if (!trackWarned) {
				trackWarned = true;
				console.warn('[upcontrol] track needs an event name of 1-40 characters, not starting uc.');
			}
			return;
		}
		if (tracked >= 200) return;
		tracked++;
		var body = { v: 1, t: 'event', p: path, w: innerWidth, n: name };
		if (Object.prototype.toString.call(fields) === '[object Object]') {
			var l = {}, kept = 0, keys = Object.keys(fields);
			for (var i = 0; i < keys.length && kept < 10; i++) {
				var k = keys[i], v = fields[k], tv = typeof v;
				if (!k || k.length > 60 || k.slice(0, 3).toLowerCase() === 'uc.') continue;
				if (tv !== 'string' && tv !== 'number' && tv !== 'boolean') continue;
				l[k] = String(v).slice(0, 100);
				kept++;
			}
			if (kept) body.l = l;
		}
		mark(body);
		post(JSON.stringify(body));
	}

	// The nearest element carrying data-uc-event sends it, its other
	// data-uc-* attributes as the fields.
	function trackNamed(t) {
		var el = t instanceof Element && t.closest('[data-uc-event]');
		if (!el) return;
		var fields = {};
		for (var i = 0; i < el.attributes.length; i++) {
			var a = el.attributes[i].name;
			if (a.indexOf('data-uc-') === 0 && a !== 'data-uc-event') fields[a.slice(8)] = el.attributes[i].value;
		}
		track(el.getAttribute('data-uc-event'), fields);
	}

	// What the page load came from, only the keys it has.
	function sourceInfo() {
		var out = {};
		if (source) out.source = source;
		['utm_source', 'utm_medium', 'utm_campaign'].forEach(function (k) {
			var v = loadQuery.get(k);
			if (v) out[k] = v;
		});
		if (landing) out.landing = landing;
		return out;
	}

	// A host matches an entry when it is the entry or ends in .entry; an
	// entry ending in a dot is a label prefix, so google. covers google.co.uk.
	function listed(host, list) {
		return list.some(function (d) {
			if (d.slice(-1) === '.') return host.indexOf(d) === 0 || host.indexOf('.' + d) > 0;
			return host === d || host.slice(-d.length - 1) === '.' + d;
		});
	}

	// Worked out from this page load alone: nothing is kept on the device.
	function sourceOf() {
		var qs = new URLSearchParams(location.search);
		if (qs.has('utm_source') || qs.has('utm_medium') || qs.has('utm_campaign')) return 'campaign';
		if (!document.referrer) return 'direct';
		var ref;
		try { ref = new URL(document.referrer); } catch (e) { return ''; }
		if (ref.origin === location.origin) return '';
		var host = ref.hostname.toLowerCase().replace(/^www\./, '');
		if (listed(host, SEARCH)) return 'search';
		if (listed(host, SOCIAL)) return 'social';
		return 'referral';
	}

	// The segment to keep: v when valid, '' for a clear, the current one
	// (and one warning a page) otherwise. Same id rule as sel().
	function segmentOf(v) {
		if (v === '' || v == null) return '';
		if (typeof v === 'string' && /^[a-z0-9_-]{1,24}$/.test(v) && !/\d{4}/.test(v)) return v;
		if (!segWarned) {
			segWarned = true;
			console.warn('[upcontrol] segment must be 1-24 of a-z, 0-9, _ and -: ' + v);
		}
		return segment;
	}

	// Engaged: visible, with input in the last 30 s. Every input credits the
	// gap since the previous one, at most 30 s of it.
	function credit() {
		var now = performance.now();
		var add = Math.min(now - lastInput, 30000, 1800000 - viewEngaged);
		lastInput = now;
		if (add > 0) { engaged += add; viewEngaged += add; }
	}
	['pointerdown', 'keydown', 'scroll', 'mousemove', 'touchstart', 'wheel'].forEach(function (t) {
		document.addEventListener(t, function () { if (visible) credit(); }, { capture: true, passive: true });
	});

	// Ends the dead-click watch; with record, a click after which nothing
	// changed becomes a dead cell. Unrecorded is never over-counted.
	function endCheck(record) {
		var w = check;
		if (!w) return;
		check = null;
		clearTimeout(w.timer);
		var quiet = !w.changed && !w.mo.takeRecords().length;
		w.mo.disconnect();
		if (!record || !quiet || location.href !== w.href ||
			String(getSelection() || '') || document.activeElement !== w.active) return;
		var k = w.s + ':' + w.x + ':' + w.y;
		var entry = dead.get(k);
		if (entry) entry[3]++;
		else if (dead.size < 300) dead.set(k, [w.s, w.x, w.y, 1]);
	}

	function watchDead(s, c) {
		var w = { s: s, x: c[0], y: c[1], href: location.href, active: document.activeElement, changed: false };
		w.mo = new MutationObserver(function () { w.changed = true; });
		w.mo.observe(document.documentElement, { childList: true, subtree: true, attributes: true, characterData: true });
		w.timer = setTimeout(function () { endCheck(true); }, 600);
		check = w;
	}

	// The element a position belongs to: the nearest interactive ancestor,
	// or the element holding the svg the click landed in.
	function norm(t) {
		if (!(t instanceof Element)) return null;
		var el = t.closest(INTERACTIVE);
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
		trackNamed(e.target); // a named control pressed by keyboard is still pressed
		if (e.detail === 0) return; // keyboard activation carries no position
		var el = norm(e.target);
		if (!el) return;
		var s = sel(el);
		if (!s) return;
		if (!hitSeen.has(s) && hitSeen.size < 300) { hitSeen.add(s); hitNew.push(s); }
		var c = cell(el, e.clientX, e.clientY);
		if (!c) return;
		// A dead candidate: nothing interactive, not the page itself, and a
		// box small enough to be something a reader meant to press. One watch
		// at a time; rage already counts the repeats.
		if (!check && !e.target.closest(INTERACTIVE) &&
			el !== document.body && el !== document.documentElement) {
			var b = el.getBoundingClientRect();
			if (b.width * b.height < innerWidth * innerHeight / 4) watchDead(s, c);
		}
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

	// Form analytics: which fields a view focused and filled, which form it
	// submitted, and the field it left an unsubmitted form from. Only ever
	// whether a field holds something, never what.
	function field(t) {
		if (!(t instanceof Element) || !t.matches('input,select,textarea')) return null;
		if (t.localName === 'input' && /^(hidden|submit|button|reset|image)$/.test(t.type)) return null;
		return formOf(t) ? t : null;
	}

	function formOf(el) {
		return el.form || el.closest('form');
	}

	function formHit(kind, el) {
		var s = sel(el);
		if (!s) return;
		var k = kind + ' ' + s;
		if (formSeen.has(k) || formSeen.size >= 300) return;
		formSeen.add(k);
		formNew.push([kind, s]);
	}

	function onFill(e) {
		var f = field(e.target);
		if (!f) return;
		if (f.type === 'checkbox' || f.type === 'radio' ? f.checked : f.value !== '') formHit('fill', f);
	}

	document.addEventListener('focusin', function (e) {
		var f = field(e.target);
		if (!f) return;
		formHit('focus', f);
		formLast.set(formOf(f), f);
	}, true);
	document.addEventListener('focusout', onFill, true);
	document.addEventListener('change', onFill, true);
	document.addEventListener('submit', function (e) {
		if (!(e.target instanceof HTMLFormElement)) return;
		formDone.add(e.target);
		formHit('submit', e.target);
	}, true);

	// Abandonment, on a page close or a route change only: a reader who
	// leaves the tab for a code in an email and comes back has not left.
	function leave() {
		formLast.forEach(function (f, form) {
			if (!formDone.has(form)) formHit('left', f);
		});
	}

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
		endCheck(false);
		if (visible) credit();
		var e = Math.floor(engaged);
		var a = [];
		while (aSent < THRESHOLDS.length && viewEngaged >= THRESHOLDS[aSent]) a.push(aSent++);
		if (clicks.size === 0 && moves.size === 0 && dead.size === 0 && !hitNew.length &&
			!formNew.length && scrollSent && !e && !a.length) return;
		var body = { v: 1, t: 'heat', p: path, w: innerWidth };
		mark(body);
		if (!scrollSent) {
			scrollSent = true;
			body.s = Math.round(maxReach * 1000) / 1000;
		}
		if (e) { body.e = e; engaged -= e; }
		if (a.length) body.a = a;
		if (hitNew.length) body.h = hitNew;
		if (formNew.length) body.fm = formNew.splice(0, 100);
		var cArr = [], mArr = [], dArr = [];
		clicks.forEach(function (v) { cArr.push(v); });
		moves.forEach(function (v) { mArr.push(v); });
		dead.forEach(function (v) { dArr.push(v); });
		if (cArr.length) { cArr.sort(function (a, b) { return b[3] - a[3]; }); body.c = cArr; }
		if (mArr.length) { mArr.sort(function (a, b) { return b[3] - a[3]; }); body.m = mArr; }
		if (dArr.length) { dArr.sort(function (a, b) { return b[3] - a[3]; }); body.d = dArr; }
		// The keepalive budget is 64 KB for the whole page, the host's own
		// beacons included; keep to half of it by halving the richest lists
		// first until the body fits.
		function halve(k) {
			var list = body[k];
			if (!list || list.length < 2) return false;
			body[k] = list.slice(0, Math.ceil(list.length / 2));
			return true;
		}
		var json = JSON.stringify(body);
		while (json.length > 30000 && (halve('m') || halve('d') || halve('c') || halve('h') || halve('fm'))) {
			json = JSON.stringify(body);
		}
		post(json);
		clicks.clear();
		moves.clear();
		dead.clear();
		hitNew = [];
	}

	document.addEventListener('visibilitychange', function () {
		if (document.visibilityState === 'hidden') {
			flush();
			visible = false;
		} else {
			visible = true;
			lastInput = performance.now();
		}
	});
	addEventListener('pagehide', function () { leave(); flush(); });

	function onRoute() {
		if (location.pathname === path) return;
		var from = path;
		leave();
		flush();
		path = location.pathname;
		scrollSent = false;
		maxReach = 0;
		engaged = 0;
		viewEngaged = 0;
		aSent = 0;
		hitSeen.clear();
		hitNew = [];
		formSeen.clear();
		formNew = [];
		formLast.clear();
		formDone.clear();
		dead.clear();
		lastInput = performance.now();
		view(from);
	}
	onHistory(onRoute);
	addEventListener('popstate', onRoute);

	var from = '';
	try {
		var ref = new URL(document.referrer);
		if (ref.origin === location.origin) from = ref.pathname;
	} catch (e) {}
	// The tag's data-segment, then what the site queued before this script
	// loaded, in order; from then on upcontrol() is this one.
	segment = segmentOf(me.getAttribute('data-segment'));
	var queued = (typeof window.upcontrol === 'function' && window.upcontrol.q) || [];
	for (var qi = 0; qi < queued.length; qi++) {
		if (queued[qi][0] === 'segment') segment = segmentOf(queued[qi][1]);
	}
	window.upcontrol = function (cmd, arg, fields) {
		if (cmd === 'track') { track(arg, fields); return; }
		if (cmd === 'source') return sourceInfo();
		if (cmd !== 'segment') return;
		var next = segmentOf(arg);
		if (next === segment) return;
		// What was gathered under the old segment is reported under it.
		flush();
		segment = next;
	};
	view(from);
	// Queued events after the first view, so a funnel's page view precedes them.
	for (qi = 0; qi < queued.length; qi++) {
		if (queued[qi][0] === 'track') track(queued[qi][1], queued[qi][2]);
	}

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
		var segment = ''; // the filters, '' for all; kept across loads and routes
		var source = '';
		var layers = { clicks: true, rage: false, dead: false, moves: false, scroll: false, forms: false };
		var hm = null;
		var stats = new Map(); // selector -> { clicks, rage, dead, hits }, sums of n
		var formStats = new Map(); // selector -> its hm.forms entry
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

		function updateStatus() {
			if (!hm) return;
			var noHeat = !(hm.clicks || []).length && !(hm.rage || []).length &&
				!(hm.moves || []).length && !(hm.scroll && hm.scroll[0]);
			if (hm.heatPages != null && hm.views > 0 && noHeat) {
				statusMain('This plan keeps heatmaps for its ' + hm.heatPages + ' busiest pages; this page is not among them.');
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
			fetch(base + '/w/heatmap?token=' + encodeURIComponent(token) +
				'&path=' + encodeURIComponent(location.pathname) +
				'&key=' + encodeURIComponent(key) +
				'&w=' + innerWidth + (range ? '&range=' + range : '') +
				(segment ? '&segment=' + encodeURIComponent(segment) : '') +
				(source ? '&source=' + source : ''), { credentials: 'omit' })
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
					hm.forms = hm.forms || [];
					range = data.range;
					select.value = range;
					fillSegments();
					resolved = new Map();
					['clicks', 'rage', 'dead', 'moves', 'forms'].forEach(function (k) {
						(hm[k] || []).forEach(function (c) { resolved.set(c.selector, null); });
					});
					formStats = new Map();
					hm.forms.forEach(function (f) { formStats.set(f.selector, f); });
					stats = new Map();
					['clicks', 'rage', 'dead', 'hits'].forEach(function (k) {
						(hm[k] || []).forEach(function (c) {
							var st = stats.get(c.selector);
							if (!st) { st = { clicks: 0, rage: 0, dead: 0, hits: 0 }; stats.set(c.selector, st); }
							st[k] += c.n;
						});
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
			badges.textContent = '';
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
			if (layers.dead) heat = heat.concat(hm.dead || []);
			if (heat.length) drawHeat(heat, 22);
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

		// n of the views, as a whole percent; '<1%' for anything above 0 under 1.
		function share(n) {
			var p = hm.views > 0 ? n / hm.views * 100 : 0;
			return p > 0 && p < 1 ? '<1%' : Math.round(p) + '%';
		}

		// A form's line, or a field's focus, fill and (when any) left-here parts.
		function formParts(f) {
			if (f.submit > 0) return ['submitted ' + share(f.submit)];
			var parts = ['focus ' + share(f.focus), 'fill ' + share(f.fill)];
			if (f.left > 0) parts.push('left here ' + share(f.left));
			return parts;
		}

		function tipText(y) {
			if (!hm) return '';
			if (layers.scroll) {
				var sc = hm.scroll || [];
				if (!sc[0]) return '';
				var band = Math.floor((scrollY + y) / (docHeight() / 20));
				return Math.round((sc[band] || 0) / sc[0] * 100) + '% of views reached here';
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
	}
})();
