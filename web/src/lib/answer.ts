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
 * bold, italics, and code spans - and nothing else. It is not a markdown parser and
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
	// Wrapped lines of one paragraph are joined with the newline kept. In a
	// paragraph it renders as the space it always did; kept, it is the line
	// break an italic must not run across.
	const flush = () => {
		if (prose.length > 0) out.push({ kind: 'p', text: prose.join('\n') });
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
				out.push({ kind: 'p', text: prose.join('\n') });
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

/** One run of text, possibly a citation marker, bold, italic, or a code span. */
export type Piece = {
	text: string;
	cite: number | null;
	bold?: boolean;
	italic?: boolean;
	code?: boolean;
};

/**
 * An italic: one pair of `*` or `_` around text, on one line.
 *
 * Strict on purpose, because both characters mean other things in an answer:
 *
 * - The text starts and ends with a non-space, so "2 * 3 * 4" and a bullet's
 *   "* " are not emphasis.
 * - Neither marker touches a letter or digit on its outer side, so "2*3*4"
 *   and "snake_case_name" are not either. (Markdown allows `*` inside a
 *   word; the arithmetic it would break is commoner in answers than the
 *   emphasis it would allow.)
 * - The inside holds no marker of its own kind and no line break, so one
 *   stray asterisk cannot italicise the rest of a paragraph, and a pair is
 *   never nested.
 *
 * No lookbehind: Safari before 16.4 cannot parse one, and the regular
 * expression is built for every answer, so it failed every answer on those
 * phones. What comes before the opening marker is captured instead - the
 * start, or a character that is not a letter, digit or the marker - and
 * handed back as text. The cost: an italic straight after something else
 * this pass took ("[2]*x*") has no character left to capture and stays as
 * written.
 */
const starItalic = String.raw`(^|[^\p{L}\p{N}*])\*(?![\s*])([^*\n]*?[^\s*])\*(?![\p{L}\p{N}*])`;
const underscoreItalic = String.raw`(^|[^\p{L}\p{N}_])_(?![\s_])([^_\n]*?[^\s_])_(?![\p{L}\p{N}_])`;

/**
 * Splits a line into pieces the template can render.
 *
 * Citation markers become links, `**text**` becomes bold, `*text*` and
 * `_text_` become italic, `` `text` `` becomes code. Everything is rendered
 * as a TEXT node - never with @html - because this is a language model's
 * output rendered in somebody's browser, and the one thing that must not be
 * possible is markup in an answer becoming markup on the page.
 */
export function segments(content: string): Piece[] {
	const out: Piece[] = [];
	// Citations, bold, code spans and italics, in one pass so their
	// boundaries cannot overlap into nonsense. Bold comes before the
	// italics, so "**a**" is bold and never an italic "*a*" in asterisks.
	// Code before the italics too: an italic's match starts one character
	// early, at the character before its marker, so "`_x_`" would otherwise
	// be read as an italic beginning at the backtick.
	const pattern = new RegExp(
		String.raw`\[(\d{1,3})\]|\*\*([^*]+)\*\*|` + '`([^`]+)`' + `|${starItalic}|${underscoreItalic}`,
		'gu'
	);

	let last = 0;
	let match: RegExpExecArray | null;
	while ((match = pattern.exec(content)) !== null) {
		const [, cite, bold, code, starLead, star, underscoreLead, underscore] = match;
		// An italic's match begins with the character before its marker,
		// which is ordinary text.
		const start = match.index + (starLead ?? underscoreLead ?? '').length;
		if (start > last) out.push({ text: content.slice(last, start), cite: null });
		if (cite !== undefined) {
			out.push({ text: match[0], cite: Number(cite) });
		} else if (bold !== undefined) {
			// A bolded heading can contain a citation - "**Scope [2]**" - and
			// the bold alternative matches the whole of it first. Splitting the
			// inside again keeps the chip. It cannot recurse further: the bold
			// pattern excludes asterisks, so the inside holds no more bold.
			for (const piece of segments(bold)) out.push({ ...piece, bold: true });
		} else if (star !== undefined || underscore !== undefined) {
			// Split again for the same reason: "*see [2]*" keeps its chip.
			// The inside has no marker of its own kind, so this ends.
			for (const piece of segments(star ?? underscore ?? '')) out.push({ ...piece, italic: true });
		} else if (code !== undefined) {
			out.push({ text: code, cite: null, code: true });
		}
		last = match.index + match[0].length;
	}
	if (last < content.length) out.push({ text: content.slice(last), cite: null });
	return out;
}

/** One book in a reading list, as the answer and a stored turn both carry it. */
export type ListedBook = { title: string; authors?: string[] };

/**
 * The text of a turn that answered with books.
 *
 * A reading list arrives as books rather than prose. The same text is built
 * for an answer as it arrives and for a turn read back from a reopened
 * thread, so the two cannot drift apart.
 */
export function withReadingList(answer: string, books: ListedBook[] | undefined): string {
	if (!books || books.length === 0) return answer;
	const list = books
		.map((b) => `- ${b.title}${b.authors?.length ? ' — ' + b.authors.join(', ') : ''}`)
		.join('\n');
	return (answer ? answer + '\n\n' : 'From your library:\n\n') + list;
}

/** What a question that could not be answered should say. */
export type AskFailure = {
	message: string;
	/** True when it is worth asking again in a while: nothing is broken. */
	busy: boolean;
};

/** Said when a proxy answered for the server, with no message of its own. */
export const UNREACHABLE = 'The server did not answer. Try again in a minute.';

/**
 * Turns a failed ask into what the person reads.
 *
 * The server already words the model's failures for people - "The assistant is
 * busy right now. Try again in a minute." - so those are shown as they come.
 * What changes is the tone: a busy model is weather, not a fault, and is not
 * shown in the colour of one. A proxy's own error page has no message worth
 * showing ("Bad Gateway"), so it gets one.
 */
export function askFailure(err: unknown): AskFailure {
	const code = typeof err === 'object' && err && 'code' in err ? String(err.code) : '';
	const message = err instanceof Error ? err.message : String(err);

	if (code === 'llm_unavailable') return { message, busy: true };
	if (code === 'http_502' || code === 'http_503' || code === 'http_504') {
		return { message: UNREACHABLE, busy: true };
	}
	return { message, busy: false };
}
