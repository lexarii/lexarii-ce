// SPDX-License-Identifier: AGPL-3.0-or-later

/*
 * The machinery behind reader.test.mjs: finding a browser, assembling the
 * Android reader page the way the app does, and waiting on the page without
 * sleeping.
 *
 * The page is android/app/src/main/assets/reader/reader.html with locator.js
 * and reader.js inlined - exactly what ReaderShell.assemble does - plus a
 * stand-in for the app's LexariiBridge that records every call.
 */

import fs from 'node:fs';
import http from 'node:http';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

import { onPath } from '../../scripts/on-path.mjs';

const here = path.dirname(fileURLToPath(import.meta.url));
export const ASSETS = path.resolve(here, '../../../android/app/src/main/assets/reader');
export const FIXTURES = path.join(here, 'fixtures');

export function fixture(name) {
	return fs.readFileSync(path.join(FIXTURES, name), 'utf8');
}

/**
 * The browsers to test in, each with how to launch it, or a reason there are
 * none.
 *
 * Chromium always, because the Android WebView IS Chromium and nothing else
 * says anything about it. Firefox only when asked for (READER_TEST_FIREFOX=1):
 * it catches code that works by accident of one engine, but no phone runs it.
 *
 * Browsers are not vendored. In order, a Chromium is:
 *   READER_TEST_CHROMIUM   the path to an executable - a mistake there fails
 *                          rather than skips, because somebody asked for it;
 *   Playwright's own       `npx playwright-core install --only-shell chromium`, from web/;
 *   one on PATH            chromium, chromium-browser, google-chrome... - by
 *                          absolute path, from absolute PATH entries only
 *                          (scripts/on-path.mjs)
 */
export async function findBrowsers() {
	let pw;
	try {
		pw = await import('playwright-core');
	} catch {
		return { skip: 'playwright-core is not installed: run `npm ci` in web/' };
	}

	const found = [];
	const chromium = await launchable(pw.chromium, process.env.READER_TEST_CHROMIUM, [
		'chromium',
		'chromium-browser',
		'google-chrome-stable',
		'google-chrome'
	]);
	if (chromium.error) return { error: chromium.error };
	if (!chromium.launch) {
		return {
			skip:
				'no Chromium found. Set READER_TEST_CHROMIUM to a Chromium or Chrome executable, ' +
				'or run `npx playwright-core install --only-shell chromium` in web/'
		};
	}
	found.push({ name: 'chromium', launch: chromium.launch, touch: true });

	if (/^(1|true|yes)$/i.test(process.env.READER_TEST_FIREFOX ?? '')) {
		// Playwright drives its own patched Firefox, not a stock one, so there
		// is no PATH fallback here.
		const firefox = await launchable(pw.firefox, process.env.READER_TEST_FIREFOX_PATH, []);
		if (firefox.error) return { error: firefox.error };
		if (!firefox.launch) {
			return {
				error:
					'READER_TEST_FIREFOX is set but no Playwright Firefox was found: run ' +
					'`npx playwright-core install firefox` in web/, or set READER_TEST_FIREFOX_PATH'
			};
		}
		found.push({ name: 'firefox', launch: firefox.launch, touch: false });
	}
	return { browsers: found };
}

/** How to launch `type`, or {} when there is nothing to launch. */
async function launchable(type, explicit, names) {
	const tryLaunch = async (executablePath) => {
		const options = executablePath ? { executablePath } : {};
		const browser = await type.launch(options);
		await browser.close();
		return () => type.launch(options);
	};

	if (explicit) {
		try {
			return { launch: await tryLaunch(explicit) };
		} catch (err) {
			return { error: `cannot launch ${explicit}: ${firstLine(err)}` };
		}
	}
	try {
		return { launch: await tryLaunch(undefined) };
	} catch (err) {
		// Anything but "not installed" is a real failure and is not hidden.
		if (!/Executable doesn't exist|browserType\.launch: .*not found|install/i.test(String(err))) {
			return { error: `Playwright's ${type.name()} would not start: ${firstLine(err)}` };
		}
	}
	for (const name of names) {
		const exe = onPath(name);
		if (!exe) continue;
		try {
			return { launch: await tryLaunch(exe) };
		} catch {
			/* too old, or not really a Chromium; try the next */
		}
	}
	return {};
}

function firstLine(err) {
	return String(err?.message ?? err).split('\n')[0];
}

/**
 * The reader page as the app assembles it, with a recording bridge.
 *
 * `bridge` lists the bridge methods to provide; leaving `chrome` out stands
 * in for an app from before it existed.
 */
