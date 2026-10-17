// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, expect, it } from 'vitest';
import { askFailure, blocks, segments, UNREACHABLE, withReadingList } from './answer';
import { ApiError } from '$api/client';
import answerSource from './answer.ts?raw';

describe('blocks', () => {
	it('keeps a plain answer as one paragraph', () => {
		expect(blocks('Wilde wrote it in 1890.')).toEqual([
			{ kind: 'p', text: 'Wilde wrote it in 1890.' }
		]);
	});

	it('separates paragraphs on blank lines', () => {
		expect(blocks('First.\n\nSecond.')).toEqual([
			{ kind: 'p', text: 'First.' },
			{ kind: 'p', text: 'Second.' }
		]);
	});

	it('joins a wrapped paragraph into one paragraph', () => {
		// A single newline inside a paragraph is the model wrapping, not a
		// break it meant. Rendering it as a break made answers ragged. The
		// newline is kept - inside a paragraph it renders as a space - so
		// an italic can tell where a line ended.
		expect(blocks('One sentence\nand its continuation.')).toEqual([
			{ kind: 'p', text: 'One sentence\nand its continuation.' }
		]);
	});

	it('turns a run of bullets into a list', () => {
		expect(blocks('* Ownership\n* Scope\n- Retention')).toEqual([
			{ kind: 'ul', items: ['Ownership', 'Scope', 'Retention'] }
		]);
	});

	it('does not treat a paragraph that begins with an asterisk as a list', () => {
		// Every line has to be a bullet. Otherwise a sentence beginning with
		// emphasis loses the rest of itself.
		expect(blocks('*Emphasis* opens this one\nand the line after is prose.')).toEqual([
			{ kind: 'p', text: '*Emphasis* opens this one\nand the line after is prose.' }
		]);
	});

	it('returns the content unchanged when there is nothing to split', () => {
		// Including while streaming, when content is briefly empty or a
		// fragment: the answer must never vanish because it is half-written.
		expect(blocks('')).toEqual([{ kind: 'p', text: '' }]);
		expect(blocks('**Own')).toEqual([{ kind: 'p', text: '**Own' }]);
	});
});

describe('segments', () => {
	it('leaves text without markers alone', () => {
		expect(segments('plain')).toEqual([{ text: 'plain', cite: null }]);
	});

	it('marks citations, keeping the bracket text for the chip', () => {
		expect(segments('He says so [3].')).toEqual([
			{ text: 'He says so ', cite: null },
			{ text: '[3]', cite: 3 },
			{ text: '.', cite: null }
		]);
	});

	it('unwraps bold and code, dropping their markers', () => {
		expect(segments('**Scope** and `--reranking`')).toEqual([
			{ text: 'Scope', cite: null, bold: true },
			{ text: ' and ', cite: null },
			{ text: '--reranking', cite: null, code: true }
		]);
	});

	it('does not read a citation out of a year or a longer number', () => {
		// Three digits is the cap: passage lists do not reach a thousand, and
		// [1890] in a quoted title is not a citation.
		expect(segments('[1890]')).toEqual([{ text: '[1890]', cite: null }]);
	});

	it('leaves an unclosed marker as the text it already was', () => {
		// Arrives constantly while streaming.
		expect(segments('**half')).toEqual([{ text: '**half', cite: null }]);
		expect(segments('`half')).toEqual([{ text: '`half', cite: null }]);
	});

	it('keeps a citation clickable inside a bold heading', () => {
		// The bold alternative matches the whole of "**Scope [2]**" first, so
		// without splitting the inside again the chip would be lost to bold
		// text reading "Scope [2]".
		expect(segments('**Scope [2]**')).toEqual([
			{ text: 'Scope ', cite: null, bold: true },
			{ text: '[2]', cite: 2, bold: true }
		]);
	});
});

