import { execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import adapter from '@sveltejs/adapter-static';
import { onPath } from './scripts/on-path.mjs';

/**
 * What this build calls itself.
 *
 * SvelteKit defaults this to `Date.now()`, and the value is baked into the
 * generated modules - so two builds of identical source produced entirely
 * different chunk hashes, every file in `_app/immutable` renamed, and nothing
 * about the binary reproducible. Measured: two runs a second apart shared not
 * one output file.
 *
 * F-Droid rebuilds a tagged commit and compares it against what was published;
 * if they differ, the app is not accepted. So this has to be a fact about the
 * SOURCE rather than about the moment, and the commit is exactly that.
 *
 * LEXARII_BUILD_VERSION wins, so a release pipeline that already knows the
 * version can say so. Then the commit. Then "dev", for a tree with no git at
 * all - still deterministic, which is the property that matters.
 */
function buildVersion() {
	if (process.env.LEXARII_BUILD_VERSION) return process.env.LEXARII_BUILD_VERSION;
	// By absolute path, from an absolute PATH entry: see scripts/on-path.mjs.
	const git = onPath('git');
	if (!git) return 'dev';
	try {
		return execFileSync(git, ['rev-parse', 'HEAD'], {
			cwd: fileURLToPath(new URL('.', import.meta.url)),
			stdio: ['ignore', 'pipe', 'ignore']
		})
			.toString()
			.trim();
	} catch {
		return 'dev';
	}
}

/**
 * The hash of the inline theme script in src/app.html, for script-src.
 *
 * That script has to be inline and synchronous: applying the theme from a
 * component paints one light frame first, which on a dark theme is a flash of
 * white at every cold load. Moving it to a file would fix the CSP and put the
 * flash back on the round trip.
 *
 * SvelteKit's `mode: 'hash'` hashes the scripts IT generates, not one written
 * by hand in the template, so this is computed from the same file the build
 * reads - which means editing the script cannot leave a stale hash behind and
 * silently break the theme in production. Hashing the source rather than
 * pinning a constant is the whole point.
 *
 * CSP hashes the script's exact text content, so the bytes between the tags
 * are what goes in, untrimmed.
 */
function themeScriptHash() {
	const html = readFileSync(new URL('./src/app.html', import.meta.url), 'utf8');
	const match = html.match(/<script>([\s\S]*?)<\/script>/);
	if (!match) {
		throw new Error(
			'svelte.config.js expected an inline <script> in src/app.html to hash for the ' +
				'Content-Security-Policy. If it was deliberately removed, remove this too.'
		);
	}
	return 'sha256-' + createHash('sha256').update(match[1], 'utf8').digest('base64');
}

/** @type {import('@sveltejs/kit').Config} */
export default {
	kit: {
		// Static output, embedded into the Go binary with go:embed and served by
		// the Go server. One binary is the whole install.
		adapter: adapter({ fallback: 'index.html', strict: false }),
		version: { name: buildVersion() },
		// Where scripts may come from, enforced by the browser.
		//
		// The Go server sets the directives that do not depend on this
		// (frame-ancestors, base-uri, object-src, form-action) as a header on
		// every response; this adds the one that does. Both policies are
		// enforced together, so a script must satisfy this and a frame must
		// satisfy that.
		//
		// `mode: 'hash'` makes SvelteKit hash its own inline bootstrap rather
		// than asking for 'unsafe-inline', which would give away most of what
		// a script-src is for.
		//
		// style-src is deliberately absent. The template wraps the app in a
		// `style="display: contents"` attribute and CSP3 governs style
		// attributes under style-src, so declaring it would need
		// 'unsafe-hashes' or a refactor of the layout for very little: the
		// value here is stopping injected SCRIPTS.
		csp: {
			mode: 'hash',
			directives: {
				'script-src': ['self', themeScriptHash()]
			}
		},
		alias: {
			$components: 'src/lib/components',
			$api: 'src/lib/api'
		}
	}
};
