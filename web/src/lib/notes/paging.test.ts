// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, expect, it } from 'vitest';
import { appendPage } from './paging';

const byId = (n: { id: string }) => n.id;

describe('appending a page of notes', () => {
	it('keeps the order: what was shown, then the next page', () => {
		const out = appendPage([{ id: 'a' }, { id: 'b' }], [{ id: 'c' }, { id: 'd' }], byId);
		expect(out.map(byId)).toEqual(['a', 'b', 'c', 'd']);
	});

	it('never shows a note twice', () => {
		const out = appendPage([{ id: 'a' }, { id: 'b' }], [{ id: 'b' }, { id: 'c' }], byId);
		expect(out.map(byId)).toEqual(['a', 'b', 'c']);
	});

	it('returns what was shown when the page adds nothing', () => {
		const shown = [{ id: 'a' }];
		expect(appendPage(shown, [{ id: 'a' }], byId)).toBe(shown);
		expect(appendPage(shown, [], byId)).toBe(shown);
	});
});
