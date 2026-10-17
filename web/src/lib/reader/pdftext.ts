// SPDX-License-Identifier: AGPL-3.0-or-later

/**
 * Finding a quoted phrase in a rendered PDF page, and turning it back into a
 * DOM range.
 *
 * A PDF highlight is stored as the words themselves plus which occurrence of
 * them on the page, because a PDF has no stable element paths to anchor to.
 * Painting one back means finding that phrase in pdf.js's text layer - which
 * is rebuilt from scratch on every render - and producing a Range over the
 * actual text nodes.
 *
 * Extracted from the PDF reader because it is the subtlest logic in either
 * reader and the only part of one that can be tested without a browser: it
 * works on text nodes and string offsets, which jsdom has, rather than on
 * canvas layout, which it does not. It had no tests at all while it lived in
 * the component.
 */

/**
 * The page's words, indexed so a phrase can be found and turned back into a
 * DOM range.
 *
 * `hay` is the text with runs of whitespace collapsed, because pdf.js breaks a
 * line wherever the PDF did and nobody selecting a sentence means "including
 * that line break". `origin` maps each character of `hay` back to the raw text
 * it came from, and `starts` says which node each raw character belongs to.
 * Searching in one space and answering in the other is what painted every mark
 * a line too high, so both are kept together.
 */
export type PageIndex = {
	nodes: Text[];
	starts: number[];
	hay: string;
	origin: number[];
};

/** Reads a rendered text layer into an index. */
export function buildIndex(layer: HTMLElement | null | undefined): PageIndex | null {
	if (!layer) return null;

	const nodes: Text[] = [];
	const starts: number[] = [];
	let text = '';
	const walker = document.createTreeWalker(layer, NodeFilter.SHOW_TEXT);
	for (let node = walker.nextNode(); node; node = walker.nextNode()) {
		starts.push(text.length);
		nodes.push(node as Text);
		text += node.textContent ?? '';
	}

	let hay = '';
	const origin: number[] = [];
	for (let i = 0; i < text.length; i++) {
		const ch = text[i] ?? '';
		if (/\s/.test(ch)) {
			if (hay.endsWith(' ')) continue;
			hay += ' ';
		} else {
			hay += ch;
		}
		origin.push(i);
	}
	return { nodes, starts, hay, origin };
}

/**
 * Which text node holds a raw character offset, and where in it.
 *
 * Walks backwards so the LAST node starting at or before the offset wins:
 * pdf.js emits empty and zero-width nodes, and a forward scan stops on the
 * first of several sharing a start.
 */
export function nodeAt(
	index: Pick<PageIndex, 'nodes' | 'starts'>,
	offset: number
): [Text, number] | null {
	for (let i = index.nodes.length - 1; i >= 0; i--) {
		const node = index.nodes[i];
		const start = index.starts[i];
		if (node !== undefined && start !== undefined && start <= offset) {
			return [node, Math.min(offset - start, node.length)];
		}
	}
	return null;
}

/**
 * The range covering the nth occurrence of a quote on this page, or null when
 * the page does not contain it.
 *
 * Null is an ordinary answer, not a failure: a mark made before the book was
 * re-rendered, or on a page whose text layer has not been built yet, simply
 * cannot be painted this time round.
 */
export function rangeOf(
	layer: HTMLElement | null | undefined,
	quote: string,
	occurrence: number
): Range | null {
	const index = buildIndex(layer);
	const needle = quote.replace(/\s+/g, ' ').trim();
	if (!index || !needle) return null;

	let found = -1;
	for (let n = 0; n <= occurrence; n++) {
		found = index.hay.indexOf(needle, found + 1);
		if (found < 0) return null;
	}

	const from = nodeAt(index, index.origin[found] ?? 0);
	const to = nodeAt(index, (index.origin[found + needle.length - 1] ?? 0) + 1);
	if (!from || !to) return null;

	const range = document.createRange();
	try {
		range.setStart(from[0], from[1]);
		range.setEnd(to[0], to[1]);
	} catch {
		return null;
	}
	return range;
}

/**
 * Which occurrence of a phrase the reader has selected.
 *
 * A quote alone is not an anchor: "the" appears forty times on a page. What
 * makes the mark findable again is the quote plus how many identical ones
 * precede it, counted in the same collapsed space the search uses.
 */
export function occurrenceIn(
	layer: HTMLElement | null | undefined,
	selection: Selection,
	quote: string
): number {
	const index = buildIndex(layer);
	if (!index) return 0;

	let raw = -1;
	try {
		const at = selection.getRangeAt(0);
		const node = index.nodes.indexOf(at.startContainer as Text);
		if (node < 0) return 0;
		raw = (index.starts[node] ?? 0) + at.startOffset;
	} catch {
		return 0;
	}

	// Where that lands in the collapsed text.
	let upTo = index.origin.findIndex((o) => o >= raw);
	if (upTo < 0) upTo = index.hay.length;

	const before = index.hay.slice(0, upTo);
	let count = 0;
	for (let at = before.indexOf(quote); at >= 0; at = before.indexOf(quote, at + 1)) count++;
	return count;
}
