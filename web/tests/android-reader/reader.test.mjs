// SPDX-License-Identifier: AGPL-3.0-or-later

/*
 * The Android reader page, driven in a real browser.
 *
 * android/app/src/main/assets/reader/ is a WebView page, and the JVM tests
 * cannot reach anything that happens inside it: pagination, painting marks,
 * turning a selection into locators, taps and swipes, typography. This drives
 * that page headless in Chromium - the engine the WebView is - assembled
 * exactly as the app assembles it, with the app's bridge replaced by one that
 * records every call.
 *
 *   make android-reader-test                     Chromium
 *   READER_TEST_FIREFOX=1 make android-reader-test   and Firefox
 *
 * With no browser available it says so and exits 0, the way the Android
 * targets do without an SDK - unless READER_TEST_REQUIRE=1, for CI, where a
 * check that silently checks nothing is worse than a red one. See harness.mjs
 * for where it looks.
 *
 * READER_TEST_SHOTS=<dir> saves a screenshot of every theme there.
 * READER_TEST_COVERAGE=<file> appends the page scripts' line coverage to that
 * LCOV file (Chromium only); `npm run test:coverage` sets it.
 */

import { after, before, describe, it } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';

import { coverage } from './coverage.mjs';
import {
	contrast,
	deltaE,
	findBrowsers,
	fixture,
	installHelpers,
	rgb,
	serve,
	show
} from './harness.mjs';

const found = await findBrowsers();
if (found.error) {
	console.error(`FAIL android-reader-test: ${found.error}`);
	process.exit(1);
}
if (found.skip) {
	if (/^(1|true|yes)$/i.test(process.env.READER_TEST_REQUIRE ?? '')) {
		console.error(`FAIL android-reader-test: ${found.skip} (READER_TEST_REQUIRE is set)`);
		process.exit(1);
	}
	console.log(`SKIP android-reader-test: ${found.skip}`);
	process.exit(0);
}

const LONG = fixture('long.html');
const WRAPPED = fixture('wrapped.html');
const ZONES = fixture('zones.html');
const SHOTS = process.env.READER_TEST_SHOTS;
/** Set by `npm run test:coverage`: the LCOV file to add the page's scripts to. */
const COVER = coverage(process.env.READER_TEST_COVERAGE);
after(() => COVER?.write());

/** Marks as the app sends them. Chapter 2 is LONG. */
const MARKS = [
	{
		id: 'A',
		start: 'epub:2/p[3]:5',
		end: 'epub:2/p[3]:40',
		colour: null,
		shared: false,
		hasNote: false
	},
	{
		id: 'B',
		start: 'epub:2/p[5]:10',
		end: 'epub:2/p[6]:20',
		colour: 'green',
		shared: true,
		hasNote: false
	},
	// Starts before the <em> and ends inside it: crosses an element boundary.
	{
		id: 'C',
		start: 'epub:2/p[8]:30',
		end: 'epub:2/p[8]:130',
		colour: 'yellow',
		shared: false,
		hasNote: true
	},
	{
		id: 'D',
		start: 'epub:3/p[3]:5',
		end: 'epub:3/p[3]:40',
		colour: null,
		shared: false,
		hasNote: false
	},
	{
		id: 'E',
		start: 'epub:2/p[999]:0',
		end: 'epub:2/p[999]:10',
		colour: null,
		selectedText: 'gone',
		shared: false,
		hasNote: true
	},
	{
		id: 'F',
		start: 'epub:2/p[2]:0',
		end: 'epub:3/p[1]:10',
		colour: null,
		shared: false,
		hasNote: false
	},
	{ id: 'G', start: 'garbage', end: 'garbage', colour: null, shared: false, hasNote: false },
	// Overlaps A.
	{
		id: 'H',
		start: 'epub:2/p[3]:30',
		end: 'epub:2/p[3]:60',
		colour: 'blue',
		shared: false,
		hasNote: false
	}
];

const THEMES = {
	light: [251, 248, 241],
	sepia: [244, 236, 216],
	dark: [22, 20, 15],
	black: [0, 0, 0]
};

