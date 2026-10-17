// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, expect, it } from 'vitest';
import { currentEntry, flattenToc, headingBefore, type TocNode } from './toc';

// The shape of Project Gutenberg's Pride and Prejudice: a title page and
// chapters I-X in spine item 1, chapters XI-XII in item 2. Each chapter starts
// some pages after the last.
const PRIDE: TocNode[] = [
	{ title: 'PRIDE. and PREJUDICE', spineIndex: 1, fragment: 'title' },
	{ title: 'Chapter I.', spineIndex: 1, fragment: 'c1' },
	{ title: 'CHAPTER II.', spineIndex: 1, fragment: 'c2' },
	{ title: 'CHAPTER X.', spineIndex: 1, fragment: 'c10' },
	{ title: 'CHAPTER XI.', spineIndex: 2, fragment: 'c11' },
	{ title: 'CHAPTER XII.', spineIndex: 2, fragment: 'c12' }
];

/** Where each fragment starts, in whichever chapter is on screen. */
const PAGES: Record<string, number> = { title: 0, c1: 2, c2: 9, c10: 60, c11: 0, c12: 14 };
const pageOf = (fragment: string) => PAGES[fragment] ?? null;

const title = (spine: number, page: number) =>
	currentEntry(PRIDE, spine, page, pageOf)?.title ?? null;

describe('currentEntry', () => {
	// The reported bug: every page of the first file said "CHAPTER X.",
	// because the last entry naming the file won wherever the reader was.
	it('names the chapter the reader is in, not the last one in the file', () => {
		expect(title(1, 0)).toBe('PRIDE. and PREJUDICE');
		expect(title(1, 2)).toBe('Chapter I.');
		expect(title(1, 5)).toBe('Chapter I.');
		expect(title(1, 9)).toBe('CHAPTER II.');
		expect(title(1, 59)).toBe('CHAPTER II.');
		expect(title(1, 60)).toBe('CHAPTER X.');
		expect(title(1, 80)).toBe('CHAPTER X.');
	});

	it('counts the next file from its own targets', () => {
		expect(title(2, 0)).toBe('CHAPTER XI.');
		expect(title(2, 13)).toBe('CHAPTER XI.');
		expect(title(2, 14)).toBe('CHAPTER XII.');
	});

	// Every entry in an earlier file has been passed, so the last of them is
	// where a file with no entries of its own sits.
	it('carries the last passed entry into a file the contents does not name', () => {
		expect(title(3, 0)).toBe('CHAPTER XII.');
	});

	it('is nothing before the first entry', () => {
		expect(title(0, 0)).toBeNull();
	});

	// A target the sanitiser dropped, or a book that names files only: the
	// entry still names this file, from its start.
	it('counts an entry whose target is not in the chapter from its first page', () => {
		const toc: TocNode[] = [
			{ title: 'One', spineIndex: 1 },
			{ title: 'Gone', spineIndex: 2, fragment: 'missing' }
		];
		expect(currentEntry(toc, 2, 0, () => null)?.title).toBe('Gone');
		expect(currentEntry(toc, 1, 7, () => null)?.title).toBe('One');
	});

	// A part and its first chapter on one page: the chapter says more.
	it('prefers the more specific of two entries at the same place', () => {
		const toc: TocNode[] = [
			{
				title: 'Part One',
				spineIndex: 1,
				fragment: 'p1',
				children: [{ title: 'Chapter 1', spineIndex: 1, fragment: 'c1' }]
			}
		];
		const flat = flattenToc(toc);
		expect(currentEntry(flat, 1, 0, () => 0)?.title).toBe('Chapter 1');
	});

	it('skips entries that point nowhere', () => {
		const toc: TocNode[] = [
			{ title: 'Real', spineIndex: 0 },
			{ title: 'Nowhere', spineIndex: -1 }
		];
		expect(currentEntry(toc, 0, 0, () => null)?.title).toBe('Real');
	});

	// A contents list is the book's, and need not be in reading order.
	it('goes by position rather than by order in the list', () => {
		const toc: TocNode[] = [
			{ title: 'Later', spineIndex: 1, fragment: 'b' },
			{ title: 'Earlier', spineIndex: 1, fragment: 'a' }
		];
		const at = (f: string) => (f === 'a' ? 1 : 5);
		expect(currentEntry(toc, 1, 6, at)?.title).toBe('Later');
		expect(currentEntry(toc, 1, 3, at)?.title).toBe('Earlier');
	});
});

describe('flattenToc', () => {
	it('lists every entry depth-first', () => {
		const toc: TocNode[] = [
			{
				title: 'A',
				spineIndex: 0,
				children: [
					{ title: 'A.1', spineIndex: 0, children: [{ title: 'A.1.a', spineIndex: 1 }] },
					{ title: 'A.2', spineIndex: 2 }
				]
			},
			{ title: 'B', spineIndex: 3 }
		];
		expect(flattenToc(toc).map((e) => e.title)).toEqual(['A', 'A.1', 'A.1.a', 'A.2', 'B']);
		expect(flattenToc(undefined)).toEqual([]);
	});
});

describe('headingBefore', () => {
	const headings = [
		{ title: 'CHAPTER I.', page: 0 },
		{ title: 'CHAPTER II.', page: 6 },
		{ title: 'CHAPTER III.', page: 11 }
	];

	it('names the last heading that has started', () => {
		expect(headingBefore(headings, 0)).toBe('CHAPTER I.');
		expect(headingBefore(headings, 10)).toBe('CHAPTER II.');
		expect(headingBefore(headings, 11)).toBe('CHAPTER III.');
	});

	it('is empty before the first heading', () => {
		expect(headingBefore([{ title: 'Late', page: 3 }], 1)).toBe('');
	});
});
