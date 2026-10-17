// SPDX-License-Identifier: AGPL-3.0-or-later

/**
 * Reading the marks a PDF carries in its own file.
 *
 * A PDF's highlights are rectangles over the page, and turning a rectangle
 * back into words needs to know where the words are. The browser knows,
 * because pdf.js gives every run of text a position; the server does not - its
 * extractor reports every word at position zero on the very books that carry
 * annotations, which is why this lives here and not in ingest. The same reason
 * cover rendering happens in the browser.
 *
 * Nothing is rendered. This reads the annotation list and the text content,
 * both of which are data, so importing a 400-page book does not mean drawing
 * one.
 */

import type { ImportedMark } from '$api/client';

/**
 * The annotation types that mark up text: all four carry quadPoints and mean
 * "these words".
 *
 * A Link is in every book with a table of contents and is nobody's highlight.
 * A Popup is the little window attached to another annotation rather than a
 * mark of its own, and importing both would double every note.
 */
const markupTypes = new Set(['Highlight', 'Underline', 'StrikeOut', 'Squiggly']);

/** Sticky notes: their own text, attached to a place rather than to words. */
const noteTypes = new Set(['Text', 'FreeText']);

/** How far a mark may sit from a line's baseline and still belong to it. */
const lineTolerance = 12;

type Box = { x1: number; y1: number; x2: number; y2: number };
type Run = { text: string; box: Box };
type Line = { baseline: number; runs: Run[] };

/**
 * Reads one document's marks.
 *
 * `onProgress` is called per page: a long book takes a while, and somebody
 * watching deserves to know it is moving.
 */
export async function readMarks(
	// eslint-disable-next-line @typescript-eslint/no-explicit-any
	doc: any,
	onProgress?: (page: number, pages: number) => void
): Promise<ImportedMark[]> {
	const out: ImportedMark[] = [];

	for (let n = 1; n <= doc.numPages; n++) {
		onProgress?.(n, doc.numPages);

		const page = await doc.getPage(n);
		// eslint-disable-next-line @typescript-eslint/no-explicit-any
		const annotations: any[] = await page.getAnnotations({ intent: 'display' });

		const markup = annotations.filter(
			(a) => markupTypes.has(a.subtype) && quadCount(a.quadPoints) > 0
		);
		const notes = annotations.filter((a) => noteTypes.has(a.subtype) && contentsOf(a) !== '');
		if (markup.length === 0 && notes.length === 0) continue;

		// Text is fetched only for pages that carry marks, which on a real book
		// is a handful of the hundreds.
		const lines = linesOf(await page.getTextContent());
		const seen = new Map<string, number>();

		for (const annotation of markup) {
			const text = boxesOf(annotation.quadPoints)
				.map((box) => textUnder(box, lines))
				.filter(Boolean)
				.join(' ')
				.replace(/\s+/g, ' ')
				.trim();
			if (!text) continue;

			// Which occurrence of this phrase on this page, counted the way the
			// reader counts one made by hand - so an imported mark and a
			// hand-made one are found again the same way.
			const key = `${n}:${text}`;
			const occurrence = seen.get(key) ?? 0;
			seen.set(key, occurrence + 1);

			out.push({ page: n, occurrence, text, note: contentsOf(annotation) });
		}

		for (const note of notes) {
			out.push({ page: n, occurrence: 0, text: '', note: contentsOf(note) });
		}
	}
	return out;
}

/** What the other reader wrote with a mark, which is usually nothing. */
// eslint-disable-next-line @typescript-eslint/no-explicit-any
function contentsOf(annotation: any): string {
	const raw = annotation.contentsObj?.str ?? annotation.contents ?? '';
	return typeof raw === 'string' ? raw.trim() : '';
}

/**
 * How many numbers a quadPoints value holds, or zero.
 *
 * Not `Array.isArray`: pdf.js gives a Float32Array, which is array-LIKE and
 * not an array. Testing for the wrong one silently rejected all 1,051
 * underlines in a real book while reporting there was nothing to import.
 */
// eslint-disable-next-line @typescript-eslint/no-explicit-any
function quadCount(quadPoints: any): number {
	if (!quadPoints || typeof quadPoints.length !== 'number') return 0;
	return quadPoints.length;
}

/**
 * Groups a page's text into lines by baseline.
 *
 * pdf.js emits one line as several runs when the font changes, and a mark
 * belongs to ONE line. Matching every run whose vertical band overlapped the
 * mark pulled in the lines above and below, which turned a highlight over
 * "(2.2.3)" into "integra ( 2.2.3) nts of".
 */
