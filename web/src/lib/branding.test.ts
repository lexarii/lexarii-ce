// SPDX-License-Identifier: AGPL-3.0-or-later
import { describe, expect, it, vi } from 'vitest';
import { brandingProbe, pageName } from './branding';

describe('pageName', () => {
	it.each([
		['/', 'home'],
		['/signin', 'signin'],
		['/account', 'account'],
		['/works/[id]', 'works-id'],
		['/read/[id]', 'read-id'],
		['/collections/[id]', 'collections-id'],
		['/(group)/settings', 'settings'],
		['/files/[...rest]', 'files-rest'],
		['/[[lang]]/about', 'lang-about'],
		['/works/[id=uuid]', 'works-id']
	])('%s is %s', (routeId, name) => {
		expect(pageName(routeId)).toBe(name);
	});

	it('names a path that matched no route', () => {
		expect(pageName(null)).toBe('not-found');
	});
});

describe('brandingProbe', () => {
	it('reads the flag from the server', async () => {
		expect(await brandingProbe(async () => ({ branding: true }))()).toBe(true);
		expect(await brandingProbe(async () => ({ branding: false }))()).toBe(false);
		// An older server that does not send the field has no branding.
		expect(await brandingProbe(async () => ({}))()).toBe(false);
	});

	it('asks once, however often it is called', async () => {
		const ask = vi.fn(async () => ({ branding: true }));
		const probe = brandingProbe(ask);
		expect(await probe()).toBe(true);
		expect(await probe()).toBe(true);
		expect(ask).toHaveBeenCalledTimes(1);
	});

	it('treats a failure as no branding, and asks again next time', async () => {
		const ask = vi
			.fn<() => Promise<{ branding?: boolean }>>()
			.mockRejectedValueOnce(new Error('offline'))
			.mockResolvedValueOnce({ branding: true });
		const probe = brandingProbe(ask);
		expect(await probe()).toBe(false);
		expect(await probe()).toBe(true);
		expect(ask).toHaveBeenCalledTimes(2);
	});
});
