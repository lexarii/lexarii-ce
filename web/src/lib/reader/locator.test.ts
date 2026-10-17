// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect, beforeEach } from 'vitest';

import {
	anchorFromRange,
	anchorFromSelection,
	elementAtPath,
	elementPath,
	normaliseRange,
	paintHighlight,
	pendingImages,
	rangeFromAnchor,
	unpaintHighlights,
	whenLoaded,
	type SelectionAnchor
} from './locator';

/** Builds a chapter fragment of the shape the reader actually renders. */
function chapter(html: string): HTMLElement {
	const flow = document.createElement('div');
	flow.className = 'flow';
	flow.innerHTML = html;
	document.body.replaceChildren(flow);
	return flow;
}

const SAMPLE = `
	<section>
		<div>
			<p>Your continuous delivery build pipeline should be the primary location.</p>
			<p>However, these <em>automated</em> quality assertions should not replace discussion.</p>
		</div>
	</section>
`;

function marks(root: Element): string[] {
	return [...root.querySelectorAll('mark.sm-highlight')].map((m) => m.textContent ?? '');
}

describe('paintHighlight', () => {
	let flow: HTMLElement;

	beforeEach(() => {
		flow = chapter(SAMPLE);
	});

	// The regression this file exists for.
	//
	// A selection inside one paragraph has that paragraph's TEXT NODE as its
	// common ancestor, and a TreeWalker never yields its own root. Walking from
	// the common ancestor therefore found nothing, and every ordinary highlight
	// silently painted nothing at all while reporting success everywhere else.
	it('paints a selection that lies inside a single text node', () => {
		const p = flow.querySelector('p')!;
		const text = p.firstChild as Text;

		const range = document.createRange();
		range.setStart(text, 5);
		range.setEnd(text, 26);
		expect(range.commonAncestorContainer.nodeType).toBe(Node.TEXT_NODE);

		expect(paintHighlight(range, 'mark-1', 'amber')).toBe(true);
		expect(marks(flow).join('')).toBe('continuous delivery b');
	});

	it('paints a selection spanning several elements, one mark per text node', () => {
		const paragraphs = [...flow.querySelectorAll('p')];
		const range = document.createRange();
		range.setStart(paragraphs[0]!.firstChild as Text, 5);
		range.setEnd(paragraphs[1]!.lastChild as Text, 10);

		expect(paintHighlight(range, 'mark-2', '')).toBe(true);
		// Several marks, but together they are exactly the selected text.
		expect(marks(flow).length).toBeGreaterThan(1);
		expect(marks(flow).join('')).toBe(range.toString());
	});

	it('records the annotation id and colour on every piece', () => {
		const paragraphs = [...flow.querySelectorAll('p')];
		const range = document.createRange();
		range.setStart(paragraphs[0]!.firstChild as Text, 0);
		range.setEnd(paragraphs[1]!.lastChild as Text, 5);
		paintHighlight(range, 'mark-3', 'rose');

		for (const mark of flow.querySelectorAll('mark.sm-highlight')) {
			expect((mark as HTMLElement).dataset.annotation).toBe('mark-3');
			expect((mark as HTMLElement).dataset.colour).toBe('rose');
		}
	});

	it('leaves the document text unchanged', () => {
		const before = flow.textContent;
		const p = flow.querySelector('p')!;
		const range = document.createRange();
		range.setStart(p.firstChild as Text, 5);
		range.setEnd(p.firstChild as Text, 26);
		paintHighlight(range, 'mark-4', '');

		expect(flow.textContent).toBe(before);
	});
});