// eslint-disable-next-line @typescript-eslint/no-explicit-any
function linesOf(content: any): Line[] {
	const byBaseline = new Map<number, Run[]>();

	// eslint-disable-next-line @typescript-eslint/no-explicit-any
	for (const item of content.items as any[]) {
		if (!item.str || !item.transform) continue;
		// transform is [a, b, c, d, e, f]: e and f place it, and the baseline
		// is f. The height is the line's, not the glyph's.
		const x = item.transform[4];
		const baseline = item.transform[5];
		const height = item.height || Math.abs(item.transform[3]) || 0;
		const run: Run = {
			text: item.str,
			box: { x1: x, y1: baseline, x2: x + (item.width || 0), y2: baseline + height }
		};

		const key = Math.round(baseline);
		let at = key;
		for (const existing of byBaseline.keys()) {
			if (Math.abs(existing - key) <= 2) {
				at = existing;
				break;
			}
		}
		const runs = byBaseline.get(at);
		if (runs) runs.push(run);
		else byBaseline.set(at, [run]);
	}

	return [...byBaseline.entries()].map(([baseline, runs]) => ({
		baseline,
		runs: runs.sort((a, b) => a.box.x1 - b.box.x1)
	}));
}

/**
 * Turns quadPoints into rectangles.
 *
 * Two shapes depending on the pdf.js version: a flat run of numbers, eight per
 * quad, or a list of objects with x and y. Both name the same four corners.
 */
// eslint-disable-next-line @typescript-eslint/no-explicit-any
function boxesOf(quadPoints: any): Box[] {
	const out: Box[] = [];

	if (typeof quadPoints[0] === 'number') {
		for (let i = 0; i + 7 < quadPoints.length; i += 8) {
			const xs = [quadPoints[i], quadPoints[i + 2], quadPoints[i + 4], quadPoints[i + 6]];
			const ys = [quadPoints[i + 1], quadPoints[i + 3], quadPoints[i + 5], quadPoints[i + 7]];
			out.push({
				x1: Math.min(...xs),
				y1: Math.min(...ys),
				x2: Math.max(...xs),
				y2: Math.max(...ys)
			});
		}
		return out;
	}

	for (const quad of quadPoints) {
		const points = Array.isArray(quad) ? quad : [quad];
		const xs = points.map((p: { x: number }) => p.x);
		const ys = points.map((p: { y: number }) => p.y);
		if (xs.length === 0) continue;
		out.push({
			x1: Math.min(...xs),
			y1: Math.min(...ys),
			x2: Math.max(...xs),
			y2: Math.max(...ys)
		});
	}
	return out;
}

/**
 * The words a mark covers, taken from the one line it sits on.
 *
 * pdf.js hands back whole lines, so an underline under three words of a forty
 * word line has to be cut out of it. By proportion, because the text content
 * carries no per-character positions: for proportional type that is an
 * approximation, and it is the difference between a quotation somebody
 * recognises and nothing at all.
 */
export function textUnder(box: Box, lines: Line[]): string {
	// An underline sits at or just below the baseline; a highlight covers the
	// letters. Both are nearest the baseline of their own line.
	let closest: Line | null = null;
	let distance = Infinity;
	for (const line of lines) {
		const d = Math.min(
			Math.abs(line.baseline - box.y1),
			Math.abs(line.baseline - (box.y1 + box.y2) / 2)
		);
		if (d < distance) {
			distance = d;
			closest = line;
		}
	}
	if (!closest || distance > lineTolerance) return '';

	const parts: string[] = [];
	for (const run of closest.runs) {
		const r = run.box;
		const width = r.x2 - r.x1;
		if (width <= 0) continue;

		const from = Math.max(box.x1, r.x1);
		const to = Math.min(box.x2, r.x2);
		if (to - from < 1) continue;

		// Nearly the whole run: take it as written rather than re-slicing it.
		if (to - from >= width * 0.9) {
			parts.push(run.text);
			continue;
		}
		const start = Math.floor(((from - r.x1) / width) * run.text.length);
		const end = Math.ceil(((to - r.x1) / width) * run.text.length);
		parts.push(run.text.slice(Math.max(0, start), Math.min(run.text.length, end)));
	}
	return parts.join('').replace(/\s+/g, ' ').trim();
}