for (const engine of found.browsers) {
	describe(engine.name, () => {
		let browser;
		let server;

		before(async () => {
			browser = await engine.launch();
			server = await serve();
		});
		after(async () => {
			await browser?.close();
			server?.close();
		});

		/** A fresh reader page, as a phone held upright. */
		async function open({ theme, lang, bridge, reducedMotion, width = 380, height = 720 } = {}) {
			const context = await browser.newContext({
				viewport: { width, height },
				deviceScaleFactor: 2,
				reducedMotion: reducedMotion ?? 'no-preference',
				...(engine.touch ? { hasTouch: true, isMobile: true } : {})
			});
			const page = await context.newPage();
			await COVER?.track(context, page);
			page.setDefaultTimeout(5000);
			const errors = [];
			page.on('pageerror', (e) => errors.push(String(e)));
			page.on('console', (m) => {
				if (m.type() === 'error') errors.push(m.text());
			});
			const query = new URLSearchParams();
			if (theme) query.set('theme', theme);
			if (lang) query.set('lang', lang);
			if (bridge) query.set('bridge', bridge.join(','));
			await page.goto(`${server.url}?${query}`);
			await page.evaluate(installHelpers);
			return { page, context, errors };
		}

		/** A tap the way this engine gets one: a finger in Chromium, a mouse in Firefox. */
		const tap = (page, x, y) =>
			engine.touch ? page.touchscreen.tap(x, y) : page.mouse.click(x, y);

		/** A point inside the page's text area, by thirds: 0 left, 1 centre, 2 right. */
		const zone = (page, third) =>
			page.evaluate((t) => {
				const box = document.getElementById('viewport').getBoundingClientRect();
				const x = { 0: 60, 1: window.innerWidth / 2, 2: box.right - 40 }[t];
				return { x, y: box.bottom - 60 };
			}, third);

		describe('loading', () => {
			it('loads under its content policy with no errors, and says it is ready', async () => {
				const { page, context, errors } = await open();
				assert.deepEqual(errors, []);
				assert.ok(await page.evaluate(() => window.lastCall('ready')));
				await context.close();
			});

			it('draws its first frame in the theme the app baked in', async () => {
				const { page, context } = await open({ theme: 'sepia' });
				const bg = await page.evaluate(() => getComputedStyle(document.body).backgroundColor);
				assert.deepEqual(rgb(bg), THEMES.sepia);
				assert.equal((await page.evaluate(() => Reader.setTypography({}))).theme, 'sepia');
				await context.close();
			});

			// What a screen reader announces the page as, and the voice it reads
			// the text in. The app fills in "und" when it does not know.
			it('has a title, and the language the app baked in', async () => {
				for (const lang of ['und', 'fr']) {
					const { page, context } = await open({ lang });
					assert.equal(await page.title(), 'Lexarii');
					assert.equal(await page.evaluate(() => document.documentElement.lang), lang);
					await context.close();
				}
			});

			it('falls back to light for a baked-in theme it does not know', async () => {
				const { page, context } = await open({ theme: 'mauve' });
				const bg = await page.evaluate(() => getComputedStyle(document.body).backgroundColor);
				assert.deepEqual(rgb(bg), THEMES.light);
				assert.equal((await page.evaluate(() => Reader.setTypography({}))).theme, 'light');
				await context.close();
			});
		});

		describe('marks', () => {
			let page;
			let context;
			let unpainted;
			let painted;

			before(async () => {
				({ page, context } = await open());
				await show(page, LONG, 2, 5, null);
				unpainted = await page.evaluate(() => ({
					pages: firstVisibleByPage(),
					sw: flow.scrollWidth
				}));
				await page.evaluate(() => {
					calls.length = 0;
				});
				await page.evaluate((m) => Reader.setMarks(m), MARKS);
				painted = await page.evaluate(() => ({
					A: [textOf('A'), blockText('p[3]').slice(5, 40)],
					// With the line break between the two paragraphs in the
					// fixture, which lies inside the range and is painted too.
					B: [textOf('B'), blockText('p[5]').slice(10) + '\n' + blockText('p[6]').slice(0, 20)],
					C: [textOf('C'), blockText('p[8]').slice(30, 130)],
					cPieces: marksOf('C').length,
					H: [textOf('H'), blockText('p[3]').slice(30, 60)],
					D: marksOf('D').length,
					bShared: marksOf('B').every((m) => m.hasAttribute('data-shared')),
					aShared: marksOf('A').some((m) => m.hasAttribute('data-shared')),
					cNote: marksOf('C').map((m) => m.hasAttribute('data-note')),
					cColour: marksOf('C')[0].dataset.colour,
					aColour: marksOf('A')[0].dataset.colour,
					unresolved: callsOf('unresolved'),
					pages: firstVisibleByPage(),
					sw: flow.scrollWidth
				}));
			});
			after(() => context?.close());

			it('paginates the chapter into several pages', () => {
				assert.ok(unpainted.pages.length > 4, `${unpainted.pages.length} pages`);
			});
			it('paints a mark over exactly its text', () => assert.equal(...painted.A));
			it('paints a mark across two paragraphs', () => assert.equal(...painted.B));
			it('paints a mark across an inline element boundary, in pieces', () => {
				assert.equal(...painted.C);
				assert.ok(painted.cPieces >= 2);
			});
			it('paints an overlapping mark', () => assert.equal(...painted.H));
			it("ignores another chapter's marks", () => assert.equal(painted.D, 0));
			it('flags every piece of a shared mark, and only those', () => {
				assert.ok(painted.bShared && !painted.aShared);
			});
			it('puts the note flag on the last piece only', () => {
				assert.ok(painted.cNote.length >= 2);
				assert.deepEqual(painted.cNote, [...painted.cNote.slice(0, -1).map(() => false), true]);
			});
			it('carries a colour, and leaves a mark with none bare', () => {
				assert.equal(painted.cColour, 'yellow');
				assert.equal(painted.aColour, undefined);
			});
			it('reports the marks it cannot place, once, naming the chapter', () => {
				assert.deepEqual(painted.unresolved, [['unresolved', '["E","F"]', 2]]);
			});
			it('moves no page boundary by painting', () => {
				assert.deepEqual(painted.pages, unpainted.pages);
				assert.equal(painted.sw, unpainted.sw);
			});

			it('repaints without wrapping marks in marks', async () => {
				await page.evaluate(() => {
					calls.length = 0;
				});
				await page.evaluate((m) => Reader.setMarks(m), MARKS);
				const r = await page.evaluate(() => ({
					nested: flow.querySelectorAll('mark.sm-highlight mark.sm-highlight').length,
					// Only H, overlapping A, may sit inside another mark.
					hInside: marksOf('H').filter((m) => m.parentElement.matches('mark.sm-highlight')).length,
					unresolved: callsOf('unresolved').length
				}));
				assert.equal(r.nested, r.hInside);
				assert.equal(r.unresolved, 0, 'the same list is not reported twice for one chapter');
			});

			it('reports the list emptying once, then stays quiet', async () => {
				const fixed = MARKS.filter((m) => m.id !== 'E' && m.id !== 'F');
				await page.evaluate(() => {
					calls.length = 0;
				});
				await page.evaluate((m) => Reader.setMarks(m), fixed);
				assert.deepEqual(await page.evaluate(() => callsOf('unresolved')), [
					['unresolved', '[]', 2]
				]);
				await page.evaluate(() => {
					calls.length = 0;
				});
				await page.evaluate((m) => Reader.setMarks(m), fixed);
				assert.deepEqual(await page.evaluate(() => callsOf('unresolved')), []);
			});

			it('reports again when a repaint changes the list', async () => {
				await page.evaluate(() => {
					calls.length = 0;
				});
				await page.evaluate((m) => Reader.setMarks(m), MARKS);
				assert.deepEqual(await page.evaluate(() => callsOf('unresolved')), [
					['unresolved', '["E","F"]', 2]
				]);
				await page.evaluate(() => {
					calls.length = 0;
				});
				await page.evaluate(
					(m) => Reader.setMarks(m),
					MARKS.filter((m) => m.id !== 'E')
				);
				assert.deepEqual(await page.evaluate(() => callsOf('unresolved')), [
					['unresolved', '["F"]', 2]
				]);
				await page.evaluate((m) => Reader.setMarks(m), MARKS);
			});

			it('stays quiet through a resize or a type change that changes nothing', async () => {
				await page.evaluate(() => {
					calls.length = 0;
				});
				await page.setViewportSize({ width: 720, height: 380 });
				await page.waitForFunction(() => laidOut());
				await page.setViewportSize({ width: 380, height: 720 });
				await page.waitForFunction(() => laidOut());
				await page.evaluate(() => {
					Reader.setTypography({ fontScale: 1.3 });
					Reader.setTypography({ fontScale: 1 });
				});
				assert.deepEqual(await page.evaluate(() => callsOf('unresolved')), []);
				// And the marks were repainted all the same.
				assert.ok(await page.evaluate(() => marksOf('A').length > 0));
			});

			it("paints the new chapter's marks when the chapter changes", async () => {
				await show(page, LONG, 3, 5, null);
				const r = await page.evaluate(() => ({
					d: [textOf('D'), blockText('p[3]').slice(5, 40)],
					a: marksOf('A').length,
					unresolved: callsOf('unresolved')
				}));
				assert.equal(...r.d);
				assert.equal(r.a, 0);
				assert.deepEqual(r.unresolved, [['unresolved', '[]', 3]]);
			});

			it('reports on every show, empty or not, naming the chapter', async () => {
				await show(page, LONG, 2, 5, { path: 'p[20]' });
				assert.deepEqual(await page.evaluate(() => callsOf('unresolved')), [
					['unresolved', '["E","F"]', 2]
				]);
				await show(page, LONG, 3, 5, null);
				assert.deepEqual(await page.evaluate(() => callsOf('unresolved')), [
					['unresolved', '[]', 3]
				]);
				// A clean chapter after a clean chapter still says so: it is how
				// the app learns that a mark it had lost there is found again.
				await show(page, LONG, 4, 5, null);
				assert.deepEqual(await page.evaluate(() => callsOf('unresolved')), [
					['unresolved', '[]', 4]
				]);
				// And the same chapter shown again reports again.
				await show(page, LONG, 4, 5, null);
				assert.deepEqual(await page.evaluate(() => callsOf('unresolved')), [
					['unresolved', '[]', 4]
				]);
			});

			it('reports once per show, after its own paint, however the marks arrive', async () => {
				await page.evaluate(() => {
					calls.length = 0;
				});
				// Marks sent between show() and its layout, and a show()
				// overtaken by another before it laid out: one report, for the
				// chapter that was finally shown.
				const r = await page.evaluate(
					async ([html, m]) => {
						Reader.show(html, 3, 5, null);
						Reader.setMarks(m);
						Reader.show(html, 2, 5, null);
						Reader.setMarks(m);
						await frames(3);
						return callsOf('unresolved');
					},
					[LONG, MARKS]
				);
				assert.deepEqual(r, [['unresolved', '["E","F"]', 2]]);
			});

			it('reports once for a show that a debounced resize lands in the middle of', async () => {
				await show(page, LONG, 3, 5, null);
				const r = await page.evaluate(async (html) => {
					calls.length = 0;
					// Hold show()'s frames - as a WebView off screen does - until
					// the resize, debounced by 100ms, has had time to fire.
					const raf = window.requestAnimationFrame;
					const held = [];
					window.requestAnimationFrame = (f) => held.push(f);
					window.dispatchEvent(new Event('resize'));
					Reader.show(html, 2, 5, null);
					await new Promise((resolve) => setTimeout(resolve, 250));
					const early = callsOf('unresolved');
					window.requestAnimationFrame = raf;
					// The first frame asks for the second from the real one.
					while (held.length) held.shift()(performance.now());
					await frames(3);
					return { early, all: callsOf('unresolved') };
				}, LONG);
				assert.deepEqual(r.early, [], 'nothing before the chapter is laid out');
				assert.deepEqual(r.all, [['unresolved', '["E","F"]', 2]]);
			});
		});

		describe('position', () => {
			let page;
			let context;

			before(async () => {
				({ page, context } = await open());
				await page.evaluate((m) => Reader.setMarks(m), MARKS);
				await show(page, LONG, 2, 5, { path: 'p[20]' });
			});
			after(() => context?.close());

			it('lands on the target, with marks painted', async () => {
				const r = await page.evaluate(() => ({
					on: onScreen('p[20]'),
					page: pageIdx(),
					a: marksOf('A').length
				}));
				assert.ok(r.on && r.page > 0 && r.a > 0, JSON.stringify(r));
			});

			it('reports, debounced, the first block on the page', async () => {
				await page.waitForFunction(() => lastCall('position'));
				const [call, first] = await page.evaluate(() => [
					lastCall('position'),
					firstVisibleByPage()[pageIdx()]
				]);
				assert.equal(call[1], `epub:2/${first}:0`);
				assert.ok(call[2] > 0.4 && call[2] < 0.6, `overall ${call[2]} for chapter 3 of 5`);
			});

			it('keeps the paragraph it landed on in view through a rotation', async () => {
				await page.evaluate(() => {
					calls.length = 0;
				});
				await page.setViewportSize({ width: 720, height: 380 });
				await page.waitForFunction(() => laidOut());
				const r = await page.evaluate(() => ({
					on: onScreen('p[20]'),
					a: [textOf('A'), blockText('p[3]').slice(5, 40)],
					c: [textOf('C'), blockText('p[8]').slice(30, 130)]
				}));
				assert.ok(r.on, 'p[20] on screen after rotating');
				assert.equal(...r.a);
				assert.equal(...r.c);
			});

			// Sideways, the page is narrower than the screen by the measure, so its
			// width is a fraction of a pixel - as a real phone's often is. Measured
			// with a rounded width, the strip drifted by that fraction every page
			// until, nine pages in, no paragraph counted as on screen at all.
			it('pages without drift when the page width is fractional', async () => {
				const r = await page.evaluate(() => {
					const left = flow.getBoundingClientRect().left;
					const lefts = [...flow.querySelectorAll('p')].map(
						(p) => p.getClientRects()[0].left - left
					);
					return {
						width: flow.getBoundingClientRect().width,
						// How far each paragraph's page is from where the reader
						// thinks pages are, in pixels.
						off: lefts.map((l) => Math.abs(l - Math.round(l / advance()) * advance())),
						pages: firstVisibleByPage(),
						lastStart: Math.round(lefts.at(-1) / advance())
					};
				});
				assert.notEqual(r.width, Math.round(r.width), 'the width is fractional here');
				assert.ok(Math.max(...r.off) < 0.5, `drift of ${Math.max(...r.off).toFixed(2)}px`);
				const empty = r.pages
					.slice(0, r.lastStart + 1)
					.map((p, i) => (p ? null : i))
					.filter((i) => i !== null);
				assert.deepEqual(
					empty,
					[],
					'pages with a paragraph starting on them that none was found on'
				);
			});

			it('reports the new page after a rotation, naming what starts it', async () => {
				await page.waitForFunction(() => lastCall('position'));
				const [call, first] = await page.evaluate(() => [
					lastCall('position'),
					firstVisibleByPage()[pageIdx()]
				]);
				assert.equal(call[1], `epub:2/${first}:0`);
			});

			it('comes back to the same page after rotating there and back', async () => {
				await page.setViewportSize({ width: 380, height: 720 });
				await page.waitForFunction(() => laidOut());
				await show(page, LONG, 2, 5, null);
				await page.evaluate(() => {
					for (let i = 0; i < 5; i++) Reader.turn(1);
				});
				const before = await page.evaluate(() => ({
					page: pageIdx(),
					first: firstVisibleByPage()[pageIdx()]
				}));
				for (const size of [
					{ width: 720, height: 380 },
					{ width: 380, height: 720 },
					{ width: 720, height: 380 },
					{ width: 380, height: 720 }
				]) {
					await page.setViewportSize(size);
					await page.waitForFunction(() => laidOut());
					assert.ok(
						await page.evaluate((p) => onScreen(p), before.first),
						`${before.first} at ${size.width}`
					);
				}
				assert.equal(await page.evaluate(() => pageIdx()), before.page);
			});

			it('keeps the page and paragraph when marks change while reading', async () => {
				await show(page, LONG, 2, 5, { path: 'p[26]' });
				const before = await page.evaluate(() => ({
					page: pageIdx(),
					first: firstVisibleByPage()[pageIdx()]
				}));
				const more = MARKS.concat([
					{
						id: 'I',
						start: 'epub:2/p[10]:0',
						end: 'epub:2/p[25]:10',
						colour: 'pink',
						shared: false,
						hasNote: true
					}
				]);
				await page.evaluate((m) => Reader.setMarks(m), more);
				const after = await page.evaluate(() => ({
					page: pageIdx(),
					first: firstVisibleByPage()[pageIdx()],
					i: marksOf('I').length
				}));
				assert.deepEqual([after.page, after.first], [before.page, before.first]);
				assert.ok(after.i > 5);
				await page.evaluate(() => {
					calls.length = 0;
					Reader.flush();
				});
				assert.equal(
					(await page.evaluate(() => lastCall('position')))[1],
					`epub:2/${before.first}:0`
				);
			});
		});

		// Pictures with no size in their markup - every one in a Project
		// Gutenberg book - take no space until they arrive. A position restored
		// before then landed several pages early, and the text then moved on
		// without the reader as they loaded.
		describe('pictures that arrive after the text', () => {
			/**
			 * LONG with a tall picture in its heading and three more among
			 * the paragraphs, each sent `ms` late. In <div>s, so every <p>
			 * keeps its path.
			 */
			const illustrated = (tag, ms) => {
				let n = 0;
				const img = () => `<img src="/img/${tag}-${n++}.svg?w=300&h=500&ms=${ms}">`;
				let p = 0;
				return LONG.replace('<h2>Chapter</h2>', `<h2>${img()}Chapter</h2>`).replace(
					/<\/p>/g,
					(end) => (++p === 4 || p === 9 || p === 15 ? `${end}<div>${img()}</div>` : end)
				);
			};
			const allLoaded = (page) =>
				page.waitForFunction(() => [...flow.querySelectorAll('img')].every((i) => i.complete));

			it('lands on a saved position only once the pictures above it are in', async () => {
				const { page, context } = await open();
				await show(page, illustrated('land', 300), 2, 5, { path: 'p[20]' });
				await allLoaded(page);
				await page.waitForFunction(() => onScreen('p[20]'), null, { timeout: 2000 });
				// And says so: the position reported is the page it landed on.
				await page.waitForFunction(() => lastCall('position'));
				const [call, first] = await page.evaluate(() => [
					lastCall('position')[1],
					firstVisibleByPage()[pageIdx()]
				]);
				assert.equal(call, `epub:2/${first}:0`);
				await context.close();
			});

			it('keeps the reader on their paragraph when pictures above it arrive late', async () => {
				const { page, context } = await open();
				// Opened at the start, which waits for nothing.
				await show(page, illustrated('late', 600), 2, 5, undefined);
				for (let i = 0; i < 3; i++) await page.evaluate(() => Reader.turn(1));
				const reading = await page.evaluate(() => firstVisibleByPage()[pageIdx()]);
				assert.ok(reading, 'a paragraph on the page turned to');
				await allLoaded(page);
				await page.waitForFunction((p) => onScreen(p), reading, { timeout: 2000 });
				await context.close();
			});
		});

		describe('selection to locators', () => {
			let page;
			let context;
			const roundTrip = () =>
				page.evaluate(() => {
					const got = Reader.highlightSelection();
					if (!got) return { got };
					const s = Locator.parseLocator(got.start);
					const e = Locator.parseLocator(got.end);
					const back = Locator.rangeFromAnchor(
						{
							startPath: s.path,
							startOffset: s.charOffset,
							endPath: e.path,
							endOffset: e.charOffset,
							text: ''
						},
						flow
					);
					return { got, back: back && back.toString(), cleared: getSelection().isCollapsed };
				});
			const squash = (s) => (s ?? '').replace(/\s+/g, ' ').trim();

			before(async () => {
				({ page, context } = await open());
				await page.evaluate((m) => Reader.setMarks(m), MARKS);
				await show(page, LONG, 2, 5, null);
			});
			after(() => context?.close());

			it('turns a selection crossing into an <em> into locators that round-trip', async () => {
				const selected = await page.evaluate(() => {
					const t = textNodes(Locator.elementAtPath('p[12]', flow));
					return select(t[0], 4, t[1], 5);
				});
				const r = await roundTrip();
				assert.equal(r.got.start, 'epub:2/p[12]:4');
				assert.match(r.got.end, /^epub:2\/p\[12\]:\d+$/);
				assert.equal(r.back, selected);
				assert.equal(r.got.selectedText, squash(selected));
				assert.ok(r.cleared, 'selection cleared once it became a mark');
			});

			it('names unpainted text for a selection starting inside a painted mark', async () => {
				const selected = await page.evaluate(() => {
					const inMark = marksOf('A')[0].firstChild;
					return select(inMark, 2, textNodes(Locator.elementAtPath('p[5]', flow))[0], 7);
				});
				const got = await page.evaluate(() => Reader.highlightSelection());
				assert.equal(got.start, 'epub:2/p[3]:7');
				const back = await page.evaluate((g) => {
					Locator.unpaintHighlights(flow);
					const s = Locator.parseLocator(g.start);
					const e = Locator.parseLocator(g.end);
					return Locator.rangeFromAnchor(
						{
							startPath: s.path,
							startOffset: s.charOffset,
							endPath: e.path,
							endOffset: e.charOffset,
							text: ''
						},
						flow
					).toString();
				}, got);
				assert.equal(squash(back), squash(selected));
				await page.evaluate((m) => Reader.setMarks(m), MARKS);
			});

			it('ends a whole-paragraph selection at that paragraph, not the next (the locator.ts fix)', async () => {
				const r = await page.evaluate(() => {
					const p = Locator.elementAtPath('p[14]', flow);
					select(p, 0, Locator.elementAtPath('p[15]', flow), 0);
					// Locator's own answer, with no help from reader.js.
					const raw = Locator.anchorFromSelection(getSelection(), flow);
					return { raw, got: Reader.highlightSelection(), len: p.textContent.length };
				});
				assert.equal(`${r.raw.endPath}:${r.raw.endOffset}`, `p[14]:${r.len}`);
				assert.equal(r.got.start, 'epub:2/p[14]:0');
				assert.equal(r.got.end, `epub:2/p[14]:${r.len}`);
			});

			describe('pretty-printed XHTML', () => {
				before(() => show(page, WRAPPED, 2, 5, null));

				// [name, start, end, expected] - each point is [what, offset],
				// resolved in the page. "ws after a" is the "\n    " between the
				// first two paragraphs, inside the div.
				const cases = [
					['a whole paragraph: (p, 0) to (next p, 0)', ['a', 0], ['b', 0], 'a'],
					['the triple-click form, into the next wrapper', ['text b', 0], ['g', 0], 'b'],
					[
						'a paragraph as (wrapper, i) to (wrapper, i + 1)',
						['div at a', 0],
						['div at a', 1],
						'a'
					],
					['two whole paragraphs across a wrapper boundary', ['a', 0], ['g', 0], 'a b'],
					['mid-paragraph to the start of the next', ['text a', 6], ['b', 0], 'a from 6'],
					['a start in the whitespace between blocks', ['ws after a', 2], ['text b', 9], 'b to 9'],
					[
						'a start in the whitespace before the first block',
						['ws before a', 1],
						['text a', 5],
						'a to 5'
					],
					['an end in the whitespace between blocks', ['text a', 6], ['ws after a', 3], 'a from 6'],
					['an end in text outside any block', ['text g', 0], ['loose', 4], 'g']
				];
				for (const [name, start, end, want] of cases) {
					it(`round-trips ${name}`, async () => {
						const r = await page.evaluate(
							([s, e, w]) => {
								const P = (p) => Locator.elementAtPath(p, flow);
								const a = P('section[1]/div[1]/p[1]');
								const b = P('section[1]/div[1]/p[2]');
								const g = P('section[1]/div[2]/p[1]');
								const div = P('section[1]/div[1]');
								const loose = [...P('section[1]').childNodes].find(
									(n) => n.nodeType === 3 && /loose/.test(n.data)
								);
								const at = ([what, off]) =>
									({
										a: [a, off],
										b: [b, off],
										g: [g, off],
										'text a': [textNodes(a)[0], off],
										'text b': [textNodes(b)[0], off],
										'text g': [textNodes(g)[0], off],
										'div at a': [div, [...div.childNodes].indexOf(a) + off],
										'ws after a': [a.nextSibling, off],
										'ws before a': [a.previousSibling, off],
										loose: [loose, off]
									})[what];
								const expected = {
									a: a.textContent,
									b: b.textContent,
									g: g.textContent,
									'a b': a.textContent + ' ' + b.textContent,
									'a from 6': a.textContent.slice(6),
									'b to 9': b.textContent.slice(0, 9),
									'a to 5': a.textContent.slice(0, 5)
								}[w];
								select(...at(s), ...at(e));
								return { expected };
							},
							[start, end, want]
						);
						const got = await roundTrip();
						assert.ok(got.got, 'a mark');
						assert.equal(squash(got.back), squash(r.expected));
						assert.equal(squash(got.got.selectedText), squash(r.expected));
						assert.ok(got.cleared);
					});
				}

				for (const [name, which, from, to] of [
					['only the whitespace between blocks', 'ws', 0, 4],
					['only text outside any block', 'loose', 3, 12]
				]) {
					it(`refuses ${name}, and leaves the selection alone`, async () => {
						const r = await page.evaluate(
							([w, f, t]) => {
								const P = (p) => Locator.elementAtPath(p, flow);
								const node =
									w === 'ws'
										? P('section[1]/div[1]/p[1]').nextSibling
										: [...P('section[1]').childNodes].find(
												(n) => n.nodeType === 3 && /loose/.test(n.data)
											);
								const before = select(node, f, node, t);
								const got = Reader.highlightSelection();
								const sel = getSelection();
								const range = sel.rangeCount ? sel.getRangeAt(0) : null;
								return {
									got,
									kept:
										!!range &&
										sel.toString() === before &&
										range.startContainer === node &&
										range.startOffset === f &&
										range.endOffset === t
								};
							},
							[which, from, to]
						);
						assert.equal(r.got, null);
						assert.ok(r.kept);
					});
				}

				it('answers null with nothing selected', async () => {
					const got = await page.evaluate(() => {
						getSelection().removeAllRanges();
						return Reader.highlightSelection();
					});
					assert.equal(got, null);
				});
			});
		});

		describe('taps, swipes and keys', () => {
			let page;
			let context;

			before(async () => {
				({ page, context } = await open());
				await page.evaluate((m) => Reader.setMarks(m), MARKS);
			});
			after(() => context?.close());

			const fresh = async (target = null) => {
				await show(page, LONG, 2, 5, target);
				await page.evaluate(() => {
					getSelection().removeAllRanges();
					calls.length = 0;
				});
			};
			/** The middle of the first on-screen piece of a mark. */
			const markPoint = (id) =>
				page.evaluate((m) => {
					const box = document.getElementById('viewport').getBoundingClientRect();
					for (const piece of marksOf(m)) {
						for (const r of piece.getClientRects()) {
							if (r.left >= box.left && r.right <= box.right && r.width > 4) {
								return { x: r.left + r.width / 2, y: r.top + r.height / 2 };
							}
						}
					}
					return null;
				}, id);

			it('turns forward on a tap in the right third', async () => {
				await fresh();
				const { x, y } = await zone(page, 2);
				await tap(page, x, y);
				await page.waitForFunction(() => pageIdx() === 1);
				assert.deepEqual(await page.evaluate(() => callsOf('chrome')), []);
			});

			it('turns back on a tap in the left third', async () => {
				const { x, y } = await zone(page, 0);
				await tap(page, x, y);
				await page.waitForFunction(() => pageIdx() === 0);
			});

			it('asks for the controls on a tap in the centre, and does not turn', async () => {
				await fresh();
				await page.evaluate(() => Reader.turn(1));
				const { x, y } = await zone(page, 1);
				await tap(page, x, y);
				await page.waitForFunction(() => callsOf('chrome').length === 1);
				const r = await page.evaluate(() => ({ page: pageIdx(), chrome: lastCall('chrome') }));
				assert.equal(r.page, 1);
				assert.deepEqual(r.chrome, ['chrome'], 'called with no arguments');
			});

			it('opens a mark on a tap, instead of turning', async () => {
				await fresh();
				const at = await markPoint('A');
				await tap(page, at.x, at.y);
				await page.waitForFunction(() => lastCall('markTapped'));
				const r = await page.evaluate(() => ({ page: pageIdx(), tapped: lastCall('markTapped') }));
				assert.deepEqual(r, { page: 0, tapped: ['markTapped', 'A'] });
			});

			it("opens somebody else's shared mark on a tap", async () => {
				await fresh({ path: 'p[6]' });
				const p = await page.evaluate(() => pageIdx());
				const at = await markPoint('B');
				assert.ok(at, 'B is on screen');
				await tap(page, at.x, at.y);
				await page.waitForFunction(() => lastCall('markTapped'));
				assert.deepEqual(await page.evaluate(() => [pageIdx(), lastCall('markTapped')]), [
					p,
					['markTapped', 'B']
				]);
			});

			it('only clears a live selection on a tap, without turning', async () => {
				await fresh();
				await page.evaluate(() => selectSome());
				const { x, y } = await zone(page, 2);
				await tap(page, x, y);
				await page.waitForFunction(() => getSelection().toString() === '');
				assert.deepEqual(await page.evaluate(() => [pageIdx(), callsOf('edge')]), [0, []]);
			});

			it('only clears a live selection on a tap over a mark, without opening it', async () => {
				await fresh();
				await page.evaluate(() => selectSome());
				const at = await markPoint('A');
				await tap(page, at.x, at.y);
				await page.waitForFunction(() => getSelection().toString() === '');
				assert.deepEqual(await page.evaluate(() => [pageIdx(), callsOf('markTapped')]), [0, []]);
			});

			it('paints marks that arrive during a selection only once it clears', async () => {
				await fresh();
				const selected = await page.evaluate(() => selectSome());
				const extra = MARKS.concat([
					{
						id: 'J',
						start: 'epub:2/p[2]:0',
						end: 'epub:2/p[2]:15',
						colour: null,
						shared: false,
						hasNote: false
					}
				]);
				await page.evaluate((m) => Reader.setMarks(m), extra);
				assert.deepEqual(
					await page.evaluate(() => [marksOf('J').length, getSelection().toString()]),
					[0, selected]
				);
				await page.evaluate(() => getSelection().removeAllRanges());
				await page.waitForFunction(() => marksOf('J').length > 0);
				await page.evaluate((m) => Reader.setMarks(m), MARKS);
			});

			describe('the centre third, where other things win', () => {
				before(async () => {
					await show(page, ZONES, 7, 9, null);
					await page.evaluate(() =>
						Reader.setMarks([
							{
								id: 'Z',
								start: 'epub:7/p[3]:0',
								end: 'epub:7/p[3]:140',
								colour: null,
								shared: false,
								hasNote: false
							}
						])
					);
				});
				after(() => page.evaluate(() => Reader.setMarks([])));

				/** A point in the centre third on top of `selector`. */
				const centreOf = (selector) =>
					page.evaluate((sel) => {
						const x = window.innerWidth / 2;
						for (const el of flow.querySelectorAll(sel)) {
							for (const r of el.getClientRects()) {
								if (r.left < x - 4 && r.right > x + 4) return { x, y: r.top + r.height / 2 };
							}
						}
						return null;
					}, selector);
				const clear = () =>
					page.evaluate(() => {
						getSelection().removeAllRanges();
						calls.length = 0;
					});

				it('follows a link there, rather than asking for the controls', async () => {
					await clear();
					const at = await centreOf('a[href]');
					assert.ok(at, 'the link crosses the centre');
					await tap(page, at.x, at.y);
					await page.waitForFunction(() => callsOf('follow').length === 1);
					const r = await page.evaluate(() => ({
						follow: lastCall('follow'),
						chrome: callsOf('chrome')
					}));
					assert.deepEqual(r, { follow: ['follow', 3, 'target'], chrome: [] });
				});

				it('opens a mark there, rather than asking for the controls', async () => {
					await clear();
					const at = await centreOf('mark.sm-highlight');
					assert.ok(at, 'the mark crosses the centre');
					await tap(page, at.x, at.y);
					await page.waitForFunction(() => callsOf('markTapped').length === 1);
					assert.deepEqual(await page.evaluate(() => callsOf('chrome')), []);
				});

				it('only clears a live selection there', async () => {
					await clear();
					await page.evaluate(() => selectSome());
					const { x, y } = await zone(page, 1);
					await tap(page, x, y);
					await page.waitForFunction(() => getSelection().toString() === '');
					assert.deepEqual(await page.evaluate(() => callsOf('chrome')), []);
				});
			});

			it('turns forward from the centre for an app with no chrome() to call', async () => {
				const old = await open({
					bridge: ['ready', 'position', 'edge', 'follow', 'markTapped', 'unresolved']
				});
				await show(old.page, LONG, 2, 5, null);
				const { x, y } = await zone(old.page, 1);
				await tap(old.page, x, y);
				await old.page.waitForFunction(() => pageIdx() === 1);
				assert.deepEqual(old.errors, []);
				await old.context.close();
			});

			it('turns with the arrow and page keys, and leaves shift alone', async () => {
				await fresh();
				await page.keyboard.press('ArrowRight');
				await page.keyboard.press('PageDown');
				assert.equal(await page.evaluate(() => pageIdx()), 2);
				await page.keyboard.press('ArrowLeft');
				assert.equal(await page.evaluate(() => pageIdx()), 1);
				await page.keyboard.press('Shift+ArrowRight');
				await page.keyboard.press('PageUp');
				assert.equal(await page.evaluate(() => pageIdx()), 0);
			});

			if (engine.touch) {
				describe('swipes', () => {
					let cdp;
					before(async () => {
						cdp = await context.newCDPSession(page);
					});

					/** A finger drawn from x0 to x1; `midway` runs a third of the way along. */
					const swipe = async (x0, x1, y, midway) => {
						await cdp.send('Input.dispatchTouchEvent', {
							type: 'touchStart',
							touchPoints: [{ x: x0, y }]
						});
						for (let i = 1; i <= 6; i++) {
							await cdp.send('Input.dispatchTouchEvent', {
								type: 'touchMove',
								touchPoints: [{ x: x0 + ((x1 - x0) * i) / 6, y }]
							});
							if (i === 2 && midway) await midway();
						}
						// Dispatched and handled by the time this resolves: CDP waits
						// for the page to acknowledge the event.
						await cdp.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] });
						await page.evaluate(() => frames(1));
					};

					it('turns forward on a swipe left and back on a swipe right', async () => {
						await fresh();
						await swipe(300, 80, 400);
						assert.equal(await page.evaluate(() => pageIdx()), 1);
						await swipe(80, 300, 400);
						assert.equal(await page.evaluate(() => pageIdx()), 0);
					});

					it('turns on a swipe that starts in the centre, rather than asking for the controls', async () => {
						await fresh();
						await swipe(230, 20, 400);
						assert.equal(await page.evaluate(() => pageIdx()), 1);
						assert.deepEqual(await page.evaluate(() => callsOf('chrome')), []);
					});

					it('does not turn while a selection handle is dragged', async () => {
						await fresh();
						await page.evaluate(() => selectSome());
						await swipe(300, 80, 400);
						assert.equal(await page.evaluate(() => pageIdx()), 0);
					});

					it('does not turn on a drag that makes a selection part-way', async () => {
						await fresh();
						await swipe(300, 80, 400, () => page.evaluate(() => selectSome()));
						assert.equal(await page.evaluate(() => pageIdx()), 0);
					});

					it('turns again once the selection is gone', async () => {
						await page.evaluate(() => getSelection().removeAllRanges());
						await page.evaluate(() => frames(2));
						await swipe(300, 80, 400);
						assert.equal(await page.evaluate(() => pageIdx()), 1);
					});
				});
			}
		});

		describe('typography', () => {
			it('can be set before the first chapter, which is laid out with it', async () => {
				const { page, context, errors } = await open();
				const applied = await page.evaluate(() =>
					Reader.setTypography({
						theme: 'black',
						fontFamily: 'sans',
						fontScale: 1.3,
						lineHeight: 1.8,
						margin: 'wide'
					})
				);
				assert.deepEqual(applied, {
					theme: 'black',
					fontFamily: 'sans',
					fontScale: 1.3,
					lineHeight: 1.8,
					margin: 'wide'
				});
				assert.deepEqual(
					await page.evaluate(() => callsOf('position')),
					[],
					'nothing to report yet'
				);
				await show(page, LONG, 2, 5, null);
				const r = await page.evaluate(() => {
					const f = getComputedStyle(flow);
					const v = getComputedStyle(document.getElementById('viewport'));
					return {
						size: Number.parseFloat(f.fontSize),
						root: Number.parseFloat(getComputedStyle(document.documentElement).fontSize),
						leading: Number.parseFloat(f.lineHeight),
						family: f.fontFamily,
						padding: Number.parseFloat(v.paddingLeft),
						bg: getComputedStyle(document.body).backgroundColor
					};
				});
				assert.ok(Math.abs(r.size - r.root * 1.08 * 1.3) < 0.05, `font size ${r.size}`);
				assert.ok(Math.abs(r.leading - r.size * 1.8) < 0.5, `line height ${r.leading}`);
				assert.match(r.family, /^Roboto/);
				assert.equal(r.padding, 34);
				assert.deepEqual(rgb(r.bg), THEMES.black);
				assert.deepEqual(errors, []);
				await context.close();
			});

			it('refuses or clamps anything out of contract, field by field', async () => {
				const { page, context, errors } = await open();
				const r = await page.evaluate(() => {
					const out = {};
					const set = (name, value) => {
						out[name] = Reader.setTypography(value);
					};
					set('too big', { fontScale: 5, lineHeight: 4 });
					set('too small', { fontScale: 0.1, lineHeight: 0 });
					set('not numbers', { fontScale: '1.2', lineHeight: Number.NaN });
					set('infinite', { fontScale: Infinity, lineHeight: -Infinity });
					set('unknown names', { theme: 'mauve', fontFamily: 'comic', margin: 'huge' });
					set('css in a name', {
						theme: 'dark"] body { color: red',
						fontFamily: 'serif; color: red'
					});
					set('wrong type', { theme: 1, margin: null, fontFamily: ['sans'] });
					set('null', null);
					set('a string', 'dark');
					set('partial', { theme: 'sepia' });
					return out;
				});
				const base = { theme: 'light', fontFamily: 'serif', margin: 'normal' };
				assert.deepEqual(r['too big'], { ...base, fontScale: 1.6, lineHeight: 1.9 });
				assert.deepEqual(r['too small'], { ...base, fontScale: 0.8, lineHeight: 1.3 });
				for (const name of [
					'not numbers',
					'infinite',
					'unknown names',
					'css in a name',
					'wrong type',
					'null',
					'a string'
				]) {
					assert.deepEqual(r[name], { ...base, fontScale: 0.8, lineHeight: 1.3 }, name);
				}
				assert.deepEqual(r.partial, { ...base, theme: 'sepia', fontScale: 0.8, lineHeight: 1.3 });
				assert.equal(await page.evaluate(() => document.documentElement.dataset.theme), 'sepia');
				assert.deepEqual(errors, []);
				await context.close();
			});

			describe('while reading', () => {
				let page;
				let context;
				let here;
				let startPage;

				before(async () => {
					({ page, context } = await open());
					await page.evaluate((m) => Reader.setMarks(m), MARKS);
					await show(page, LONG, 2, 5, null);
					await page.evaluate(() => {
						for (let i = 0; i < 6; i++) Reader.turn(1);
					});
					[startPage, here] = await page.evaluate(() => [
						pageIdx(),
						firstVisibleByPage()[pageIdx()]
					]);
				});
				after(() => context?.close());

				const changes = [
					['a larger size', { fontScale: 1.6 }],
					['a smaller size', { fontScale: 0.8 }],
					['looser leading', { fontScale: 1, lineHeight: 1.9 }],
					['tighter leading', { lineHeight: 1.3 }],
					['wide margins', { lineHeight: 1.6, margin: 'wide' }],
					['narrow margins', { margin: 'narrow' }],
					['the sans face', { margin: 'normal', fontFamily: 'sans' }],
					['the serif face again', { fontFamily: 'serif' }]
				];
				for (const [name, change] of changes) {
					it(`keeps the paragraph on screen through ${name}, and reports at once`, async () => {
						const r = await page.evaluate(
							([c, h]) => {
								const pagesBefore = pageCount();
								calls.length = 0;
								Reader.setTypography(c);
								return {
									on: onScreen(h),
									laidOut: laidOut(),
									pagesBefore,
									pagesAfter: pageCount(),
									position: lastCall('position'),
									first: firstVisibleByPage()[pageIdx()]
								};
							},
							[change, here]
						);
						assert.ok(r.laidOut, 're-measured');
						assert.ok(r.on, `${here} on screen`);
						assert.ok(r.position, 'position reported without waiting');
						assert.equal(r.position[1], `epub:2/${r.first}:0`);
						if ('fontScale' in change && change.fontScale !== 1) {
							assert.notEqual(r.pagesAfter, r.pagesBefore, 'the size changed the layout');
						}
					});
				}

				it('is back on the page it started from once every setting is back', async () => {
					assert.equal(await page.evaluate(() => pageIdx()), startPage);
				});

				it('does not move or report anything for a change of theme alone', async () => {
					const r = await page.evaluate(() => {
						const before = [flow.style.transform, pageCount()];
						calls.length = 0;
						Reader.setTypography({ theme: 'dark' });
						return {
							before,
							after: [flow.style.transform, pageCount()],
							position: callsOf('position')
						};
					});
					assert.deepEqual(r.after, r.before);
					assert.deepEqual(r.position, []);
				});

				it('does not report anything for a call that changes nothing', async () => {
					const r = await page.evaluate(() => {
						calls.length = 0;
						Reader.setTypography({
							theme: 'dark',
							fontScale: 1,
							lineHeight: 1.6,
							margin: 'normal',
							fontFamily: 'serif'
						});
						return callsOf('position');
					});
					assert.deepEqual(r, []);
				});

				it('leaves a live selection alone, and repaints once it clears', async () => {
					const selected = await page.evaluate(() => selectSome());
					const r = await page.evaluate(() => {
						Reader.setTypography({ fontScale: 1.2 });
						return getSelection().toString();
					});
					assert.equal(r, selected);
					await page.evaluate(() => getSelection().removeAllRanges());
					await page.evaluate(() => Reader.setTypography({ fontScale: 1 }));
					assert.ok(await page.evaluate((h) => onScreen(h), here));
				});
			});

			describe('themes', () => {
				let page;
				let context;
				const colours = ['', 'yellow', 'green', 'blue', 'pink'];

				before(async () => {
					({ page, context } = await open());
					// One mark of every colour on the first page, a shared one and a noted one.
					const marks = colours.map((c, i) => ({
						id: `T${i}`,
						start: `epub:2/p[${i + 1}]:4`,
						end: `epub:2/p[${i + 1}]:60`,
						colour: c || null,
						shared: false,
						hasNote: i === 0
					}));
					marks.push({
						id: 'S',
						start: 'epub:2/p[1]:70',
						end: 'epub:2/p[1]:110',
						colour: null,
						shared: true,
						hasNote: false
					});
					await page.evaluate((m) => Reader.setMarks(m), marks);
					await show(page, LONG, 2, 5, null);
				});
				after(() => context?.close());

				for (const [theme, page_] of Object.entries(THEMES)) {
					it(`renders ${theme}, with legible ink, links and highlights`, async () => {
						const r = await page.evaluate((t) => {
							Reader.setTypography({ theme: t });
							const col = (el, prop, pseudo) => getComputedStyle(el, pseudo)[prop];
							return {
								page: col(document.body, 'backgroundColor'),
								ink: col(flow, 'color'),
								link: col(flow.querySelector('a[href]'), 'color'),
								fills: ['', 'yellow', 'green', 'blue', 'pink'].map((c, i) =>
									col(marksOf(`T${i}`)[0], 'backgroundColor')
								),
								markInk: col(marksOf('T0')[0], 'color'),
								shared: col(marksOf('S')[0], 'textDecorationColor'),
								sharedFill: col(marksOf('S')[0], 'backgroundColor'),
								note: col(marksOf('T0').at(-1), 'backgroundColor', '::after')
							};
						}, theme);
						const pg = rgb(r.page);
						const ink = rgb(r.ink);
						assert.deepEqual(pg, page_, 'the page is the theme');
						assert.deepEqual(rgb(r.markInk), ink, 'a mark keeps the ink');
						assert.ok(contrast(ink, pg) >= 7, `ink on page ${contrast(ink, pg).toFixed(2)}`);
						assert.ok(
							contrast(rgb(r.link), pg) >= 4.5,
							`link ${contrast(rgb(r.link), pg).toFixed(2)}`
						);
						const fills = r.fills.map(rgb);
						assert.equal(new Set(fills.map(String)).size, fills.length, 'every colour is its own');
						for (const [i, fill] of fills.entries()) {
							const name = colours[i] || 'default';
							assert.ok(
								contrast(ink, fill) >= 4.5,
								`ink on ${name} ${contrast(ink, fill).toFixed(2)}`
							);
							assert.ok(
								deltaE(fill, pg) >= 10,
								`${name} visible on the page: dE ${deltaE(fill, pg).toFixed(1)}`
							);
						}
						assert.ok(
							/rgba\(0, 0, 0, 0\)|transparent/.test(r.sharedFill),
							'a shared mark is not filled'
						);
						assert.ok(
							contrast(rgb(r.shared), pg) >= 3,
							`shared underline ${contrast(rgb(r.shared), pg).toFixed(2)}`
						);
						assert.ok(
							contrast(rgb(r.note), pg) >= 3,
							`note dot ${contrast(rgb(r.note), pg).toFixed(2)}`
						);
						if (SHOTS) {
							fs.mkdirSync(SHOTS, { recursive: true });
							await page.screenshot({ path: path.join(SHOTS, `${engine.name}-${theme}.png`) });
						}
					});
				}
			});

			describe('motion', () => {
				it('slides a page turn by default', async () => {
					const { page, context } = await open();
					const d = await page.evaluate(() => getComputedStyle(flow).transitionDuration);
					assert.equal(d, '0.18s');
					await context.close();
				});
				it('jumps instead for somebody who asked for reduced motion', async () => {
					const { page, context } = await open({ reducedMotion: 'reduce' });
					const d = await page.evaluate(() => getComputedStyle(flow).transitionDuration);
					assert.equal(d, '0s');
					await context.close();
				});
			});
		});
	});
}