describe('unpaintHighlights', () => {
	// Painting runs on every page turn, so it has to be repeatable. Without a
	// clean slate each pass wrapped text a previous pass had already wrapped,
	// nesting highlights and leaving deleted ones on screen.
	it('restores the original markup and text', () => {
		const flow = chapter(SAMPLE);
		const before = flow.innerHTML;
		const textBefore = flow.textContent;

		const p = flow.querySelector('p')!;
		const range = document.createRange();
		range.setStart(p.firstChild as Text, 5);
		range.setEnd(p.firstChild as Text, 26);
		paintHighlight(range, 'mark-5', 'amber');
		expect(flow.querySelectorAll('mark.sm-highlight')).toHaveLength(1);

		unpaintHighlights(flow);
		expect(flow.querySelectorAll('mark.sm-highlight')).toHaveLength(0);
		expect(flow.textContent).toBe(textBefore);
		expect(flow.innerHTML).toBe(before);
	});

	it('is safe on a document with no highlights', () => {
		const flow = chapter(SAMPLE);
		const before = flow.innerHTML;
		unpaintHighlights(flow);
		expect(flow.innerHTML).toBe(before);
	});

	it('survives repeated paint and unpaint without nesting or drift', () => {
		const flow = chapter(SAMPLE);
		const anchor: SelectionAnchor = {
			startPath: 'section[1]/div[1]/p[1]',
			startOffset: 5,
			endPath: 'section[1]/div[1]/p[1]',
			endOffset: 26,
			text: ''
		};
		const original = flow.innerHTML;

		for (let i = 0; i < 5; i++) {
			unpaintHighlights(flow);
			const range = rangeFromAnchor(anchor, flow);
			expect(range).not.toBeNull();
			expect(paintHighlight(range!, 'mark-6', 'amber')).toBe(true);

			expect(flow.querySelectorAll('mark.sm-highlight mark.sm-highlight')).toHaveLength(0);
			expect(marks(flow).join('')).toBe('continuous delivery b');
		}

		unpaintHighlights(flow);
		expect(flow.innerHTML).toBe(original);
	});
});

describe('rangeFromAnchor', () => {
	it('resolves an anchor to the text it was made against', () => {
		const flow = chapter(SAMPLE);
		const range = rangeFromAnchor(
			{
				startPath: 'section[1]/div[1]/p[1]',
				startOffset: 5,
				endPath: 'section[1]/div[1]/p[1]',
				endOffset: 26,
				text: ''
			},
			flow
		);
		expect(range?.toString()).toBe('continuous delivery b');
	});

	// Offsets are counted over a block's TEXT, not its markup, so a highlight
	// survives the inline elements around it changing - which a re-ingest or a
	// different sanitiser pass can do.
	it('resolves the same text after inline markup around it changes', () => {
		const flow = chapter(SAMPLE);
		const anchor: SelectionAnchor = {
			startPath: 'section[1]/div[1]/p[2]',
			startOffset: 15,
			endPath: 'section[1]/div[1]/p[2]',
			endOffset: 24,
			text: ''
		};
		expect(rangeFromAnchor(anchor, flow)?.toString()).toBe('automated');

		// The same words, with the <em> gone and a <strong> elsewhere.
		const rewrapped = chapter(`
			<section>
				<div>
					<p>Your continuous delivery build pipeline should be the primary location.</p>
					<p>However, <strong>these</strong> automated quality assertions should not replace discussion.</p>
				</div>
			</section>
		`);
		expect(rangeFromAnchor(anchor, rewrapped)?.toString()).toBe('automated');
	});

	// The orphan case, which must stay possible: a book can be replaced by a
	// different edition, and painting over the wrong words would be worse than
	// showing the mark as orphaned.
	it('returns null when the anchor no longer resolves', () => {
		const flow = chapter(SAMPLE);
		const gone = rangeFromAnchor(
			{
				startPath: 'section[1]/div[1]/p[99]',
				startOffset: 0,
				endPath: 'section[1]/div[1]/p[99]',
				endOffset: 10,
				text: ''
			},
			flow
		);
		expect(gone).toBeNull();
	});

	it('returns null for an anchor that collapses to nothing', () => {
		const flow = chapter(SAMPLE);
		const empty = rangeFromAnchor(
			{
				startPath: 'section[1]/div[1]/p[1]',
				startOffset: 5,
				endPath: 'section[1]/div[1]/p[1]',
				endOffset: 5,
				text: ''
			},
			flow
		);
		expect(empty).toBeNull();
	});
});

