// SPDX-License-Identifier: AGPL-3.0-or-later

/**
 * Where the reader is, in the book's own words.
 *
 * A contents entry names a spine item AND a place inside it. Many books put
 * several chapters in one file - every Project Gutenberg novel does, and
 * Pride and Prejudice puts eleven in the first - so "the last entry at or
 * before this spine item" named the LAST chapter in the file on every page of
 * it: the reader on chapter one was told they were in chapter ten.
 *
 * So an entry in the chapter on screen counts only once its target has been
 * reached: its fragment starts on this page or an earlier one. An entry in an
 * earlier spine item always has been.
 */

/** The shape of a contents entry: what the manifest sends. */
export interface TocNode {
	title: string;
	spineIndex: number;
	fragment?: string;
	children?: TocNode[];
}

/** Every entry in reading order, depth-first: a part before its chapters. */
export function flattenToc<T extends TocNode>(entries: readonly T[] | undefined): T[] {
	const out: T[] = [];
	const walk = (list: readonly TocNode[] | undefined) => {
		for (const entry of list ?? []) {
			out.push(entry as T);
			walk(entry.children);
		}
	};
	walk(entries);
	return out;
}

/**
 * The entry the reader is inside, or null when none has started yet.
 *
 * `pageOf(fragment)` says which page of the chapter on screen a fragment's
 * target starts on, or null when the chapter has no such element - an entry
 * whose target the sanitiser dropped still names this chapter, and counts
 * from its first page.
 *
 * Of the entries reached, the one furthest into the book wins. Between two
 * at the same place - a part and its first chapter on one page, or entries in
 * an earlier file, all of which have been passed - the later in the contents
 * wins: it is the more specific, and the one the reader passed last.
 */
export function currentEntry<T extends TocNode>(
	entries: readonly T[],
	spineIndex: number,
	page: number,
	pageOf: (fragment: string) => number | null
): T | null {
	let best: T | null = null;
	let bestSpine = -1;
	let bestPage = -1;
	for (const entry of entries) {
		if (entry.spineIndex < 0 || entry.spineIndex > spineIndex) continue;
		let at = 0;
		if (entry.spineIndex === spineIndex && entry.fragment) {
			at = pageOf(entry.fragment) ?? 0;
			if (at > page) continue;
		}
		if (entry.spineIndex > bestSpine || (entry.spineIndex === bestSpine && at >= bestPage)) {
			best = entry;
			bestSpine = entry.spineIndex;
			bestPage = at;
		}
	}
	return best;
}

/**
 * The last heading that starts on or before `page`, for a book whose contents
 * say nothing about where the reader is.
 */
export function headingBefore(
	headings: readonly { title: string; page: number }[],
	page: number
): string {
	let best = '';
	for (const heading of headings) {
		if (heading.page <= page && heading.title) best = heading.title;
	}
	return best;
}
