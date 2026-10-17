// SPDX-License-Identifier: AGPL-3.0-or-later

import { beforeEach, describe, expect, it } from 'vitest';
import { applyTheme, savedTheme } from './theme';

beforeEach(() => {
	localStorage.clear();
	document.documentElement.removeAttribute('data-theme');
});

describe('the library theme', () => {
	// The bug this exists for: tokens.css carries four complete palettes keyed
	// on data-theme, and app.html set data-theme="light" unconditionally. That
	// made every other palette unreachable AND defeated the
	// prefers-color-scheme rule, which is written to apply only when the
	// attribute is absent.
	it('leaves the attribute off for "system", so the media query can win', () => {
		applyTheme('dark');
		expect(document.documentElement.getAttribute('data-theme')).toBe('dark');

		applyTheme('system');
		expect(document.documentElement.hasAttribute('data-theme')).toBe(false);
	});

	it('applies an explicit choice', () => {
		for (const theme of ['light', 'dark', 'black'] as const) {
			applyTheme(theme);
			expect(document.documentElement.getAttribute('data-theme')).toBe(theme);
		}
	});

	it('remembers the choice across a visit', () => {
		applyTheme('black');
		expect(savedTheme()).toBe('black');
	});

	it('follows the system when nothing has been chosen', () => {
		expect(savedTheme()).toBe('system');
	});

	it('follows the system rather than trusting a value it does not know', () => {
		localStorage.setItem('lexarii.theme', 'chartreuse');
		expect(savedTheme()).toBe('system');
	});
});
