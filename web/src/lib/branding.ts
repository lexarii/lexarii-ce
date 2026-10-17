// SPDX-License-Identifier: AGPL-3.0-or-later

/**
 * An installation's own look, without a fork.
 *
 * When the server says `branding: true` (GET /api/v1/auth/providers), the root
 * layout loads one stylesheet from the installation's branding directory,
 * after the app's own, so it can override the design tokens and style the
 * hooks documented in docs/BRANDING.md. Everything else it needs - images,
 * fonts - it names itself, relative to its own URL.
 */

/** The one file the client loads by name. */
export const BRANDING_STYLESHEET = '/branding/branding.css';

/**
 * Returns a function that reports whether this installation has branding,
 * asking the server the first time and remembering the answer.
 *
 * Branding is fixed for the life of the server, so one question per load of
 * the app is enough; an invalidation must not re-ask and flicker the
 * stylesheet off and on. A failure is "no branding" - the app must never wait
 * on, or break over, how it looks - and is not remembered, so the next load
 * asks again.
 */
export function brandingProbe(ask: () => Promise<{ branding?: boolean }>): () => Promise<boolean> {
	let answer: Promise<boolean> | undefined;
	return () => {
		answer ??= ask().then(
			(options) => options.branding === true,
			() => {
				answer = undefined;
				return false;
			}
		);
		return answer;
	};
}

/**
 * The name a route goes by in `data-page` on `<body>`, for branding CSS to
 * select on.
 *
 * The route's id with its slashes turned to hyphens: parameters keep their
 * names without brackets, groups are dropped, and the root is "home". So
 * `/signin` is "signin", `/works/[id]` is "works-id", `/` is "home", and a
 * path that matched no route is "not-found".
 */
export function pageName(routeId: string | null): string {
	if (routeId === null) return 'not-found';
	const segments = routeId
		.split('/')
		.filter((segment) => segment !== '' && !/^\(.*\)$/.test(segment))
		.map((segment) => segment.replace(/^\[+(?:\.\.\.)?|\]+$/g, '').replace(/=.*$/, ''));
	return segments.length === 0 ? 'home' : segments.join('-');
}