describe('elementPath', () => {
	it('round-trips through elementAtPath', () => {
		const flow = chapter(SAMPLE);
		for (const el of flow.querySelectorAll('p, section, div, em')) {
			const path = elementPath(el, flow);
			expect(elementAtPath(path, flow), path).toBe(el);
		}
	});

	it('distinguishes siblings of the same tag', () => {
		const flow = chapter(SAMPLE);
		const paragraphs = [...flow.querySelectorAll('p')];
		expect(elementPath(paragraphs[0]!, flow)).not.toBe(elementPath(paragraphs[1]!, flow));
	});
});

/**
 * Pretty-printed XHTML, the shape most EPUBs arrive in: whitespace text
 * directly inside the section and the divs, blocks inside wrappers, and one
 * run of text that belongs to no block at all.
 */
const WRAPPED =
	'<section>\n  <h2>Wrapped</h2>\n  <div class="body">\n    <p>Alpha one <em>two</em> three.</p>\n' +
	'    <p>Beta four five six.</p>\n  </div>\n  <div>\n    <p>Gamma seven <b>eight</b>.</p>\n' +
	'  </div>\n  loose words\n  <p>Delta nine.</p>\n</section>\n';

describe('anchorFromRange', () => {
	let flow: HTMLElement;
	let alpha: Element;
	let beta: Element;
	let gamma: Element;
	let body: Element;
	let loose: Text;

	const texts = (el: Node): Text[] => {
		const out: Text[] = [];
		const walker = document.createTreeWalker(el, NodeFilter.SHOW_TEXT);
		for (let n = walker.nextNode(); n; n = walker.nextNode()) out.push(n as Text);
		return out;
	};
	const range = (sc: Node, so: number, ec: Node, eo: number): Range => {
		const r = document.createRange();
		r.setStart(sc, so);
		r.setEnd(ec, eo);
		return r;
	};
	/** Stores the range, resolves it again, and returns what comes back. */
	const roundTrip = (r: Range): { anchor: SelectionAnchor | null; back: string | null } => {
		const anchor = anchorFromRange(r, flow);
		const back = anchor ? (rangeFromAnchor(anchor, flow)?.toString() ?? null) : null;
		return { anchor, back };
	};
	const squash = (s: string | null | undefined) => (s ?? '').replace(/\s+/g, ' ').trim();

	beforeEach(() => {
		flow = chapter(WRAPPED);
		alpha = elementAtPath('section[1]/div[1]/p[1]', flow)!;
		beta = elementAtPath('section[1]/div[1]/p[2]', flow)!;
		gamma = elementAtPath('section[1]/div[2]/p[1]', flow)!;
		body = elementAtPath('section[1]/div[1]', flow)!;
		loose = [...elementAtPath('section[1]', flow)!.childNodes].find(
			(n): n is Text => n.nodeType === Node.TEXT_NODE && /loose/.test((n as Text).data)
		)!;
	});

	// The TODO entry this exists for. Chrome ends a whole-paragraph selection
	// at (next paragraph, 0); read as-is, the end names the END of the next
	// paragraph and the mark covers both.
	it('ends a whole-paragraph selection at the end of that paragraph, not the next', () => {
		const { anchor, back } = roundTrip(range(alpha, 0, beta, 0));
		expect(anchor).toMatchObject({
			startPath: 'section[1]/div[1]/p[1]',
			startOffset: 0,
			endPath: 'section[1]/div[1]/p[1]',
			endOffset: alpha.textContent!.length
		});
		expect(back).toBe(alpha.textContent);
		expect(anchor!.text).toBe('Alpha one two three.');
	});

	it('handles the triple-click form that ends in the next wrapper', () => {
		const { anchor, back } = roundTrip(range(texts(beta)[0]!, 0, gamma, 0));
		expect(anchor?.endPath).toBe('section[1]/div[1]/p[2]');
		expect(back).toBe(beta.textContent);
	});

	it('handles a paragraph selected as (wrapper, i) to (wrapper, i + 1)', () => {
		const i = [...body.childNodes].indexOf(alpha as ChildNode);
		const { back } = roundTrip(range(body, i, body, i + 1));
		expect(back).toBe(alpha.textContent);
	});

	it('spans two paragraphs and a wrapper boundary, with a line break between them', () => {
		const { anchor, back } = roundTrip(range(alpha, 0, gamma, 0));
		expect(anchor?.startPath).toBe('section[1]/div[1]/p[1]');
		expect(anchor?.endPath).toBe('section[1]/div[1]/p[2]');
		expect(squash(back)).toBe(squash(alpha.textContent + ' ' + beta.textContent));
		expect(anchor!.text).toBe('Alpha one two three.\nBeta four five six.');
	});

	it('ends mid-paragraph-to-next-start at the end of the first', () => {
		const { back } = roundTrip(range(texts(alpha)[0]!, 6, beta, 0));
		expect(back).toBe(alpha.textContent!.slice(6));
	});

	it('moves a start in the whitespace between blocks forward into the next block', () => {
		const between = alpha.nextSibling as Text;
		expect(between.data.trim()).toBe('');
		const { anchor, back } = roundTrip(range(between, 2, texts(beta)[0]!, 9));
		expect(anchor).toMatchObject({ startPath: 'section[1]/div[1]/p[2]', startOffset: 0 });
		expect(back).toBe('Beta four');
	});

	it('moves a start in the whitespace before the first block of a wrapper forward', () => {
		const before = alpha.previousSibling as Text;
		const { back } = roundTrip(range(before, 1, texts(alpha)[0]!, 5));
		expect(back).toBe('Alpha');
	});

	it('moves an end in the whitespace between blocks back to the block before it', () => {
		const between = alpha.nextSibling as Text;
		const { anchor, back } = roundTrip(range(texts(alpha)[0]!, 6, between, 3));
		expect(anchor?.endPath).toBe('section[1]/div[1]/p[1]');
		expect(back).toBe(alpha.textContent!.slice(6));
	});

	it('moves an end in text outside any block back to the last block', () => {
		const { anchor, back } = roundTrip(range(texts(gamma)[0]!, 0, loose, 4));
		expect(anchor?.endPath).toBe('section[1]/div[2]/p[1]');
		expect(back).toBe(gamma.textContent);
	});

	it('leaves a boundary already inside a block exactly where it was', () => {
		const t = texts(alpha);
		const { anchor } = roundTrip(range(t[0]!, 3, t[1]!, 2));
		expect(anchor).toMatchObject({ startOffset: 3, endOffset: 'Alpha one '.length + 2 });
		expect(anchor?.text).toBe('ha one tw');
	});

	it('refuses a range of nothing but whitespace between blocks', () => {
		const between = alpha.nextSibling as Text;
		expect(anchorFromRange(range(between, 0, between, 4), flow)).toBeNull();
	});

	it('refuses a range of nothing but text outside any block', () => {
		expect(anchorFromRange(range(loose, 3, loose, 12), flow)).toBeNull();
	});

	it('refuses a range reaching outside the root', () => {
		const outside = document.createElement('p');
		outside.textContent = 'elsewhere';
		document.body.appendChild(outside);
		expect(anchorFromRange(range(texts(alpha)[0]!, 0, outside.firstChild!, 4), flow)).toBeNull();
	});

	// The format is the one thing this must not change: only which DOM
	// position a selection maps to.
	it('produces paths and offsets in the same shape as before', () => {
		const anchor = anchorFromRange(range(alpha, 0, beta, 0), flow)!;
		expect(anchor.startPath).toMatch(/^([a-z0-9-]+\[\d+\]\/)*[a-z0-9-]+\[\d+\]$/);
		expect(Number.isInteger(anchor.startOffset)).toBe(true);
		expect(Number.isInteger(anchor.endOffset)).toBe(true);
	});
});

