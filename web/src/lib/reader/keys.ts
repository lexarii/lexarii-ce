// SPDX-License-Identifier: AGPL-3.0-or-later

/**
 * Whether a keystroke belongs to something the reader is typing into.
 *
 * The page-turn keys are unqualified and global, so without this they are
 * taken from every field on the page: a note cannot contain a space, because
 * space turns the page instead; the arrow keys cannot move a caret or work a
 * slider; and typing "t" into a note also flips the contents panel open. Every
 * one of those controls lives in the same component as the handler, so the
 * keyboard was unusable in exactly the place the reader offers one.
 *
 * Shared because both readers need it and the fix is the same in both. It was
 * pasted into each of them, which is how the next person to meet a keyboard
 * bug would have fixed one reader and not the other.
 */
export function isTyping(target: EventTarget | null): boolean {
	const element = target as HTMLElement | null;
	if (!element) return false;

	const tag = element.tagName;
	return (
		tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT' || element.isContentEditable === true
	);
}
