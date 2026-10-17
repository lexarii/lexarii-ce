// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, expect, it } from 'vitest';
import { candidates } from './people';

const ada = { id: '1', username: 'ada' };
const bo = { id: '2', username: 'Bo' };
const cy = { id: '3', username: 'cy' };

describe('who the share panel lists', () => {
	it('is everybody when the installation lists its people', () => {
		expect(candidates(true, [ada, bo, cy], [ada])).toEqual([ada, bo, cy]);
	});

	it('is only the people already dealt with when it does not', () => {
		const out = candidates(false, [ada, bo, cy], [cy], [ada]);
		expect(out.map((p) => p.username)).toEqual(['ada', 'cy']);
	});

	it('names each person once, in name order whatever the case', () => {
		const out = candidates(false, [], [cy, bo], [bo, ada], [{ id: '1', username: 'ada' }]);
		expect(out.map((p) => p.id)).toEqual(['1', '2', '3']);
	});

	it('is nobody when nothing is known yet', () => {
		expect(candidates(false, [ada])).toEqual([]);
	});
});
