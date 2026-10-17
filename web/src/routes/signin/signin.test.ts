// SPDX-License-Identifier: AGPL-3.0-or-later
//
// The sign-in page's branding hooks are a supported contract
// (docs/BRANDING.md): an installation's stylesheet selects on these names, so
// renaming or dropping one breaks somebody's sign-in page without a test
// saying so - unless this one does.
import { flushSync, mount, tick, unmount } from 'svelte';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';

vi.mock('$app/state', () => ({ page: { url: new URL('https://lexarii.test/signin') } }));
vi.mock('$app/navigation', () => ({ goto: vi.fn() }));
vi.mock('$api/client', () => ({
	authOptions: vi.fn(async () => ({
		providers: [{ name: 'local', displayName: 'Password', kind: 'password' }],
		setupRequired: false,
		links: [{ label: 'Request access', href: '/help/access' }],
		branding: true
	}))
}));

import SignIn from './+page.svelte';

let mounted: ReturnType<typeof mount> | undefined;
let target: HTMLElement;

beforeEach(async () => {
	target = document.createElement('div');
	document.body.append(target);
	mounted = mount(SignIn, { target });
	flushSync();
	await new Promise((resolve) => setTimeout(resolve, 0));
	await tick();
	flushSync();
});

afterEach(() => {
	if (mounted) unmount(mounted);
	mounted = undefined;
	target.remove();
});

it('names the page on its root', () => {
	const root = target.querySelector('main');
	expect(root?.dataset.page).toBe('signin');
});

it('puts the brand, the form and the links in the card', () => {
	const card = target.querySelector('main > .auth-card');
	expect(card).not.toBeNull();
	expect(card?.querySelector('h1.auth-brand')?.textContent).toBe('Lexarii');
	expect(card?.querySelector('form')).not.toBeNull();
	const links = card?.querySelector('nav.auth-links');
	expect(links?.querySelector('a')?.getAttribute('href')).toBe('/help/access');
});

it('has an empty decoration behind the card, hidden from assistive technology', () => {
	const decoration = target.querySelector('main > .auth-decoration');
	expect(decoration).not.toBeNull();
	expect(decoration?.tagName).toBe('DIV');
	expect(decoration?.getAttribute('aria-hidden')).toBe('true');
	expect(decoration?.childNodes).toHaveLength(0);
	// Before the card, so it is behind it in the document as well as on screen.
	expect(decoration?.nextElementSibling?.classList.contains('auth-card')).toBe(true);
});