describe('segments, italics', () => {
	it('unwraps an asterisk pair and an underscore pair', () => {
		expect(segments('It is *not* the count, it is _Jonathan_.')).toEqual([
			{ text: 'It is ', cite: null },
			{ text: 'not', cite: null, italic: true },
			{ text: ' the count, it is ', cite: null },
			{ text: 'Jonathan', cite: null, italic: true },
			{ text: '.', cite: null }
		]);
	});

	it('italicises a title of several words, and a single letter', () => {
		expect(segments('*Pride and Prejudice* and *x*')).toEqual([
			{ text: 'Pride and Prejudice', cite: null, italic: true },
			{ text: ' and ', cite: null },
			{ text: 'x', cite: null, italic: true }
		]);
	});

	it('keeps a citation clickable inside an italic', () => {
		expect(segments('*see [2]*')).toEqual([
			{ text: 'see ', cite: null, italic: true },
			{ text: '[2]', cite: 2, italic: true }
		]);
	});

	it('keeps bold as bold, and an italic inside it', () => {
		expect(segments('**Scope** and **the _Count_**')).toEqual([
			{ text: 'Scope', cite: null, bold: true },
			{ text: ' and ', cite: null },
			{ text: 'the ', cite: null, bold: true },
			{ text: 'Count', cite: null, italic: true, bold: true }
		]);
	});

	it('does not read arithmetic as emphasis', () => {
		for (const sum of ['2*3*4', '2 * 3 * 4', 'a*b + c*d', '(2*3)*4', '5 * x*y']) {
			expect(segments(sum)).toEqual([{ text: sum, cite: null }]);
		}
	});

	it('does not read an underscore inside a word as emphasis', () => {
		for (const name of ['snake_case_name', 'call do_this_now', '__init__', 'a_b_']) {
			expect(segments(name)).toEqual([{ text: name, cite: null }]);
		}
	});

	it('does not read a bullet marker as emphasis', () => {
		expect(segments('* Core concept * and more')).toEqual([
			{ text: '* Core concept * and more', cite: null }
		]);
	});

	it('does not run an italic across a line break', () => {
		const text = 'the *first\nsecond* line';
		expect(segments(text)).toEqual([{ text, cite: null }]);
		// And blocks keeps the break for it to see.
		expect(blocks(text)).toEqual([{ kind: 'p', text }]);
	});

	it('takes one pair at a time, never nesting', () => {
		expect(segments('*a* b *c*')).toEqual([
			{ text: 'a', cite: null, italic: true },
			{ text: ' b ', cite: null },
			{ text: 'c', cite: null, italic: true }
		]);
		expect(segments('**')).toEqual([{ text: '**', cite: null }]);
		expect(segments('* *')).toEqual([{ text: '* *', cite: null }]);
	});

	it('leaves an unclosed italic as the text it already was', () => {
		// Arrives constantly while streaming.
		expect(segments('*half')).toEqual([{ text: '*half', cite: null }]);
		expect(segments('_half')).toEqual([{ text: '_half', cite: null }]);
	});

	it('hands the character before an italic back as text', () => {
		expect(segments('(*a*) "_b_"')).toEqual([
			{ text: '(', cite: null },
			{ text: 'a', cite: null, italic: true },
			{ text: ') "', cite: null },
			{ text: 'b', cite: null, italic: true },
			{ text: '"', cite: null }
		]);
		expect(segments('*a* *b*')).toEqual([
			{ text: 'a', cite: null, italic: true },
			{ text: ' ', cite: null },
			{ text: 'b', cite: null, italic: true }
		]);
	});

	it('uses no lookbehind, which Safari before 16.4 cannot parse', () => {
		expect(answerSource).not.toMatch(/\(\?<[=!]/);
	});

	it('does not look for emphasis inside code', () => {
		expect(segments('`a*b*c` and `_x_`')).toEqual([
			{ text: 'a*b*c', cite: null, code: true },
			{ text: ' and ', cite: null },
			{ text: '_x_', cite: null, code: true }
		]);
	});
});

describe('blocks, on what the model actually writes', () => {
	it('separates a heading from the list beneath it', () => {
		// Taken from a real answer. There is no blank line between the two, so
		// splitting on blank lines alone left "* Core concept" as literal
		// asterisks in the middle of a paragraph.
		expect(
			blocks('General definitions\n* Core concept: a semaphore [10].\n* Mutex: a binary one.')
		).toEqual([
			{ kind: 'p', text: 'General definitions' },
			{ kind: 'ul', items: ['Core concept: a semaphore [10].', 'Mutex: a binary one.'] }
		]);
	});

	it('resumes prose after a list', () => {
		expect(blocks('Before\n- one\n- two\nAfter')).toEqual([
			{ kind: 'p', text: 'Before' },
			{ kind: 'ul', items: ['one', 'two'] },
			{ kind: 'p', text: 'After' }
		]);
	});
});

describe('askFailure', () => {
	const busy = 'The assistant is busy right now. Try again in a minute.';

	it('shows a busy model as the server words it, and as a wait', () => {
		expect(askFailure(new ApiError('llm_unavailable', busy, 503))).toEqual({
			message: busy,
			busy: true
		});
	});

	it('shows a refusing model server as a fault', () => {
		const message = 'The assistant could not answer: its model server refused the request.';
		expect(askFailure(new ApiError('model_error', message, 502))).toEqual({
			message,
			busy: false
		});
	});

	it('words a proxy error page, which has no message of its own', () => {
		for (const status of [502, 503, 504]) {
			expect(askFailure(new ApiError('http_' + status, 'Bad Gateway', status))).toEqual({
				message: UNREACHABLE,
				busy: true
			});
		}
	});

	it('passes anything else through', () => {
		expect(askFailure(new ApiError('not_found', 'conversation not found', 404))).toEqual({
			message: 'conversation not found',
			busy: false
		});
		expect(askFailure(new Error('network down'))).toEqual({ message: 'network down', busy: false });
		expect(askFailure('odd')).toEqual({ message: 'odd', busy: false });
	});
});

describe('withReadingList', () => {
	const holmes = { title: 'The Adventures of Sherlock Holmes', authors: ['Arthur Conan Doyle'] };

	it('leaves an answer with no books alone', () => {
		expect(withReadingList('Moriarty [1].', undefined)).toBe('Moriarty [1].');
		expect(withReadingList('Moriarty [1].', [])).toBe('Moriarty [1].');
	});

	it('introduces a bare list', () => {
		expect(withReadingList('', [holmes, { title: 'Untitled' }])).toBe(
			'From your library:\n\n- The Adventures of Sherlock Holmes — Arthur Conan Doyle\n- Untitled'
		);
	});

	it('keeps what the answer said above the list', () => {
		expect(withReadingList('Found from their text:', [holmes])).toBe(
			'Found from their text:\n\n- The Adventures of Sherlock Holmes — Arthur Conan Doyle'
		);
	});
});
