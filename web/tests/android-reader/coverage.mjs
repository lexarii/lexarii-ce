// SPDX-License-Identifier: AGPL-3.0-or-later

/*
 * Line coverage of the Android reader page's own scripts, for
 * `npm run test:coverage`.
 *
 * The page runs in a real Chromium, out of reach of vitest's instrumentation,
 * so this asks the browser instead: V8 counts every block it runs, and
 * Playwright hands those counts over per script. locator.js and reader.js are
 * inlined into the page verbatim, so a script whose text is exactly one of
 * them IS that file, offsets and all.
 *
 * The counts become LCOV line records appended to the file vitest wrote, so
 * one lcov.info covers both. Paths are relative to the repository root, as
 * vitest's are (see vite.config.ts).
 *
 * Lines only - no function or branch records - and a line counts as what
 * its first character counts as. Firefox has no such counter: with
 * READER_TEST_FIREFOX, only the Chromium pages are measured.
 */

import fs from 'node:fs';
import path from 'node:path';

import { ASSETS } from './harness.mjs';

const SCRIPTS = ['locator.js', 'reader.js'];

/** A collector writing to `file`, or null when no file was asked for. */
export function coverage(file) {
	if (!file) return null;
	const files = SCRIPTS.map((name) => ({
		path: `android/app/src/main/assets/reader/${name}`,
		source: fs.readFileSync(path.join(ASSETS, name), 'utf8'),
		hits: new Map()
	}));

	function add(entry) {
		const file = files.find((f) => f.source === entry.source);
		if (!file) return;
		for (const [line, count] of lineCounts(entry.source, entry.functions)) {
			file.hits.set(line, (file.hits.get(line) ?? 0) + count);
		}
	}

	return {
		/**
		 * Measures `page` until `context` is closed. Coverage has to be
		 * collected while the page is still open, and the tests close
		 * contexts, not pages - so the collection goes in front of that.
		 */
		async track(context, page) {
			if (!page.coverage) return; // not Chromium
			await page.coverage.startJSCoverage({ resetOnNavigation: false });
			const close = context.close.bind(context);
			context.close = async (...args) => {
				try {
					for (const entry of await page.coverage.stopJSCoverage()) add(entry);
				} catch {
					/* the page went first; what it ran is lost, nothing else is */
				}
				return close(...args);
			};
		},

		/** Appends one LCOV record per script that ran at all. */
		write() {
			let out = '';
			for (const f of files) {
				if (f.hits.size === 0) continue;
				const lines = [...f.hits].sort((a, b) => a[0] - b[0]);
				out += `TN:\nSF:${f.path}\n`;
				for (const [line, count] of lines) out += `DA:${line},${count}\n`;
				out += `LF:${lines.length}\nLH:${lines.filter(([, c]) => c > 0).length}\nend_of_record\n`;
			}
			if (out) {
				fs.mkdirSync(path.dirname(file), { recursive: true });
				fs.appendFileSync(file, out);
			}
		}
	};
}

/**
 * [line, count] for every line of `source` that holds code, from V8's block
 * counts.
 *
 * V8's ranges nest: a function's range, then the blocks inside it, each with
 * its own count. Painting them outermost first leaves every character with
 * the count of the innermost range around it.
 */
function lineCounts(source, functions) {
	const ranges = functions.flatMap((f) => f.ranges);
	ranges.sort((a, b) => a.startOffset - b.startOffset || b.endOffset - a.endOffset);
	const painted = new Float64Array(source.length);
	for (const r of ranges) painted.fill(r.count, r.startOffset, r.endOffset);

	const out = [];
	let offset = 0;
	for (const [i, text] of source.split('\n').entries()) {
		const first = text.search(/\S/);
		if (first >= 0 && !/^(\/\/|\/\*|\*)/.test(text.slice(first))) {
			out.push([i + 1, painted[offset + first]]);
		}
		offset += text.length + 1;
	}
	return out;
}
