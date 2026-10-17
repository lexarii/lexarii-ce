// SPDX-License-Identifier: AGPL-3.0-or-later

/**
 * Locators: stable pointers into a book.
 *
 * A reading position must survive a font-size change, a rotation, a different
 * device, and a re-ingest of the file. A page number cannot: it is a property
 * of how the text happens to be laid out at this instant, and every one of
 * those changes relays it out. An element path can, which is why it is what
 * gets stored.
 *
 * The wire format matches the Go side exactly:
 *   epub:<spineIndex>/<elementPath>:<charOffset>
 */

/** Elements that are real content rather than layout wrappers. */
const BLOCK = /^(P|H[1-6]|LI|BLOCKQUOTE|PRE|FIGURE|TD|DD|DT)$/;

/**
 * Builds the path of an element relative to a root, as `tag[n]/tag[n]/…`.
 *
 * The index counts same-named siblings, so the path depends only on the
 * document's structure - which is exactly what re-rendering at a different size
 * does not change.
 */
export function elementPath(element: Element, root: Element): string {
	const parts: string[] = [];
	let node: Element | null = element;

	while (node && node !== root) {
		const parent: Element | null = node.parentElement;
		if (!parent) break;

		let index = 1;
		for (const sibling of parent.children) {
			if (sibling === node) break;
			if (sibling.tagName === node.tagName) index++;
		}
		parts.unshift(`${node.tagName.toLowerCase()}[${index}]`);
		node = parent;
	}

	return parts.join('/');
}

/** Resolves a path built by elementPath back to its element, or null. */
export function elementAtPath(path: string, root: Element): Element | null {
	if (!path) return null;

	let node: Element = root;
	for (const segment of path.split('/')) {
		const match = /^([a-z0-9-]+)\[(\d+)\]$/i.exec(segment);
		// Under noUncheckedIndexedAccess a capture group is string | undefined
		// even when the pattern guarantees it, so both are checked rather than
		// asserted away.
		const rawTag = match?.[1];
		const rawNth = match?.[2];
		if (!rawTag || !rawNth) return null;

		const tag = rawTag.toLowerCase();
		const nth = Number(rawNth);

		let seen = 0;
		let found: Element | null = null;
		for (const child of node.children) {
			if (child.tagName.toLowerCase() === tag) {
				seen++;
				if (seen === nth) {
					found = child;
					break;
				}
			}
		}
		if (!found) return null;
		node = found;
	}
	return node;
}

/**
 * Finds the first block of real content visible in the column on screen.
 *
 * This is what makes a saved position meaningful: the answer is "the paragraph
 * being read", not "column seven", and a paragraph keeps its identity when the
 * same text is laid out at a different size on a different device.
 */
export function firstVisibleElement(
	flow: HTMLElement,
	columnLeft: number,
	columnWidth: number
): Element | null {
	const flowBox = flow.getBoundingClientRect();
	let best: Element | null = null;
	let bestOffset = Infinity;

	for (const el of flow.querySelectorAll('*')) {
		// A wrapper spanning the whole chapter is technically visible on every
		// page and says nothing about where the reader is.
		if (!BLOCK.test(el.tagName)) continue;

		const box = el.getBoundingClientRect();
		if (box.width === 0 && box.height === 0) continue;

		// Position within the flow's own coordinates, undoing the page transform.
		const left = box.left - flowBox.left;
		if (left < columnLeft - 1) continue;
		if (left > columnLeft + columnWidth) continue;

		if (left < bestOffset) {
			bestOffset = left;
			best = el;
		}
	}

	return best;
}

/**
 * The pictures that can still move `target`: those not yet loaded that come
 * before it in the chapter or sit inside it. Every unloaded picture when
 * `target` is null, for landing on the chapter's last page.
 *
 * A picture with no width and height in its markup - most of them; Project
 * Gutenberg's never have - takes no space until it has loaded. Measured
 * before then, a chapter is shorter than it will be and every paragraph after
 * a picture is pages earlier than it will be. A saved position resolved
 * against that layout put the reader back several pages before where they
 * stopped, and the text then moved on without them as the pictures arrived.
 *
 * Here rather than in each reader because both readers must agree on it: the
 * Android page runs this module compiled.
 */
