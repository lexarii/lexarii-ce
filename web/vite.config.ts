// SPDX-License-Identifier: AGPL-3.0-or-later
import { sveltekit } from '@sveltejs/kit/vite';
// vitest's defineConfig is vite's own plus the `test` block, so the config
// stays type-checked rather than needing a cast.
import { defineConfig } from 'vitest/config';

export default defineConfig(({ mode }) => ({
	plugins: [sveltekit()],
	// Under vitest, Svelte resolves to its server build, where mount() does
	// not exist; the browser build is the one the components run against.
	// Tests only: the app build keeps Vite's own conditions.
	resolve: mode === 'test' ? { conditions: ['browser'] } : undefined,
	test: {
		// The reader's locator logic is plain DOM work, so it is tested against a
		// DOM rather than through the component that uses it.
		environment: 'jsdom',
		// tests/scripts holds the tests of the build and check scripts, which
		// run under Node and say so per file (@vitest-environment node). Plain
		// JavaScript, like the scripts and the reader-page tests: there are no
		// Node type definitions here to check them against.
		include: ['src/**/*.test.ts', 'tests/scripts/**/*.test.mjs'],
		// Only with --coverage, which is `npm run test:coverage`: plain
		// `npm test` measures nothing and costs nothing extra. The LCOV paths
		// are relative to the repository root, so the reader-page tests can
		// add the Android reader's scripts to the same file in the same terms
		// (tests/android-reader/coverage.mjs).
		coverage: {
			provider: 'v8',
			include: ['src/**', 'scripts/**'],
			reportsDirectory: 'coverage',
			reporter: [
				['lcovonly', { projectRoot: decodeURIComponent(new URL('..', import.meta.url).pathname) }],
				'text-summary'
			]
		}
	},
	server: {
		proxy: {
			// The Go server owns the API in dev; Vite owns the client.
			'/api': 'http://localhost:8080',
			// And the installation's branding, when LEXARII_BRANDING_DIR is set.
			'/branding': 'http://localhost:8080'
		}
	}
}));
