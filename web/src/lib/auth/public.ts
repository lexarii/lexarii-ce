// SPDX-License-Identifier: AGPL-3.0-or-later

/**
 * Routes that can be shown to somebody who is not signed in.
 *
 * The root layout checks the session on every page and sends a signed-out
 * visitor to /signin. A route opts out by returning this from its load
 * function - `+page.ts` for one page, or `+layout.ts` for every page under it:
 *
 *     import { PUBLIC_ROUTE } from '$lib/auth/public';
 *     export const load = () => PUBLIC_ROUTE;
 *
 * This is about what the CLIENT shows, and nothing else. The server decides
 * what an anonymous request may read, so a public page can only display data
 * from endpoints that are open on the server too.
 */
const PUBLIC_ROUTE_KEY = '__lexariiPublicRoute';

export const PUBLIC_ROUTE = Object.freeze({ [PUBLIC_ROUTE_KEY]: true as const });

/**
 * Reports whether a route's load data (`page.data`) declares it public.
 *
 * `page.data` is every load on the route merged into one object, so the key
 * is one nothing else would return by accident: a plain `public: true` in
 * some future response would otherwise make a page public without anybody
 * deciding it should be.
 */
export function isPublicRoute(data: unknown): boolean {
	return (
		typeof data === 'object' &&
		data !== null &&
		(data as Record<string, unknown>)[PUBLIC_ROUTE_KEY] === true
	);
}

/**
 * Returns the href when it is one the sign-in page may link to, or null.
 *
 * The server refuses anything else at startup, so this is the second check of
 * two rather than the only one: a path on this server beginning with exactly
 * one "/", or an https URL. A page shown to strangers is the last place for a
 * `javascript:` link to get through because a check lived in one place only.
 */
export function safeSignInHref(href: string): string | null {
	if (/[\s\\\u0000-\u001f\u007f]/.test(href)) return null;
	if (href.startsWith('/')) return href.startsWith('//') ? null : href;
	// Spelled out, not left to the URL parser: WHATWG reads "https:evil.example"
	// as https://evil.example/, which the server refuses.
	if (!href.toLowerCase().startsWith('https://')) return null;
	try {
		const url = new URL(href);
		return url.protocol === 'https:' &&
			url.host !== '' &&
			url.username === '' &&
			url.password === ''
			? href
			: null;
	} catch {
		return null;
	}
}

/**
 * Reports whether an href leaves this server, so the link can say so
 * (rel="external noopener noreferrer"). It still opens in the same tab: a
 * sign-in page that spawns tabs is a surprise, and the back button returns.
 */
export function isExternalHref(href: string): boolean {
	return !href.startsWith('/');
}
