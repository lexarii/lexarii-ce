// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, expect, it } from 'vitest';
import { buildIndex, nodeAt, occurrenceIn, rangeOf } from './pdftext';

/**
 * A text layer as pdf.js builds one: a span per run, a line broken wherever
 * the PDF broke it, and no relationship between a sentence and an element.
 */
function layerOf(...runs: string[]): HTMLDivElement {
	const layer = document.createElement('div');
	for (const run of runs) {
		const span = document.createElement('span');
		span.textContent = run;
		layer.append(span);
	}
	document.body.replaceChildren(layer);
	return layer;
}

describe('buildIndex', () => {
	it('collapses the line breaks pdf.js inherits from the page', () => {
		// The PDF broke this line; the reader selecting it does not mean the
		// break, and searching for the phrase with a space has to find it.
		const index = buildIndex(layerOf('a sentence broken\n', '   across two lines'));
		expect(index?.hay).toBe('a sentence broken across two lines');
	});

	it('maps every collapsed character back to where it came from', () => {
		const index = buildIndex(layerOf('one', '  two'));
		expect(index?.hay).toBe('one two');
		// The raw text is "one  two": the space at hay[3] is the first of the
		// two, and "two" starts at raw offset 5.
		expect(index?.origin[3]).toBe(3);
		expect(index?.origin[4]).toBe(5);
	});

	it('is null when there is no layer to read', () => {
		expect(buildIndex(null)).toBeNull();
		expect(buildIndex(undefined)).toBeNull();
	});
});

describe('rangeOf', () => {
	it('finds a phrase that spans two spans', () => {
		const layer = layerOf('the quick brown ', 'fox jumps');
		const range = rangeOf(layer, 'brown fox', 0);

		expect(range).not.toBeNull();
		expect(range?.toString()).toBe('brown fox');
	});

	// The whole reason a mark stores an occurrence: a quote alone is not an
	// anchor, because the same words appear more than once on a page.
	it('picks the occurrence it was asked for', () => {
		const layer = layerOf('a note here. ', 'and a note there.');

		expect(rangeOf(layer, 'a note', 0)?.startOffset).toBe(0);
		const second = rangeOf(layer, 'a note', 1);
		expect(second).not.toBeNull();
		// The second "a note" is in the second span, four characters in.
		expect(second?.startContainer.textContent).toContain('and a note there');
	});

	it('finds a phrase written with a line break in it', () => {
		const layer = layerOf('the quick\n', 'brown fox');
		expect(rangeOf(layer, 'quick brown', 0)?.toString().replace(/\s+/g, ' ')).toBe('quick brown');
	});

	// Null is an ordinary answer. A mark made before the page was re-rendered,
	// or on a page whose layer is not built yet, simply cannot be painted.
	it('returns null rather than guessing', () => {
		const layer = layerOf('nothing relevant');
		expect(rangeOf(layer, 'absent phrase', 0)).toBeNull();
		expect(rangeOf(layer, 'nothing', 5)).toBeNull(); // no sixth occurrence
		expect(rangeOf(layer, '   ', 0)).toBeNull();
		expect(rangeOf(null, 'anything', 0)).toBeNull();
	});
});

describe('occurrenceIn', () => {
	it('counts the identical phrases before the one selected', () => {
		const layer = layerOf('a note here. and a note there. and a note again.');
		const text = layer.firstChild?.firstChild as Text;

		const selection = {
			getRangeAt() {
				const range = document.createRange();
				// The third "a note", at raw offset 35.
				range.setStart(text, 35);
				range.setEnd(text, 41);
				return range;
			}
		} as unknown as Selection;

		expect(occurrenceIn(layer, selection, 'a note')).toBe(2);
	});

	it('answers zero when the selection is not in this layer', () => {
		const layer = layerOf('some words');
		const elsewhere = document.createElement('p');
		elsewhere.textContent = 'some words';
		document.body.append(elsewhere);

		const selection = {
			getRangeAt() {
				const range = document.createRange();
				range.setStart(elsewhere.firstChild as Text, 0);
				range.setEnd(elsewhere.firstChild as Text, 4);
				return range;
			}
		} as unknown as Selection;

		expect(occurrenceIn(layer, selection, 'some')).toBe(0);
	});

	it('answers zero when there is no selection to read', () => {
		const layer = layerOf('some words');
		const empty = {
			getRangeAt() {
				throw new Error('no ranges');
			}
		} as unknown as Selection;

		expect(occurrenceIn(layer, empty, 'some')).toBe(0);
	});
});

describe('nodeAt', () => {
	// pdf.js emits empty and zero-width nodes, so several can share a start
	// offset; the last one wins or a mark lands in the wrong span.
	it('takes the last node starting at or before the offset', () => {
		const layer = layerOf('abc', '', 'def');
		const index = buildIndex(layer);
		expect(index).not.toBeNull();

		const [node, offset] = nodeAt(index!, 3) ?? [];
		expect(node?.textContent).toBe('def');
		expect(offset).toBe(0);
	});

	it('is null when the index is empty', () => {
		expect(nodeAt({ nodes: [], starts: [] }, 0)).toBeNull();
	});
});