export function pendingImages(root: Element, target: Element | null): HTMLImageElement[] {
	const out: HTMLImageElement[] = [];
	for (const img of root.querySelectorAll('img')) {
		if (img.complete) continue;
		if (target) {
			const at = img.compareDocumentPosition(target);
			// The target follows the picture, or contains it.
			if (!(at & (Node.DOCUMENT_POSITION_FOLLOWING | Node.DOCUMENT_POSITION_CONTAINS))) continue;
		}
		out.push(img);
	}
	return out;
}

/**
 * Resolves once every one of `images` has loaded or failed, or after `ms`,
 * whichever is first. Never rejects: a picture that will not load is not a
 * reason to keep somebody from their page, and one that is slow is caught by
 * the reader relaying out when it does arrive.
 */
export function whenLoaded(images: readonly HTMLImageElement[], ms: number): Promise<void> {
	return new Promise((resolve) => {
		let left = images.filter((img) => !img.complete).length;
		if (left === 0) {
			resolve();
			return;
		}
		const timer = setTimeout(done, ms);
		function done() {
			clearTimeout(timer);
			for (const img of images) {
				img.removeEventListener('load', one);
				img.removeEventListener('error', one);
			}
			resolve();
		}
		function one() {
			if (--left <= 0) done();
		}
		for (const img of images) {
			if (img.complete) continue;
			img.addEventListener('load', one);
			img.addEventListener('error', one);
		}
	});
}

/** Formats an EPUB locator in the canonical wire form. */
export function formatEpubLocator(spineIndex: number, path: string, charOffset = 0): string {
	return `epub:${spineIndex}/${path}:${charOffset}`;
}

export interface ParsedLocator {
	format: string;
	spineIndex: number;
	path: string;
	charOffset: number;
}

/** Parses the canonical wire form. Returns null on anything malformed. */
export function parseLocator(locator: string): ParsedLocator | null {
	const match = /^epub:(\d+)\/(.+):(\d+)$/.exec(locator);
	const spine = match?.[1];
	const path = match?.[2];
	const offset = match?.[3];
	if (!spine || !path || offset === undefined) return null;

	return {
		format: 'epub',
		spineIndex: Number(spine),
		path,
		charOffset: Number(offset)
	};
}

/** Walks the text nodes of an element in document order. */
function textNodes(root: Node): Text[] {
	const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
	const out: Text[] = [];
	let node = walker.nextNode();
	while (node) {
		out.push(node as Text);
		node = walker.nextNode();
	}
	return out;
}

/** The nearest block-level ancestor, which is what a locator anchors to. */
function blockAncestor(node: Node, root: Element): Element | null {
	let el: Element | null =
		node.nodeType === Node.ELEMENT_NODE ? (node as Element) : node.parentElement;
	while (el && el !== root) {
		if (BLOCK.test(el.tagName)) return el;
		el = el.parentElement;
	}
	return null;
}

/**
 * The character offset of a position within its block's text.
 *
 * Offsets are counted over the block's TEXT, not its markup, so a highlight
 * survives the inline elements around it being different - which is exactly
 * what a re-ingest or a different sanitiser pass can change.
 */
function offsetWithin(block: Element, container: Node, offset: number): number {
	let count = 0;
	for (const node of textNodes(block)) {
		if (node === container) return count + offset;
		count += node.data.length;
	}
	return count;
}

export interface SelectionAnchor {
	startPath: string;
	startOffset: number;
	endPath: string;
	endOffset: number;
	text: string;
}

/** Whether a text node lies inside a block of the chapter, and so can be named. */
function inBlock(node: Text, root: Element): boolean {
	return blockAncestor(node, root) !== null;
}

