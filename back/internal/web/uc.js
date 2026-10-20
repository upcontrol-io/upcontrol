// upcontrol uc.js: the script behind
// <script defer src=".../uc.js" data-key="uc_pub_..."></script>.
// It records page views, interaction heat and the page's named events
// (upcontrol('track', ...), data-uc-event) for the public key in data-key,
// or, opened through a heatmap link (#uc-heatmap=<token>), it records nothing
// and loads overlay.js, which draws the page's heatmap over the page instead. Served verbatim:
// no build, no dependency, ES2017, one IIFE. ASCII only: a page or a CDN that
// serves it without a charset decodes it as Latin-1, so the rest are escapes.
(function () {
	'use strict';

	var me = document.currentScript;
	if (!me) return;
	var base = new URL(me.src).origin;
	var key = me.getAttribute('data-key');
	// What counts as interactive, for norm() and the dead-click check alike.
	// Above the token check: the overlay reads it through norm().
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
		// The site's own upcontrol() calls must not throw while the map is drawn,
		// and upcontrol('source') still hands back an object.
		window.upcontrol = function (c) { return c === 'source' ? {} : undefined; };
		// A visitor never downloads the overlay: only a heatmap link loads it.
		var tag = document.createElement('script');
		tag.src = base + '/w/overlay.js';
		tag.async = true;
		tag.onload = function () {
			window.__ucOverlay(token, {
				base: base, key: key, norm: norm, sel: sel, INTERACTIVE: INTERACTIVE,
				onHistory: onHistory, docHeight: docHeight
			});
		};
		tag.onerror = function () { console.warn('[upcontrol] could not load the heatmap overlay'); };
		(document.head || document.documentElement).appendChild(tag);
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
	var segment = ''; // what the site says about the visitor, for g: the current view's
	var nextSegment = ''; // what the next view adopts; a change mid-view waits for it
	var segWarned = false;
	var SEARCH = ['google.', 'bing.com', 'duckduckgo.com', 'yahoo.', 'yandex.', 'baidu.com', 'ecosia.org', 'search.brave.com'];
	var SOCIAL = ['facebook.com', 'instagram.com', 't.co', 'twitter.com', 'x.com', 'linkedin.com', 'lnkd.in',
		'reddit.com', 'youtube.com', 'tiktok.com', 'pinterest.', 'vk.com', 't.me', 'threads.net', 'bsky.app'];
	var source = sourceOf(); // the page load's, kept across SPA routes; '' is unknown
	var loadQuery = new URLSearchParams(location.search); // the page load's, for upcontrol('source')
	var landing = location.pathname;
	var tracked = 0; // events this page load sent, at most 200
	var trackWarned = false;
	var viewSent = Promise.resolve(); // the latest view beacon

	// Answers the request, so a named event can wait for the view it belongs to.
	function post(json) {
		return fetch(base + '/w?key=' + encodeURIComponent(key), {
			method: 'POST',
			body: json,
			keepalive: true,
			credentials: 'omit'
			// No headers on purpose: a string body is text/plain, which a
			// browser sends without a preflight.
		}).catch(function () {});
	}

	function view(from) {
		segment = nextSegment;
		var body = {
			v: 1, t: 'view', p: path, w: innerWidth,
			r: firstView ? document.referrer : '',
			q: location.search
		};
		if (from) body.f = from;
		mark(body);
		viewSent = post(JSON.stringify(body));
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
		var json = JSON.stringify(body);
		// The server stores an event only for a visitor with a stored page view
		// that day, so an event sent beside the first view would race it and be lost.
		viewSent.then(function () { post(json); });
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

	// The segment to keep: v when valid, '' for a clear, keep (and one
	// warning a page) otherwise. Same id rule as sel().
	function segmentOf(v, keep) {
		if (v === '' || v == null) return '';
		if (typeof v === 'string' && /^[a-z0-9_-]{1,24}$/.test(v) && !/\d{4}/.test(v)) return v;
		if (!segWarned) {
			segWarned = true;
			console.warn('[upcontrol] segment must be 1-24 of a-z, 0-9, _ and -: ' + v);
		}
		return keep;
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
	nextSegment = segmentOf(me.getAttribute('data-segment'), '');
	var queued = (typeof window.upcontrol === 'function' && window.upcontrol.q) || [];
	for (var qi = 0; qi < queued.length; qi++) {
		if (queued[qi][0] === 'segment') nextSegment = segmentOf(queued[qi][1], nextSegment);
	}
	window.upcontrol = function (cmd, arg, fields) {
		if (cmd === 'track') { track(arg, fields); return; }
		if (cmd === 'source') return sourceInfo();
		// From the next view on: this view's heat, depth and engaged time keep
		// the segment the view was sent with.
		if (cmd === 'segment') nextSegment = segmentOf(arg, nextSegment);
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
})();