export function readerPage({ theme = 'light', nonce = 'n0nce', lang = 'und', bridge } = {}) {
	const methods = bridge ?? [
		'ready',
		'position',
		'edge',
		'follow',
		'markTapped',
		'unresolved',
		'chrome'
	];
	const stub =
		`<script nonce="${nonce}">\n` +
		'window.calls = [];\n' +
		'window.LexariiBridge = {};\n' +
		`for (const name of ${JSON.stringify(methods)}) {\n` +
		'  window.LexariiBridge[name] = (...args) => { window.calls.push([name, ...args]); };\n' +
		'}\n' +
		'</script>';
	const sub = {
		NONCE: nonce,
		THEME: theme,
		LANG: lang,
		LOCATOR_JS: fs.readFileSync(path.join(ASSETS, 'locator.js'), 'utf8'),
		READER_JS: fs.readFileSync(path.join(ASSETS, 'reader.js'), 'utf8')
	};
	// The scripts' placeholders are JS comments, replaced whole: see the
	// note at the top of reader.html.
	const html = fs
		.readFileSync(path.join(ASSETS, 'reader.html'), 'utf8')
		.replace(
			/\{\{(NONCE|THEME|LANG)\}\}|\/\*\{\{(LOCATOR_JS|READER_JS)\}\}\*\//g,
			(_, value, script) => sub[value ?? script]
		);
	// Ahead of the page's own scripts, so the bridge exists when reader.js
	// looks for it. It carries the nonce, so the page's policy still applies.
	const first = `<script nonce="${nonce}">`;
	return html.replace(first, stub + '\n' + first);
}

/**
 * Serves readerPage(options), where options come from the query string.
 *
 * And pictures, at /img/<anything>?w=&h=&ms=: a grey SVG of that size, sent
 * after that many milliseconds - a chapter's illustrations arriving over a
 * slow link, after the text has been laid out. Same origin, so the page's
 * policy lets them in, and the name is free so a test can defeat the cache.
 */
export function serve() {
	const server = http.createServer((req, res) => {
		const url = new URL(req.url ?? '/', 'http://localhost');
		if (url.pathname.startsWith('/img/')) {
			const w = Number(url.searchParams.get('w') ?? 300);
			const h = Number(url.searchParams.get('h') ?? 300);
			setTimeout(() => {
				res.writeHead(200, { 'content-type': 'image/svg+xml' });
				res.end(
					`<svg xmlns="http://www.w3.org/2000/svg" width="${w}" height="${h}">` +
						`<rect width="${w}" height="${h}" fill="#bbb"/></svg>`
				);
			}, Number(url.searchParams.get('ms') ?? 0));
			return;
		}
		const bridge = url.searchParams.get('bridge');
		res.writeHead(200, { 'content-type': 'text/html; charset=utf-8' });
		res.end(
			readerPage({
				theme: url.searchParams.get('theme') ?? 'light',
				lang: url.searchParams.get('lang') ?? 'und',
				bridge: bridge === null ? undefined : bridge.split(',').filter(Boolean)
			})
		);
	});
	return new Promise((resolve) => {
		server.listen(0, '127.0.0.1', () => {
			const { port } = /** @type {import('node:net').AddressInfo} */ (server.address());
			resolve({ url: `http://127.0.0.1:${port}/reader.html`, close: () => server.close() });
		});
	});
}

/**
 * Helpers installed in the page. Plain functions on window, so a test's
 * evaluate() can use them without shipping them every time.
 */
