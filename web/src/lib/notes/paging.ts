// SPDX-License-Identifier: AGPL-3.0-or-later

/**
 * Adds the next page of a newest-first list to what is already shown.
 *
 * The server pages on a keyset, so a page never repeats a row; this still
 * drops an id already present, because a "load more" pressed twice before the
 * first reply lands would otherwise show every note on that page twice.
 */
export function appendPage<T>(shown: T[], next: T[], id: (item: T) => string): T[] {
	const seen = new Set(shown.map(id));
	const fresh = next.filter((item) => !seen.has(id(item)));
	return fresh.length === 0 ? shown : [...shown, ...fresh];
}