describe('normaliseRange', () => {
	it('does not touch the range it was given', () => {
		const flow = chapter(WRAPPED);
		const alpha = elementAtPath('section[1]/div[1]/p[1]', flow)!;
		const beta = elementAtPath('section[1]/div[1]/p[2]', flow)!;
		const r = document.createRange();
		r.setStart(alpha, 0);
		r.setEnd(beta, 0);
		const out = normaliseRange(r, flow)!;
		expect(r.endContainer).toBe(beta);
		expect(out.endContainer.nodeType).toBe(Node.TEXT_NODE);
		expect(out.toString()).toBe(alpha.textContent);
	});
});

describe('anchorFromSelection', () => {
	it('reads the selection without changing it', () => {
		const flow = chapter(WRAPPED);
		const alpha = elementAtPath('section[1]/div[1]/p[1]', flow)!;
		const beta = elementAtPath('section[1]/div[1]/p[2]', flow)!;
		const r = document.createRange();
		r.setStart(alpha, 0);
		r.setEnd(beta, 0);
		const selection = window.getSelection()!;
		selection.removeAllRanges();
		selection.addRange(r);

		const anchor = anchorFromSelection(selection, flow);
		expect(anchor?.endPath).toBe('section[1]/div[1]/p[1]');
		expect(selection.getRangeAt(0).endContainer).toBe(beta);
		expect(selection.getRangeAt(0).endOffset).toBe(0);
	});

	it('is null for a collapsed selection', () => {
		const flow = chapter(WRAPPED);
		const selection = window.getSelection()!;
		selection.removeAllRanges();
		selection.collapse(flow.querySelector('p')!.firstChild, 2);
		expect(anchorFromSelection(selection, flow)).toBeNull();
	});
});

