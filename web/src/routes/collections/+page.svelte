<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<script lang="ts">
	import * as api from '$api/client';
	import type { Collection } from '$api/client';

	let items = $state<Collection[]>([]);
	let loading = $state(true);
	let error = $state<string | null>(null);
	let newName = $state('');
	let busy = $state(false);

	$effect(() => {
		void load();
	});

	async function load() {
		try {
			items = (await api.collections()).items;
			error = null;
		} catch (err) {
			error = err instanceof Error ? err.message : String(err);
		} finally {
			loading = false;
		}
	}

	async function make() {
		const name = newName.trim();
		if (!name || busy) return;
		busy = true;
		try {
			await api.createCollection(name);
			newName = '';
			await load();
		} catch (err) {
			error = err instanceof Error ? err.message : String(err);
		} finally {
			busy = false;
		}
	}

	async function remove(c: Collection) {
		// The books are untouched, and saying so is the difference between a
		// confirmation somebody reads and one they click through.
		if (!confirm(`Delete “${c.name}”? The ${c.books} book${c.books === 1 ? '' : 's'} in it stay in your library.`)) {
			return;
		}
		try {
			await api.deleteCollection(c.id);
			await load();
		} catch (err) {
			error = err instanceof Error ? err.message : String(err);
		}
	}
</script>

<svelte:head><title>Collections · Lexarii</title></svelte:head>

<main>
	<header>
		<a class="back" href="/">← Library</a>
		<h1>Collections</h1>
		<p class="hint">
			Your own groupings: the books behind one chapter, one essay, one reading
			group. Unlike subjects, which come out of the books, these are yours.
		</p>
	</header>

	<form
		class="make"
		onsubmit={(e) => {
			e.preventDefault();
			void make();
		}}
	>
		<input bind:value={newName} placeholder="New collection…" aria-label="Name of the new collection" />
		<button class="button" disabled={busy || !newName.trim()}>Add</button>
	</form>

	{#if error}
		<p class="error" role="alert">{error}</p>
	{/if}

	{#if loading}
		<p class="quiet">Looking…</p>
	{:else if items.length === 0}
		<p class="quiet">No collections yet. Name one above, then add books from their pages.</p>
	{:else}
		<ul>
			{#each items as c (c.id)}
				<li>
					<a class="name" href="/collections/{c.id}">{c.name}</a>
					<span class="count">{c.books} book{c.books === 1 ? '' : 's'}</span>
					{#if c.description}<p class="description">{c.description}</p>{/if}
					<button class="link danger" onclick={() => void remove(c)}>Delete</button>
				</li>
			{/each}
		</ul>
	{/if}
</main>

<style>
	main {
		max-width: var(--measure);
		margin: 0 auto;
		padding: var(--space-6) var(--space-4);
	}

	.back {
		color: var(--fg-muted);
		font-size: var(--text-sm);
		text-decoration: none;
	}

	h1 {
		margin: var(--space-2) 0 var(--space-1);
		font-family: var(--font-serif);
	}

	.hint,
	.quiet {
		margin: 0 0 var(--space-4);
		color: var(--fg-muted);
		font-size: var(--text-sm);
	}

	.make {
		display: flex;
		gap: var(--space-2);
		margin-bottom: var(--space-6);
	}

	.make input {
		flex: 1;
		min-height: 44px;
		padding: 0 var(--space-3);
		border: 1px solid var(--border);
		border-radius: var(--radius-md);
		background: var(--bg-raised);
		color: var(--fg);
		font: inherit;
	}

	.button {
		min-height: 44px;
		padding: 0 var(--space-4);
		border: 1px solid var(--border);
		border-radius: var(--radius-md);
		background: var(--bg-raised);
		color: var(--fg);
		font: inherit;
		font-size: var(--text-sm);
		cursor: pointer;
	}

	.button:disabled {
		opacity: 0.5;
		cursor: default;
	}

	ul {
		margin: 0;
		padding: 0;
		list-style: none;
	}

	li {
		display: grid;
		grid-template-columns: 1fr auto auto;
		align-items: baseline;
		gap: var(--space-2) var(--space-3);
		padding: var(--space-3) 0;
		border-bottom: 1px solid var(--border);
	}

	.name {
		color: var(--fg);
		font-family: var(--font-serif);
		font-size: var(--text-lg);
		text-decoration: none;
	}

	.name:hover {
		text-decoration: underline;
	}

	.count {
		color: var(--fg-subtle);
		font-size: var(--text-sm);
	}

	.description {
		grid-column: 1 / -1;
		margin: 0;
		color: var(--fg-muted);
		font-size: var(--text-sm);
	}

	.link {
		border: 0;
		background: none;
		color: var(--fg-muted);
		font: inherit;
		font-size: var(--text-xs);
		cursor: pointer;
	}

	.link.danger:hover {
		color: var(--danger);
	}

	.error {
		color: var(--danger);
	}
</style>
