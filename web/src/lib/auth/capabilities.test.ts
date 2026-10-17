// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, expect, it } from 'vitest';
import { offersAsk } from './capabilities';

describe('offering Ask', () => {
	it('is offered only when the server says a model is configured', () => {
		expect(offersAsk({ canAsk: true })).toBe(true);
		expect(offersAsk({ canAsk: false })).toBe(false);
		expect(offersAsk({})).toBe(false);
		expect(offersAsk(null)).toBe(false);
		expect(offersAsk(undefined)).toBe(false);
	});
});