/** The deepest last descendant of a node, or the node itself. */
function lastDescendant(node: Node): Node {
	let out = node;
	while (out.lastChild) out = out.lastChild;
	return out;
}

/**
 * The text position a boundary point stands for.
 *
 * Itself, when it is already in a block's text. Otherwise the start of the
 * first block text after it (`forward`, for a range's start) or the end of the
 * last block text before it (for a range's end). Null when there is none in
 * that direction inside `root`.
 */
function textPoint(
	container: Node,
	offset: number,
	forward: boolean,
	root: Element
): { node: Text; offset: number } | null {
	if (container.nodeType === Node.TEXT_NODE && inBlock(container as Text, root)) {
		return { node: container as Text, offset };
	}

	const usable = (node: Node | null): node is Text =>
		!!node &&
		node.nodeType === Node.TEXT_NODE &&
		(node as Text).data.length > 0 &&
		inBlock(node as Text, root);

	const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
	const isText = container.nodeType === Node.TEXT_NODE;

	if (forward) {
		// The first node at or after the boundary. Inside a text node that
		// is not in a block, nothing of that node counts, so it starts after.
		let from: Node;
		if (isText) {
			from = container;
		} else {
			const child = container.childNodes[offset];
			if (child) {
				if (usable(child)) return { node: child, offset: 0 };
				from = child;
			} else {
				// Past the last child: everything inside the container is before
				// the boundary.
				from = lastDescendant(container);
			}
		}
		walker.currentNode = from;
		for (let node = walker.nextNode(); node; node = walker.nextNode()) {
			if (usable(node)) return { node, offset: 0 };
		}
		return null;
	}

	// Backwards: the last text that ends at or before the boundary.
	let from: Node = container;
	if (!isText && offset > 0) {
		const child = container.childNodes[offset - 1];
		if (child) {
			const last = lastDescendant(child);
			if (usable(last)) return { node: last, offset: last.data.length };
			from = last;
		}
	}
	walker.currentNode = from;
	for (let node = walker.previousNode(); node; node = walker.previousNode()) {
		if (usable(node)) return { node, offset: node.data.length };
	}
	return null;
}

/**
 * A range with both ends moved onto text that a locator can name, or null
 * when nothing nameable lies between them.
 *
 * Offsets are counted over a block's TEXT, so a boundary has to be in a text
 * node inside a block before it can be stored. Browsers do not promise that.
 * Selecting a whole paragraph in Chrome ends the range at (next paragraph, 0),
 * an element boundary. Taken at face value, that end would name the END of the
 * following paragraph and highlight all of it. And pretty-printed XHTML puts
 * a "\n  " between blocks, directly inside a section or div, so a boundary
 * there has no block to anchor to at all.
 *
 * So a start moves forward and an end moves back, to the nearest text inside
 * a block. That changes which DOM position a selection maps to, never the
 * shape of the locator that position is written as.
 */
export function normaliseRange(range: Range, root: Element): Range | null {
	if (!root.contains(range.startContainer) || !root.contains(range.endContainer)) return null;

	const start = textPoint(range.startContainer, range.startOffset, true, root);
	const end = textPoint(range.endContainer, range.endOffset, false, root);
	if (!start || !end) return null;

	const out = document.createRange();
	try {
		out.setStart(start.node, start.offset);
		out.setEnd(end.node, end.offset);
	} catch {
		return null;
	}
	// A selection of nothing but the whitespace between two blocks moves its
	// start past its end, and setEnd then collapses the range.
	return out.collapsed ? null : out;
}

/**
 * The text of a range as a person would quote it: the whitespace a book's
 * markup carries collapsed, and a line break wherever the text crosses from
 * one block into another.
 *
 * Computed rather than taken from Selection.toString(), which differs between
 * engines (jsdom returns the raw markup whitespace, Chromium and Gecko each
 * approximate innerText in their own way), so the same selection saves the same
 * words from every reader.
 */
