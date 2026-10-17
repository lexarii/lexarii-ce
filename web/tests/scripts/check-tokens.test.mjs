// SPDX-License-Identifier: AGPL-3.0-or-later
// @vitest-environment node

import { execFileSync } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { afterAll, beforeAll, describe, expect, it } from 'vitest';
import {
	check,
	declarations,
	main,
	sources,
	withoutComments
} from '../../scripts/check-tokens.mjs';

describe('withoutComments', () => {
	it('drops block comments, including ones spanning lines', () => {
		expect(withoutComments('a /* var(--x) */ b')).toBe('a  b');
		expect(withoutComments('a /* one\n two */b/**/c')).toBe('a bc');
	});

	it('leaves an unclosed block comment, and everything after it, as it is', () => {
		expect(withoutComments('a /* b */ c /* d var(--x)')).toBe('a  c /* d var(--x)');
	});

	it('drops a line comment up to, not including, the end of its line', () => {
		expect(withoutComments('a // var(--x)\nb')).toBe('a \nb');
		expect(withoutComments('a // x\r\nb // y\u2028c // z\u2029d')).toBe('a \r\nb \u2028c \u2029d');
		expect(withoutComments('a // to the end')).toBe('a ');
		expect(withoutComments('//')).toBe('');
	});

	it('keeps the `//` of a URL, and finds a comment after one', () => {
		expect(withoutComments("url('https://example.org/a.png')")).toBe(
			"url('https://example.org/a.png')"
		);
		expect(withoutComments('a: https://x // y\nb')).toBe('a: https://x \nb');
		expect(withoutComments('://a://b//c')).toBe('://a://b');
	});

	it('reads a line comment opener inside a block comment as part of the block', () => {
		expect(withoutComments('a /* // */ b // c')).toBe('a  b ');
	});

	it('does not drop text that only looks like the start of a comment', () => {
		expect(withoutComments('a / b * c')).toBe('a / b * c');
		expect(withoutComments('')).toBe('');
	});

	// The scans are written to be linear; the obvious regexes were quadratic
	// and took seconds on a few tens of thousands of these.
	it('stays linear on input made of nothing but openers', () => {
		const started = performance.now();
		for (const text of [
			'//'.repeat(200_000),
			'/*'.repeat(200_000),
			':' + '//x\n'.repeat(200_000)
		]) {
			withoutComments(text);
		}
		[...declarations('-'.repeat(400_000))];
		expect(performance.now() - started).toBeLessThan(2000);
	});
});

describe('declarations', () => {
	it('yields each custom property followed by a colon', () => {
		const text = ':root { --a: 1; --b-2 : 2; --c\n:3 } x { color: var(--a); --d }';
		expect([...declarations(text)]).toEqual(['--a', '--b-2', '--c']);
	});

	it('does not count a use as a declaration', () => {
		expect([...declarations('p { margin: var(--space-4); }')]).toEqual([]);
	});
});

describe('check', () => {
	let root = '';
	const write = (file, text) => {
		const at = path.join(root, file);
		fs.mkdirSync(path.dirname(at), { recursive: true });
		fs.writeFileSync(at, text);
	};

	beforeAll(() => {
		root = path.join(fs.mkdtempSync(path.join(os.tmpdir(), 'check-tokens-')), 'src');
		write('app.css', ':root { --ink: #000; --space-4: 1rem; }\n/* var(--in-a-comment) */');
		write('lib/a/Page.svelte', '<style>p { color: var(--ink); margin: var( --space-4 ); }</style>');
		write('lib/b/Fallback.svelte', '<style>p { padding: var(--maybe, 1rem); }</style>');
		write(
			'lib/b/Scale.svelte',
			"<script>el.style.setProperty('--scale', '2');</script><style>p { zoom: var(--scale) }</style>"
		);
		write('lib/notes.md', 'var(--not-a-stylesheet)');
	});

	afterAll(() => fs.rmSync(path.dirname(root), { recursive: true, force: true }));

	it('finds the stylesheets and components at any depth, and nothing else', () => {
		const found = sources(root)
			.map((file) => path.relative(root, file))
			.sort();
		expect(found).toEqual([
			'app.css',
			path.join('lib', 'a', 'Page.svelte'),
			path.join('lib', 'b', 'Fallback.svelte'),
			path.join('lib', 'b', 'Scale.svelte')
		]);
	});

	it('passes when every token read is defined, in CSS or by setProperty', () => {
		const { used, missing } = check(root);
		expect([...used.keys()].sort()).toEqual(['--ink', '--scale', '--space-4']);
		expect(missing).toEqual([]);
	});

	it('ignores a token with a fallback, and one named in a comment', () => {
		const { used } = check(root);
		expect(used.has('--maybe')).toBe(false);
		expect(used.has('--in-a-comment')).toBe(false);
		expect(used.has('--not-a-stylesheet')).toBe(false);
	});

	it('reports an undefined token with the file that reads it', () => {
		write('lib/c/Typo.svelte', '<style>ul { padding-left: var(--space-5); }</style>');
		try {
			expect(check(root).missing).toEqual([['--space-5', path.join('lib', 'c', 'Typo.svelte')]]);
		} finally {
			fs.rmSync(path.join(root, 'lib', 'c'), { recursive: true });
		}
	});

	it('reports each undefined token once, at its first use', () => {
		write('lib/d/One.svelte', '<style>p { color: var(--gone); }</style>');
		write('lib/d/Two.svelte', '<style>p { color: var(--gone); }</style>');
		try {
			const { missing } = check(root);
			expect(missing).toHaveLength(1);
			expect(missing[0]?.[0]).toBe('--gone');
		} finally {
			fs.rmSync(path.join(root, 'lib', 'd'), { recursive: true });
		}
	});

	it('main says so and returns 0 when all is well, 1 and each name when not', () => {
		const log = [];
		const error = [];
		const out = {
			log: (line) => log.push(line),
			error: (line) => error.push(line)
		};

		expect(main(root, out)).toBe(0);
		expect(log).toEqual(['design tokens: 3 used, all defined']);
		expect(error).toEqual([]);

		write('broken.css', 'a { color: var(--nope); }');
		try {
			log.length = 0;
			expect(main(root, out)).toBe(1);
			expect(error).toEqual(['undefined design token --nope, first used in src/broken.css']);
			expect(log).toEqual([]);
		} finally {
			fs.rmSync(path.join(root, 'broken.css'));
		}
	});
});

describe('the check:tokens command', () => {
	// Imported, the module only defines things; run, it checks web/src. A guard
	// that failed to recognise being run would pass every time, silently.
	it('checks the real stylesheets when run, and they pass', () => {
		const script = fileURLToPath(new URL('../../scripts/check-tokens.mjs', import.meta.url));
		const out = execFileSync(process.execPath, [script], { encoding: 'utf8' });
		expect(out).toMatch(/^design tokens: \d+ used, all defined\n$/);
	});
});
