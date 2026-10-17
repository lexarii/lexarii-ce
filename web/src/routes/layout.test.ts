// SPDX-License-Identifier: AGPL-3.0-or-later
//
// The root layout's part in branding: the installation's stylesheet is linked
// when, and only when, the server says there is one, and every page is named
// on <body> for that stylesheet to select on.
import { createRawSnippet, flushSync, mount, tick, unmount } from 'svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';

const state = vi.hoisted(() => ({
	page: { data: {} as Record<string, unknown>, route: { id: '/signin' as string | null } }
}));

vi.mock('$app/state', () => ({ page: state.page }));
vi.mock('$app/navigation', () => ({ goto: vi.fn() }));
vi.mock('$api/client', () => ({
	me: vi.fn(() => new Promise(() => {})),
	ApiError: class extends Error {}
}));

import { PUBLIC_ROUTE } from '$lib/auth/public';
import Layout from './+layout.svelte';

const STYLESHEET = 'link[rel="stylesheet"][href="/branding/branding.css"]';

let mounted: ReturnType<typeof mount> | undefined;

async function render(branding: Promise<boolean>, routeId: string | null = '/signin') {
	state.page.route.id = routeId;
	state.page.data = { ...PUBLIC_ROUTE, branding };
	const target = document.createElement('div');
	document.body.append(target);
	mounted = mount(Layout, {
		target,
		props: {
			data: { branding },
			params: {},
			children: createRawSnippet(() => ({ render: () => '<p>page</p>' }))
		}
	});
	flushSync();
	// Long enough for a settled promise to reach the {#await}, and no longer.
	await new Promise((resolve) => setTimeout(resolve, 0));
	await tick();
	flushSync();
	return target;
}

afterEach(() => {
	if (mounted) unmount(mounted);
	mounted = undefined;
	document.body.innerHTML = '';
});

describe('the branding stylesheet', () => {
	it('is linked when the installation has branding', async () => {
		await render(Promise.resolve(true));
		const links = document.head.querySelectorAll(STYLESHEET);
		expect(links).toHaveLength(1);
	});

	it('is not linked when it has none', async () => {
		await render(Promise.resolve(false));
		expect(document.head.querySelector(STYLESHEET)).toBeNull();
	});

	it('does not hold up the page while the server is asked', async () => {
		const target = await render(new Promise<boolean>(() => {}));
		expect(target.textContent).toContain('page');
		expect(document.head.querySelector(STYLESHEET)).toBeNull();
	});

	it('goes when the layout does', async () => {
		await render(Promise.resolve(true));
		if (mounted) unmount(mounted);
		mounted = undefined;
		expect(document.head.querySelector(STYLESHEET)).toBeNull();
	});
});

describe('data-page on <body>', () => {
	it('names the route', async () => {
		await render(Promise.resolve(false), '/signin');
		expect(document.body.dataset.page).toBe('signin');
	});

	it('names a route with a parameter by its shape', async () => {
		await render(Promise.resolve(false), '/works/[id]');
		expect(document.body.dataset.page).toBe('works-id');
	});
});
