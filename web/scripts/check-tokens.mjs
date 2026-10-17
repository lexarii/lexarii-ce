// SPDX-License-Identifier: AGPL-3.0-or-later

// Fails when a stylesheet reads a design token nobody defines.
//
// A `var(--name)` naming a token that does not exist fails silently: the
// declaration becomes invalid at computed-value time, so the property takes
// its inherited or initial value and the browser says nothing. A
// `padding-left: var(--space-5)` - a plausible name, in a scale that goes 4,
// 6, 8 - collapsed to zero and put the bullets of every answer outside their
// own list. It took a screenshot to notice, which is not a way to find bugs.
//
// Here rather than in vitest because vitest returns an empty string for every
// CSS import, which is exactly the file the definitions live in.
//
// Fallbacks are exempt: `var(--maybe, 1rem)` says what happens when the token
// is absent, which is the whole point of writing one.

import { readFileSync, readdirSync, realpathSync } from 'node:fs';
import { join, relative } from 'node:path';
import { fileURLToPath } from 'node:url';

const src = fileURLToPath(new URL('../src/', import.meta.url));

/** Every stylesheet and component under `dir`, at any depth. */
export function sources(dir, out = []) {
	for (const entry of readdirSync(dir, { withFileTypes: true })) {
		const path = join(dir, entry.name);
		if (entry.isDirectory()) sources(path, out);
		else if (/\.(css|svelte)$/.test(entry.name)) out.push(path);
	}
	return out;
}

/*
 * Every scan below is linear, and that is deliberate rather than tidy. The
 * obvious regexes - /\/\*[\s\S]*?\*\//g for a comment, /(--[a-z0-9-]+)\s*:/g
 * for a definition - are quadratic on the wrong input: each is retried at
 * every start position and runs to the end of the text or the run before it
 * fails. 50,000 unterminated comment openers took a second, 50,000 hyphens
 * three. Only our own stylesheets come through here, but a check that can
 * hang on a half-typed file is a check people learn to skip.
 */

/**
 * Strips comments, so a token NAMED in prose is not read as a token used.
 *
 * A comment explaining `var(--total-scale-factor)` is not a use of it, and
 * reporting one wastes exactly the attention this check exists to save. The
 * `//` rule leaves `https://` alone; nothing else in these files looks like a
 * comment and is not one.
 */
export function withoutComments(text) {
	return withoutLineComments(withoutBlockComments(text));
}

/** Drops each `/* ... *\/`. An unclosed one is left as it is. */
function withoutBlockComments(text) {
	let out = '';
	let from = 0;
	for (let open = text.indexOf('/*'); open >= 0; open = text.indexOf('/*', from)) {
		const close = text.indexOf('*/', open + 2);
		// No close after this opener means none after any later one either.
		if (close < 0) break;
		out += text.slice(from, open);
		from = close + 2;
	}
	return out + text.slice(from);
}

/** Drops each `//` to the end of its line, unless a colon comes right before it. */
function withoutLineComments(text) {
	const lineEnd = /[\n\r\u2028\u2029]|$/g;
	let out = '';
	let from = 0;
	let at = text.indexOf('//');
	while (at >= 0) {
		// The next search starts past this `//`, or past the end of the line
		// just dropped, so the scan never goes back over text.
		let next = at + 1;
		if (text[at - 1] !== ':') {
			lineEnd.lastIndex = at;
			const end = lineEnd.exec(text).index;
			out += text.slice(from, at);
			from = end;
			next = end + 1;
		}
		at = text.indexOf('//', next);
	}
	return out + text.slice(from);
}

/** The custom properties `text` declares: `--name` followed by a colon. */
export function* declarations(text) {
	const colon = /\s*:/y;
	for (const match of text.matchAll(/--[a-z0-9-]+/g)) {
		colon.lastIndex = match.index + match[0].length;
		if (colon.test(text)) yield match[0];
	}
}

/**
 * The tokens the stylesheets under `root` read without a fallback, each with
 * the file (relative to `root`) that reads it first, and those of them that
 * nothing defines.
 */
export function check(root) {
	const defined = new Set();
	const used = new Map();
	for (const path of sources(root)) {
		const text = withoutComments(readFileSync(path, 'utf8'));
		for (const name of declarations(text)) defined.add(name);
		// A token can also be defined at runtime, which is how a value that only
		// JavaScript knows - a render scale, a measured height - reaches the
		// stylesheet.
		for (const [, name] of text.matchAll(/setProperty\(\s*['"`](--[a-z0-9-]+)/g)) defined.add(name);
		for (const [, name] of text.matchAll(/var\(\s*(--[a-z0-9-]+)\s*\)/g)) {
			if (!used.has(name)) used.set(name, relative(root, path));
		}
	}
	const missing = [...used].filter(([name]) => !defined.has(name));
	return { used, missing };
}

/** Checks `root`, which is web/src unless a test says otherwise; the exit status. */
export function main(root = src, { log = console.log, error = console.error } = {}) {
	const { used, missing } = check(root);
	for (const [name, path] of missing) {
		error(`undefined design token ${name}, first used in src/${path}`);
	}
	if (missing.length > 0) return 1;
	log(`design tokens: ${used.size} used, all defined`);
	return 0;
}

// Run as a program (`npm run check:tokens`), not when a test imports it.
if (process.argv[1] && realpathSync(process.argv[1]) === fileURLToPath(import.meta.url)) {
	process.exitCode = main();
}