function rangeText(range: Range, root: Element): string {
	const lines: string[] = [];
	let block: Element | null | undefined;
	let line = '';
	let pre = false;
	const flush = () => {
		const text = pre ? line : line.replace(/\s+/g, ' ');
		if (text.trim()) lines.push(pre ? trimPreformatted(text) : text.trim());
		line = '';
	};

	const common = range.commonAncestorContainer;
	const nodes = common.nodeType === Node.TEXT_NODE ? [common as Text] : textNodes(common);
	for (const node of nodes) {
		if (node !== common && !range.intersectsNode(node)) continue;
		const from = node === range.startContainer ? range.startOffset : 0;
		const to = node === range.endContainer ? range.endOffset : node.data.length;
		const piece = node.data.slice(from, to);
		const owner = blockAncestor(node, root);
		// Formatting whitespace between blocks is not part of what was read.
		if (!owner && !piece.trim()) continue;
		if (owner !== block) {
			flush();
			block = owner;
			pre = !!owner?.closest('pre');
		}
		line += piece;
	}
	flush();
	return lines.join('\n');
}

/**
 * Preformatted text as it is quoted: the line breaks that open the block
 * dropped, and the whitespace that ends it. Indentation is the text's own and
 * stays.
 *
 * This was `text.replace(/^\n+|\s+$/g, '')`, which is quadratic on text a book
 * controls: `\s+$` is tried at every whitespace character and runs to the end
 * of the run before failing on whatever follows it, so n spaces and then a
 * letter cost n²/2 steps. 200,000 spaces inside a <pre> - a few hundred bytes
 * once zipped - held the page for thirteen seconds when somebody selected
 * across them. trimEnd strips exactly what `\s` matches (both are the
 * WhiteSpace and LineTerminator code points), scanning once from the end.
 */
function trimPreformatted(text: string): string {
	let start = 0;
	while (text.charCodeAt(start) === 10) start++;
	return text.slice(start).trimEnd();
}

/**
 * Converts a range into something storable, after moving its ends onto text a
 * locator can name (see normaliseRange).
 *
 * Returns null when nothing nameable is selected: a collapsed range, one
 * outside `root`, or one covering only whitespace or text outside any block.
 */
export function anchorFromRange(range: Range, root: Element): SelectionAnchor | null {
	if (range.collapsed) return null;
	const normal = normaliseRange(range, root);
	if (!normal) return null;

	const startBlock = blockAncestor(normal.startContainer, root);
	const endBlock = blockAncestor(normal.endContainer, root);
	if (!startBlock || !endBlock) return null;

	const text = rangeText(normal, root);
	if (!text) return null;

	return {
		startPath: elementPath(startBlock, root),
		startOffset: offsetWithin(startBlock, normal.startContainer, normal.startOffset),
		endPath: elementPath(endBlock, root),
		endOffset: offsetWithin(endBlock, normal.endContainer, normal.endOffset),
		text
	};
}

/**
 * Converts a live text selection into something storable.
 *
 * The selection itself is never changed. The web reader calls this on every
 * selectionchange to decide whether to offer Highlight, and moving the
 * reader's selection from inside that handler would fight their thumb.
 *
 * Returns null when the selection is empty or collapsed, or when nothing in it
 * can be named. Either way it is a click, not a highlight.
 */
export function anchorFromSelection(selection: Selection, root: Element): SelectionAnchor | null {
	if (selection.isCollapsed || selection.rangeCount === 0) return null;
	return anchorFromRange(selection.getRangeAt(0), root);
}

/**
 * Rebuilds a DOM range from a stored anchor, or null when it no longer
 * resolves.
 *
 * Returning null is the orphan case, and it must stay possible: a book can be
 * replaced by a different edition, and pretending an anchor still points
 * somewhere would put a highlight over the wrong words, which is worse than
 * showing it as orphaned.
 */
