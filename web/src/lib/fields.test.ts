// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, expect, it } from 'vitest';
import { fieldLabel } from './fields';

describe('field labels', () => {
	it('names the fields provenance and suggestions use', () => {
		expect(fieldLabel('publishedOn')).toBe('Published');
		expect(fieldLabel('isbn13')).toBe('ISBN-13');
		expect(fieldLabel('seriesIndex')).toBe('Number in series');
		expect(fieldLabel('title')).toBe('Title');
	});

	it('spells out a field it does not know rather than showing the key', () => {
		expect(fieldLabel('pageCount')).toBe('Page count');
		expect(fieldLabel('original_language')).toBe('Original language');
		expect(fieldLabel('')).toBe('');
	});
});
