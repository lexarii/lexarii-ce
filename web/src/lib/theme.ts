// SPDX-License-Identifier: AGPL-3.0-or-later

/**
 * The library's theme: what the shelf, the chat and the settings look like.
 *
 * Not the reader's. The reader owns its own palette deliberately - a reading
 * surface is a different object from an application chrome, and somebody
 * reading in sepia does not necessarily want a sepia library behind it.
 *
 * The four palettes were written in tokens.css from the start and none of them
 * could ever be selected: nothing set the attribute they key on, and app.html
 * carried data-theme="light" unconditionally, which also defeated the
 * prefers-color-scheme rule written to catch that case. This is the part that
 * was missing.
 */

export const themes = ['system', 'light', 'dark', 'black'] as const;

export type Theme = (typeof themes)[number];

/** How each is described to a person, rather than by its token name. */
export const themeLabels: Record<Theme, string> = {
	system: 'Match my system',
	light: 'Light',
	dark: 'Dark',
	black: 'Black'
};

const key = 'lexarii.theme';

/** The saved choice, or "system" when there is none or it cannot be read. */
export function savedTheme(): Theme {
	try {
		const value = localStorage.getItem(key);
		return themes.includes(value as Theme) ? (value as Theme) : 'system';
	} catch {
		// Private browsing, or storage refused. Following the system is the
		// right answer when we cannot remember what was asked for.
		return 'system';
	}
}

/**
 * Applies a theme and remembers it.
 *
 * "system" REMOVES the attribute rather than setting it to anything, because
 * the media query in tokens.css is written to apply only in its absence. An
 * attribute saying "system" would be just as unreachable as the light one was.
 */
export function applyTheme(theme: Theme) {
	const root = document.documentElement;
	if (theme === 'system') {
		root.removeAttribute('data-theme');
	} else {
		root.setAttribute('data-theme', theme);
	}

	try {
		localStorage.setItem(key, theme);
	} catch {
		/* it applies for this visit and is forgotten, which is the lesser loss */
	}
}
