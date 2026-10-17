// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, expect, it } from 'vitest';
import { askFailure, blocks, segments, UNREACHABLE } from './answer';
import { ApiError } from '$api/client';

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

	it('joins a wrapped paragraph into one line', () => {
		// A single newline inside a paragraph is the model wrapping, not a
		// break it meant. Rendering it as a break made answers ragged.
		expect(blocks('One sentence\nand its continuation.')).toEqual([
			{ kind: 'p', text: 'One sentence and its continuation.' }
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
			{ kind: 'p', text: '*Emphasis* opens this one and the line after is prose.' }
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
