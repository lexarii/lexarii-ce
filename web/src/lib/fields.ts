// SPDX-License-Identifier: AGPL-3.0-or-later

/**
 * What a book's metadata fields are called on screen.
 *
 * The server names them as the wire does - `publishedOn`, `isbn13` - and a
 * page that prints those raw shows a person the API instead of their book.
 */
const labels: Record<string, string> = {
	title: 'Title',
	subtitle: 'Subtitle',
	authors: 'Authors',
	language: 'Language',
	publisher: 'Publisher',
	publishedOn: 'Published',
	isbn13: 'ISBN-13',
	isbn10: 'ISBN-10',
	description: 'Description',
	series: 'Series',
	seriesIndex: 'Number in series',
	subjects: 'Subjects',
	summary: 'Summary',
	audience: 'Audience',
	tags: 'Tags'
};

/**
 * The label for a field. One this list does not know yet is spelled out from
 * its camelCase name ("pageCount" → "Page count") rather than shown raw.
 */
export function fieldLabel(field: string): string {
	const known = labels[field];
	if (known) return known;
	const words = field
		.replace(/([a-z0-9])([A-Z])/g, '$1 $2')
		.replace(/[_-]+/g, ' ')
		.trim()
		.toLowerCase();
	return words ? words.charAt(0).toUpperCase() + words.slice(1) : field;
}