/*
 * The text a selection saves, on input a book controls.
 *
 * Preformatted text used to be trimmed with /^\n+|\s+$/g. `\s+$` is tried at
 * every whitespace character and runs to the end of the run before failing,
 * so n spaces followed by anything else cost n²/2 steps: 80,000 of them in a
 * <pre> froze the page for over three seconds. The regex stays here as the
 * reference the replacement must agree with, on inputs small enough for it.
 */
describe('the text of a selection, on hostile input', () => {
	const reference = (s: string) => s.replace(/^\n+|\s+$/g, '');

	/** Selects the whole of `nodes` inside a <pre> and returns the saved text. */
	function quotePre(...nodes: string[]): string | null {
		const flow = chapter('<pre></pre>');
		const pre = flow.querySelector('pre')!;
		for (const [i, data] of nodes.entries()) {
			if (i % 2) {
				const code = document.createElement('code');
				code.textContent = data;
				pre.append(code);
			} else {
				pre.append(data);
			}
		}
		const r = document.createRange();
		r.selectNodeContents(pre);
		return anchorFromRange(r, flow)?.text ?? null;
	}

	/** Deterministic, so a failure names an input that can be replayed. */
	function* strings(count: number): Generator<string> {
		const alphabet = ['\n', '\r', ' ', '\t', ' ', ' ', ' ', '﻿', 'a', 'b'];
		let seed = 0x2545f491;
		const next = () => {
			seed ^= seed << 13;
			seed ^= seed >>> 17;
			seed ^= seed << 5;
			return seed >>> 0;
		};
		for (let i = 0; i < count; i++) {
			let s = '';
			for (let n = next() % 14; n > 0; n--) s += alphabet[next() % alphabet.length];
			yield s;
		}
	}

	it('trims preformatted text exactly as the regex did', () => {
		const cases = [
			'int x;',
			'\n\nint x;\n  ',
			'\n  indented\n\tline\n\n',
			'  leading spaces stay',
			'x  ﻿',
			'\r\nx\r\n',
			' \n x',
			'a\n\nb',
			...strings(2000)
		];
		for (const s of cases) {
			const want = reference(s).trim() ? reference(s) : null;
			expect(quotePre(s), JSON.stringify(s)).toBe(want);
		}
		// 2,000 small DOM chapters: half a second on a laptop, past vitest's
		// 5s default on a slow CI runner. Correctness, not speed, is the point.
	}, 30_000);

	it('trims a <pre> made of several nodes as one', () => {
		const parts = ['\n', 'int main() {', '\n  return 0;\n', '}', '\n   \n'];
		expect(quotePre(...parts)).toBe(reference(parts.join('')));
	});

	// A bound, not a benchmark: the old trim took tens of seconds here, a
	// linear one takes milliseconds, and a second leaves room for slow CI.
	const spaces = ' '.repeat(200_000);
	const mixed = ' \t\n'.repeat(70_000);
	const breaks = '\n'.repeat(200_000);
	it.each([
		['spaces then text', 'a' + spaces + 'b', 'a' + spaces + 'b'],
		['mixed whitespace then text', 'a' + mixed + 'b', 'a' + mixed + 'b'],
		['line breaks then text', breaks + 'b', 'b'],
		['text then whitespace', 'a' + mixed, 'a']
	])('stays linear on %s in a <pre>', (_, hostile, want) => {
		const started = performance.now();
		const text = quotePre(hostile);
		expect(performance.now() - started).toBeLessThan(1000);
		expect(text).toBe(want);
	});

	it('stays linear on the same input in a paragraph', () => {
		const flow = chapter('<p></p>');
		const p = flow.querySelector('p')!;
		p.textContent = 'a' + ' \t\n'.repeat(70_000) + 'b';
		const r = document.createRange();
		r.selectNodeContents(p);
		const started = performance.now();
		expect(anchorFromRange(r, flow)?.text).toBe('a b');
		expect(performance.now() - started).toBeLessThan(1000);
	});
});

