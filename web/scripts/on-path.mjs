// SPDX-License-Identifier: AGPL-3.0-or-later

import fs from 'node:fs';
import path from 'node:path';

/**
 * The absolute path of the program `name` would run as, from PATH, or ''.
 *
 * For the build and the tests, which each start one program they do not ship:
 * git, to name the build after its commit (svelte.config.js), and a Chromium
 * to drive the Android reader page in (tests/android-reader). Both used to
 * hand a bare name to the shell or to `which`, and so ran whatever PATH
 * offered first - including from an empty or relative entry, which means the
 * directory the command happens to be run in. A `git` lying in a checked-out
 * tree would have run during the build.
 *
 * Only absolute directories are searched, and the caller runs the absolute
 * path this returns, so what runs is a file somebody installed on purpose.
 * Searched here rather than by running `which`, which is found through PATH
 * itself, and which Windows does not have.
 */
export function onPath(name, env = process.env) {
	const extensions =
		process.platform === 'win32' ? (env.PATHEXT ?? '.EXE;.CMD;.BAT').split(';') : [''];
	for (const dir of (env.PATH ?? '').split(path.delimiter)) {
		if (!path.isAbsolute(dir)) continue;
		for (const ext of extensions) {
			const candidate = path.join(dir, name + ext);
			try {
				fs.accessSync(candidate, fs.constants.X_OK);
				if (fs.statSync(candidate).isFile()) return candidate;
			} catch {
				/* not here, or not runnable: the next directory */
			}
		}
	}
	return '';
}
