// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, expect, it } from 'vitest';
import { isPublicRoute, PUBLIC_ROUTE, safeSignInHref } from './public';

describe('public routes', () => {
	it('is declared by load data, and only by exactly true', () => {
		expect(isPublicRoute(PUBLIC_ROUTE)).toBe(true);
		expect(isPublicRoute({ ...PUBLIC_ROUTE, other: 1 })).toBe(true);

		for (const data of [
			{ public: true },
			{ __lexariiPublicRoute: 'true' },
			undefined,
			null,
			{},
			{ public: 'true' },
			{ public: 1 },
			{ public: false },
			'public'
		]) {
			expect(isPublicRoute(data)).toBe(false);
		}
	});
});

describe('sign-in page links', () => {
	it('keeps a path on this server and an https link', () => {
		for (const href of ['/terms', '/help/password?x=1#a', 'https://example.org/access']) {
			expect(safeSignInHref(href)).toBe(href);
		}
	});

	it('drops everything else', () => {
		for (const href of [
			'javascript:alert(1)',
			'JAVASCRIPT:alert(1)',
			'data:text/html,hi',
			'http://example.org/',
			'mailto:someone@example.org',
			'//evil.example/',
			'/\\evil.example',
			'/\t/evil.example',
			'https://user:pass@example.org/',
			'https:evil.example',
			'https:/evil.example',
			'HTTPS:evil.example',
			'terms',
			''
		]) {
			expect(safeSignInHref(href)).toBeNull();
		}
	});
});