export function rangeFromAnchor(anchor: SelectionAnchor, root: Element): Range | null {
	const startBlock = elementAtPath(anchor.startPath, root);
	const endBlock = elementAtPath(anchor.endPath, root);
	if (!startBlock || !endBlock) return null;

	const startPoint = pointAt(startBlock, anchor.startOffset);
	const endPoint = pointAt(endBlock, anchor.endOffset);
	if (!startPoint || !endPoint) return null;

	const range = document.createRange();
	try {
		range.setStart(startPoint.node, startPoint.offset);
		range.setEnd(endPoint.node, endPoint.offset);
	} catch {
		return null;
	}
	if (range.collapsed) return null;
	return range;
}

function pointAt(block: Element, offset: number): { node: Text; offset: number } | null {
	let remaining = offset;
	const nodes = textNodes(block);
	for (const node of nodes) {
		if (remaining <= node.data.length) return { node, offset: remaining };
		remaining -= node.data.length;
	}
	// Past the end: clamp to the last position rather than failing, since a
	// trailing offset is usually a whitespace difference rather than a genuine
	// mismatch.
	const last = nodes[nodes.length - 1];
	return last ? { node: last, offset: last.data.length } : null;
}

/**
 * Wraps a range in highlight marks.
 *
 * A range may span several text nodes and several elements, so each intersected
 * text node is wrapped separately: wrapping the range whole would require
 * restructuring the DOM around it, which breaks any range that crosses an
 * element boundary.
 */
export function paintHighlight(range: Range, id: string, colour: string): boolean {
	const nodes: Text[] = [];
	const container = range.commonAncestorContainer;
	if (container.nodeType === Node.TEXT_NODE) {
		// A selection inside a single text node, which is the ordinary case. A
		// TreeWalker never yields its own root, so walking from here would find
		// nothing at all and every normal highlight would silently fail to paint.
		nodes.push(container as Text);
	} else {
		const walker = document.createTreeWalker(container, NodeFilter.SHOW_TEXT, {
			acceptNode(node) {
				return range.intersectsNode(node) ? NodeFilter.FILTER_ACCEPT : NodeFilter.FILTER_REJECT;
			}
		});
		let node = walker.nextNode();
		while (node) {
			nodes.push(node as Text);
			node = walker.nextNode();
		}
	}
	if (nodes.length === 0) return false;

	// The geometry is frozen before a single split happens. splitText moves live
	// ranges, so the moment we split the start node, range.startContainer and
	// range.endContainer are no longer the nodes we measured against, and every
	// later comparison silently reads the wrong boundary.
	const startContainer = range.startContainer;
	const startOffset = range.startOffset;
	const endContainer = range.endContainer;
	const endOffset = range.endOffset;

	const plan = nodes
		.map((text) => ({
			text,
			from: text === startContainer ? startOffset : 0,
			to: text === endContainer ? endOffset : text.data.length
		}))
		.filter((p) => p.to > p.from);

	let painted = false;
	for (const { text, from, to } of plan) {
		const piece = from > 0 ? text.splitText(from) : text;
		if (to - from < piece.data.length) piece.splitText(to - from);

		const mark = document.createElement('mark');
		mark.className = 'sm-highlight';
		mark.dataset.annotation = id;
		if (colour) mark.dataset.colour = colour;
		piece.parentNode?.insertBefore(mark, piece);
		mark.appendChild(piece);
		painted = true;
	}
	return painted;
}

/**
 * Removes every highlight mark, restoring the text around it.
 *
 * Painting has to be idempotent, because it runs on every page turn and every
 * repaint. Without this, each pass wraps text that a previous pass already
 * wrapped, so highlights nest inside each other and a deleted one stays on
 * screen until the chapter is loaded from scratch.
 */
export function unpaintHighlights(root: Element): void {
	for (const mark of [...root.querySelectorAll('mark.sm-highlight')]) {
		const parent = mark.parentNode;
		if (!parent) continue;
		while (mark.firstChild) parent.insertBefore(mark.firstChild, mark);
		parent.removeChild(mark);
	}
	// Re-merge what splitText left behind. Offsets are counted across a block's
	// text so fragmentation does not break them, but it accumulates on every
	// repaint and leaves the DOM further from the document being measured.
	root.normalize();
}
