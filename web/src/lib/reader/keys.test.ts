// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, expect, it } from 'vitest';
import { isTyping } from './keys';

describe('isTyping', () => {
	// The incident this exists for: the page-turn keys are global, so without
	// this guard space turned the page instead of typing a space, and a note
	// could not contain one.
	it('leaves the fields a reader types into alone', () => {
		for (const tag of ['input', 'textarea', 'select']) {
			expect(isTyping(document.createElement(tag))).toBe(true);
		}

		const editable = document.createElement('div');
		editable.contentEditable = 'true';
		// jsdom does not derive isContentEditable from the attribute.
		Object.defineProperty(editable, 'isContentEditable', { value: true });
		expect(isTyping(editable)).toBe(true);
	});

	it('does not swallow keystrokes aimed at the book', () => {
		expect(isTyping(document.createElement('div'))).toBe(false);
		expect(isTyping(document.createElement('button'))).toBe(false);
		expect(isTyping(document.createElement('mark'))).toBe(false);
	});

	// A keydown with no target at all still has to turn the page.
	it('treats nothing as not typing', () => {
		expect(isTyping(null)).toBe(false);
	});
});
