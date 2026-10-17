// SPDX-License-Identifier: AGPL-3.0-or-later
// @vitest-environment node

import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { afterAll, beforeAll, describe, expect, it } from 'vitest';
import { onPath } from '../../scripts/on-path.mjs';

// A PATH made of real directories, each holding what one case needs: a
// runnable `tool`, a `tool` that is not runnable, a directory called `tool`.
let root = '';
let dir;

function program(at, mode = 0o755) {
	fs.mkdirSync(path.dirname(at), { recursive: true });
	fs.writeFileSync(at, '#!/bin/sh\n');
	fs.chmodSync(at, mode);
	return at;
}

beforeAll(() => {
	root = fs.mkdtempSync(path.join(os.tmpdir(), 'on-path-'));
	dir = (name) => path.join(root, name);
	program(path.join(dir('first'), 'tool'));
	program(path.join(dir('second'), 'tool'));
	program(path.join(dir('plain'), 'tool'), 0o644);
	fs.mkdirSync(path.join(dir('folder'), 'tool'), { recursive: true });
	program(path.join(dir('win'), 'tool.CMD'));
	program(path.join(dir('win'), 'tool.EXE'), 0o644);
	program(path.join(dir('cwd'), 'tool'));
});

afterAll(() => fs.rmSync(root, { recursive: true, force: true }));

const PATH = (...dirs) => ({ PATH: dirs.join(path.delimiter) });

describe('onPath', () => {
	it('returns the absolute path of the first runnable match, in PATH order', () => {
		expect(onPath('tool', PATH(dir('empty'), dir('first'), dir('second')))).toBe(
			path.join(dir('first'), 'tool')
		);
		expect(onPath('tool', PATH(dir('second'), dir('first')))).toBe(
			path.join(dir('second'), 'tool')
		);
	});

	it('returns an empty string when nothing on PATH matches', () => {
		expect(onPath('missing', PATH(dir('first'), dir('second')))).toBe('');
		expect(onPath('tool', {})).toBe('');
		expect(onPath('tool', { PATH: '' })).toBe('');
	});

	it('passes over a file that is not runnable, and a directory with the name', () => {
		expect(onPath('tool', PATH(dir('plain'), dir('folder'), dir('second')))).toBe(
			path.join(dir('second'), 'tool')
		);
		expect(onPath('tool', PATH(dir('plain'), dir('folder')))).toBe('');
	});

	// The reason it exists: an empty or relative PATH entry means the directory
	// the command is run in, so a `git` checked into a tree would have run
	// during the build.
	it('never searches an empty or relative entry, even where the program is', () => {
		const cwd = process.cwd();
		process.chdir(dir('cwd'));
		try {
			for (const entry of ['', '.', './', 'cwd', '../cwd']) {
				expect(onPath('tool', { PATH: entry })).toBe('');
			}
			expect(onPath('tool', PATH('', '.', dir('second')))).toBe(path.join(dir('second'), 'tool'));
		} finally {
			process.chdir(cwd);
		}
	});

	it('uses the default environment when none is given', () => {
		// node is what is running this, so it is on the PATH of any machine
		// that can run the tests - unless the PATH holds only relative entries.
		const found = onPath('node');
		if (found) expect(path.isAbsolute(found)).toBe(true);
		expect(onPath('lexarii-no-such-program-anywhere')).toBe('');
	});

	describe('on Windows', () => {
		const platform = Object.getOwnPropertyDescriptor(process, 'platform');
		beforeAll(() => Object.defineProperty(process, 'platform', { value: 'win32' }));
		afterAll(() => {
			if (platform) Object.defineProperty(process, 'platform', platform);
		});

		it('tries each PATHEXT extension in order, skipping one that is not runnable', () => {
			expect(onPath('tool', { ...PATH(dir('win')), PATHEXT: '.EXE;.CMD' })).toBe(
				path.join(dir('win'), 'tool.CMD')
			);
			expect(onPath('tool', { ...PATH(dir('win')), PATHEXT: '.BAT' })).toBe('');
		});

		it('falls back to the usual extensions without PATHEXT', () => {
			expect(onPath('tool', PATH(dir('win')))).toBe(path.join(dir('win'), 'tool.CMD'));
		});

		it('does not find a file by its bare name', () => {
			expect(onPath('tool', PATH(dir('first')))).toBe('');
		});
	});
});