export function installHelpers() {
	const vp = () => document.getElementById('viewport');
	const flow = document.getElementById('flow');
	const gap = () => Math.max(44, Number.parseFloat(getComputedStyle(vp()).paddingLeft) * 2);
	// Unrounded, as reader.js measures it: see pageWidth() there.
	const width = () => flow.getBoundingClientRect().width;
	const advance = () => width() + gap();
	Object.assign(window, {
		flow,
		advance,
		pageIdx() {
			const m = /translateX\((-?[\d.]+)px\)/.exec(flow.style.transform || '');
			// + 0 turns the -0 of translateX(0px) into 0.
			return m ? Math.round(-Number(m[1]) / advance()) + 0 : 0;
		},
		pageCount() {
			return Math.round((flow.scrollWidth + gap()) / advance());
		},
		marksOf(id) {
			return [...flow.querySelectorAll(`mark.sm-highlight[data-annotation="${id}"]`)];
		},
		textOf(id) {
			return window
				.marksOf(id)
				.map((m) => m.textContent)
				.join('');
		},
		blockText(p) {
			return Locator.elementAtPath(p, flow).textContent;
		},
		/** The path of the first block on every page, in order. */
		firstVisibleByPage() {
			const out = [];
			for (let i = 0; i < window.pageCount(); i++) {
				const el = Locator.firstVisibleElement(flow, i * advance(), width());
				out.push(el ? Locator.elementPath(el, flow) : null);
			}
			return out;
		},
		/** Whether the element at `p` starts inside the page on screen. */
		onScreen(p) {
			const el = Locator.elementAtPath(p, flow);
			if (!el) return false;
			const box = vp().getBoundingClientRect();
			const left = el.getClientRects()[0]?.left ?? el.getBoundingClientRect().left;
			return left >= box.left - 1 && left < box.right;
		},
		/** Every client rect of the element at `p`, as {left, top} pairs, that is on screen. */
		visibleRects(p) {
			const el = Locator.elementAtPath(p, flow);
			const box = vp().getBoundingClientRect();
			return [...el.getClientRects()].filter(
				(r) => r.left >= box.left - 1 && r.right <= box.right + 1
			);
		},
		/** Laid out for the current width: measure() has run since the last resize. */
		laidOut() {
			return Math.abs(Number.parseFloat(flow.style.columnWidth) - width()) < 0.01;
		},
		textNodes(el) {
			const out = [];
			const w = document.createTreeWalker(el, NodeFilter.SHOW_TEXT);
			for (let n = w.nextNode(); n; n = w.nextNode()) out.push(n);
			return out;
		},
		select(sc, so, ec, eo) {
			const r = document.createRange();
			r.setStart(sc, so);
			r.setEnd(ec, eo);
			const sel = getSelection();
			sel.removeAllRanges();
			sel.addRange(r);
			return sel.toString();
		},
		/** A few words of the first block on screen, selected. */
		selectSome() {
			const el = Locator.firstVisibleElement(flow, window.pageIdx() * advance(), width());
			const t = document.createTreeWalker(el, NodeFilter.SHOW_TEXT).nextNode();
			return window.select(t, 1, t, Math.min(t.length, 9));
		},
		lastCall(name) {
			return window.calls.filter((c) => c[0] === name).at(-1) ?? null;
		},
		callsOf(name) {
			return window.calls.filter((c) => c[0] === name);
		},
		frames(n) {
			return new Promise((resolve) => {
				const step = () => (n-- <= 0 ? resolve() : requestAnimationFrame(step));
				step();
			});
		}
	});
}

/**
 * Shows a chapter and waits until it is laid out.
 *
 * Not a sleep. show() lays the chapter out in its second animation frame, and
 * frame callbacks run in the order they were asked for - so three frames
 * requested after the call end after show()'s have, however slow the machine.
 * Unless the chapter has pictures still loading above its target: show() then
 * waits for them, and so must the test.
 */
export async function show(page, html, spine, count, target) {
	await page.evaluate(() => {
		window.calls.length = 0;
	});
	await page.evaluate(
		async ([h, s, c, t]) => {
			Reader.show(h, s, c, t);
			await window.frames(3);
		},
		[html, spine, count, target]
	);
	await page.waitForFunction(() => window.laidOut());
}

/** Parses a computed `rgb()`/`rgba()` colour into [r, g, b]. */
export function rgb(css) {
	const m = /rgba?\(([\d.]+),\s*([\d.]+),\s*([\d.]+)/.exec(css);
	if (!m) throw new Error(`not a colour: ${css}`);
	return [Number(m[1]), Number(m[2]), Number(m[3])];
}

function luminance([r, g, b]) {
	const lin = (c) => {
		const v = c / 255;
		return v <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4;
	};
	return 0.2126 * lin(r) + 0.7152 * lin(g) + 0.0722 * lin(b);
}

/** WCAG 2 contrast ratio between two colours. */
export function contrast(a, b) {
	const [hi, lo] = [luminance(a), luminance(b)].sort((x, y) => y - x);
	return (hi + 0.05) / (lo + 0.05);
}

function lab([r, g, b]) {
	const lin = (c) => {
		const v = c / 255;
		return v <= 0.04045 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4;
	};
	const [R, G, B] = [lin(r), lin(g), lin(b)];
	const x = (0.4124 * R + 0.3576 * G + 0.1805 * B) / 0.95047;
	const y = 0.2126 * R + 0.7152 * G + 0.0722 * B;
	const z = (0.0193 * R + 0.1192 * G + 0.9505 * B) / 1.08883;
	const f = (t) => (t > 0.008856 ? Math.cbrt(t) : 7.787 * t + 16 / 116);
	return [116 * f(y) - 16, 500 * (f(x) - f(y)), 200 * (f(y) - f(z))];
}

/**
 * CIE76 colour difference. Contrast ratio measures lightness only, and a
 * saturated yellow on cream differs from it in hue far more than in
 * lightness - so whether a fill is VISIBLE is asked this way instead.
 */
export function deltaE(a, b) {
	const [p, q] = [lab(a), lab(b)];
	return Math.hypot(p[0] - q[0], p[1] - q[1], p[2] - q[2]);
}
