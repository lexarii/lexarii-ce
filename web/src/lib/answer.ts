// SPDX-License-Identifier: AGPL-3.0-or-later

// What an answer is made of, as far as the screen is concerned.
//
// Here rather than in the page component so it can be tested directly: it is
// parsing, it has edge cases, and it runs on the most-read screen in the
// application.

/** A paragraph, or a bullet list. */
export type Block = { kind: 'p'; text: string } | { kind: 'ul'; items: string[] };

/**
 * Splits an answer into paragraphs and bullet lists.
 *
 * The model writes markdown whether or not it is asked to, and the prompt
 * does not ask. Rendered as plain text it came out as literal asterisks -
 * "**Ownership and Scope**" - on the most-read screen in the application.
 *
 * Forbidding markdown in the prompt would have been the other fix, and it
 * is worse: a comparison answer is genuinely clearer as a list, and every
 * answer already stored would still be full of asterisks.
 *
 * This handles what the model actually produces - paragraphs, bullets,
 * bold, and code spans - and nothing else. It is not a markdown parser and
 * should not grow into one; anything it does not recognise stays as the
 * text it already was, which is the same behaviour as before.
 */
// A bullet is a star or dash with a space after it. The space matters: it is
// what separates "* Core concept" from "*Emphasis* opens this sentence".
const bullet = /^\s*[*-]\s+/;

export function blocks(content: string): Block[] {
	const out: Block[] = [];

	// Bullet lines run together into one list; everything between them runs
	// together into one paragraph. The model does not put a blank line between
	// a heading and the list under it - it writes
	//
	//     General definitions
	//     * Core concept: ...
	//     * Mutex as a semaphore: ...
	//
	// as a single block - so splitting on blank lines alone left the bullets
	// as literal asterisks in the middle of a paragraph, which is the bug this
	// is here to fix.
	let prose: string[] = [];
	let items: string[] = [];
	const flush = () => {
		if (prose.length > 0) out.push({ kind: 'p', text: prose.join(' ') });
		if (items.length > 0) out.push({ kind: 'ul', items });
		prose = [];
		items = [];
	};

	for (const raw of content.split('\n')) {
		const line = raw.trim();
		if (!line) {
			flush();
			continue;
		}
		if (bullet.test(line)) {
			// A paragraph above the list ends where the list starts.
			if (prose.length > 0) {
				out.push({ kind: 'p', text: prose.join(' ') });
				prose = [];
			}
			items.push(line.replace(bullet, ''));
			continue;
		}
		// And a list ends where prose resumes.
		if (items.length > 0) {
			out.push({ kind: 'ul', items });
			items = [];
		}
		prose.push(line);
	}
	flush();

	return out.length > 0 ? out : [{ kind: 'p', text: content }];
}

/** One run of text, possibly a citation marker, bold, or a code span. */
export type Piece = { text: string; cite: number | null; bold?: boolean; code?: boolean };

/**
 * Splits a line into pieces the template can render.
 *
 * Citation markers become links, `**text**` becomes bold, `` `text` ``
 * becomes code. Everything is rendered as a TEXT node - never with @html -
 * because this is a language model's output rendered in somebody's
 * browser, and the one thing that must not be possible is markup in an
 * answer becoming markup on the page.
 */
export function segments(content: string): Piece[] {
	const out: Piece[] = [];
	// Citations, bold, and code spans, in one pass so their boundaries
	// cannot overlap into nonsense.
	const pattern = /\[(\d{1,3})\]|\*\*([^*]+)\*\*|`([^`]+)`/g;

	let last = 0;
	let match: RegExpExecArray | null;
	while ((match = pattern.exec(content)) !== null) {
		if (match.index > last) out.push({ text: content.slice(last, match.index), cite: null });
		const [, cite, bold, code] = match;
		if (cite !== undefined) {
			out.push({ text: match[0], cite: Number(cite) });
		} else if (bold !== undefined) {
			// A bolded heading can contain a citation - "**Scope [2]**" - and
			// the bold alternative matches the whole of it first. Splitting the
			// inside again keeps the chip. It cannot recurse further: the bold
			// pattern excludes asterisks, so the inside holds no more bold.
			for (const piece of segments(bold)) out.push({ ...piece, bold: true });
		} else if (code !== undefined) {
			out.push({ text: code, cite: null, code: true });
		}
		last = match.index + match[0].length;
	}
	if (last < content.length) out.push({ text: content.slice(last), cite: null });
	return out;
}
