// SPDX-License-Identifier: AGPL-3.0-or-later

/** Somebody the share panel can show a row for. */
export interface Candidate {
	id: string;
	username: string;
}

/**
 * Who the share panel lists.
 *
 * With the installation's people listed, everybody from /people. Without -
 * LEXARII_PEOPLE_DIRECTORY=none, for anybody but an administrator - only the
 * people this person already deals with on this file: whom they lend it to,
 * whom they share their notes with, and whom they have named in the panel
 * since opening it, so unticking somebody does not make them vanish from
 * under the pointer. Each appears once, in name order.
 */
export function candidates(
	listed: boolean,
	everybody: Candidate[],
	...known: Candidate[][]
): Candidate[] {
	if (listed) return everybody;
	const byId = new Map<string, Candidate>();
	for (const group of known) {
		for (const person of group) {
			if (!byId.has(person.id)) byId.set(person.id, { id: person.id, username: person.username });
		}
	}
	return [...byId.values()].sort((a, b) =>
		a.username.localeCompare(b.username, undefined, { sensitivity: 'base' })
	);
}