describe('pendingImages', () => {
	// jsdom never fetches, so an <img> with a src stays incomplete until a
	// test says otherwise - which is what a slow picture looks like.
	const loaded = (img: HTMLImageElement) => Object.defineProperty(img, 'complete', { value: true });

	const BOOK = `
		<h2 id="c1"><img id="i1" src="a.jpg">CHAPTER I.</h2>
		<p id="p1">First.</p>
		<p><img id="i2" src="b.jpg"></p>
		<p id="p2">Second.</p>
		<img id="i3" src="c.jpg">
		<p id="p3">Third.</p>
		<img id="blocked" data-blocked="1" alt="gone">`;

	const ids = (list: Element[]) => list.map((e) => e.id);

	// The reported bug: a position restored before the pictures above it
	// had loaded landed pages early. Only those pictures can move it.
	it('waits for the pictures before the target, not after it', () => {
		const flow = chapter(BOOK);
		expect(ids(pendingImages(flow, flow.querySelector('#p2')))).toEqual(['i1', 'i2']);
		expect(ids(pendingImages(flow, flow.querySelector('#p1')))).toEqual(['i1']);
	});

	// A chapter heading with its illustration inside it is Gutenberg's
	// usual shape, and the picture decides where the heading's text falls.
	it('waits for a picture inside the target', () => {
		const flow = chapter(BOOK);
		expect(ids(pendingImages(flow, flow.querySelector('#c1')))).toEqual(['i1']);
	});

	it('waits for every picture when landing at the end', () => {
		const flow = chapter(BOOK);
		expect(ids(pendingImages(flow, null))).toEqual(['i1', 'i2', 'i3']);
	});

	// A picture already there cannot move anything; one with no src never
	// will load.
	it('skips what has loaded, and what never will', () => {
		const flow = chapter(BOOK);
		loaded(flow.querySelector<HTMLImageElement>('#i1')!);
		expect(ids(pendingImages(flow, flow.querySelector('#p3')))).toEqual(['i2', 'i3']);
	});
});

describe('whenLoaded', () => {
	it('resolves once every picture has loaded or failed', async () => {
		const flow = chapter('<img id="a" src="a.jpg"><img id="b" src="b.jpg">');
		const [a, b] = [...flow.querySelectorAll('img')];
		let done = false;
		const waiting = whenLoaded([a!, b!], 60_000).then(() => (done = true));

		a!.dispatchEvent(new Event('load'));
		await Promise.resolve();
		expect(done).toBe(false);

		b!.dispatchEvent(new Event('error'));
		await waiting;
		expect(done).toBe(true);
	});

	// A picture that never arrives must not keep somebody from their page.
	it('gives up after the time allowed', async () => {
		const flow = chapter('<img src="never.jpg">');
		const started = Date.now();
		await whenLoaded([...flow.querySelectorAll('img')], 30);
		expect(Date.now() - started).toBeLessThan(2000);
	});

	it('resolves at once with nothing to wait for', async () => {
		await expect(whenLoaded([], 60_000)).resolves.toBeUndefined();
	});
});
