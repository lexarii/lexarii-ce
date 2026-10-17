<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<script lang="ts">
	import { page } from '$app/state';
	import * as api from '$api/client';
	import type { CollectionDetail } from '$api/client';
	import BookCard from '$lib/components/BookCard.svelte';

	let detail = $state<CollectionDetail | null>(null);
	let loading = $state(true);
	let error = $state<string | null>(null);

	// The three that the tools people actually use disagree about, same as on a
	// book's own page.
	const citationFormats = [
		{ id: 'bibtex', label: 'BibTeX', tool: 'For LaTeX and BibLaTeX' },
		{ id: 'ris', label: 'RIS', tool: 'For Zotero, Mendeley and EndNote' },
		{ id: 'csl-json', label: 'CSL-JSON', tool: 'For Pandoc and most modern tools' }
	];

	$effect(() => {
		const id = page.params.id;
		if (id) void load(id);
	});

	async function load(id: string) {
		try {
			detail = await api.collection(id);
			error = null;
		} catch (err) {
			error = err instanceof Error ? err.message : String(err);
		} finally {
			loading = false;
		}
	}

	async function remove(workId: string) {
		const id = page.params.id;
		if (!id) return;
		try {
			await api.removeFromCollection(id, workId);
			await load(id);
		} catch (err) {
			error = err instanceof Error ? err.message : String(err);
		}
	}
</script>

<svelte:head><title>{detail?.name ?? 'Collection'} · Lexarii</title></svelte:head>

<main>
	<header>
		<a class="back" href="/collections">← Collections</a>
		{#if detail}
			<h1>{detail.name}</h1>
			{#if detail.description}<p class="hint">{detail.description}</p>{/if}

			<!--
				The reason most people want a bibliography is a collection: the
				books behind one chapter. Links rather than a copy button, for the
				same reason as on a book's page - a middle click opens a tab, and
				nothing depends on a clipboard API that plain HTTP does not have.
			-->
			<p class="formats">
				Bibliography:
				{#each citationFormats as format (format.id)}
					<a
						href="/api/v1/citations?collection={detail.id}&format={format.id}"
						title={format.tool}
					>
						{format.label}
					</a>
				{/each}
			</p>
		{/if}
	</header>

	{#if error}
		<p class="error" role="alert">{error}</p>
	{:else if loading}
		<p class="quiet">Looking…</p>
	{:else if detail && detail.items.length === 0}
		<p class="quiet">
			Nothing in here yet. Open a book and use <strong>Add to collection</strong>.
		</p>
	{:else if detail}
		<ul class="shelf">
			{#each detail.items as item (item.workId)}
				<li>
					<BookCard {item} />
					<button class="link" onclick={() => void remove(item.workId)}>Remove</button>
				</li>
			{/each}
		</ul>
	{/if}
</main>

<style>
	main {
		max-width: 72rem;
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

	.formats {
		display: flex;
		flex-wrap: wrap;
		gap: var(--space-2);
		align-items: center;
		margin: 0 0 var(--space-6);
		color: var(--fg-muted);
		font-size: var(--text-sm);
	}

	.formats a {
		display: inline-flex;
		align-items: center;
		min-height: 44px;
		padding: 0 var(--space-3);
		border: 1px solid var(--border);
		border-radius: var(--radius-md);
		color: var(--fg);
		text-decoration: none;
	}

	.formats a:hover {
		background: var(--bg-sunken);
	}

	.shelf {
		display: grid;
		grid-template-columns: repeat(auto-fill, minmax(11rem, 1fr));
		gap: var(--space-4);
		margin: 0;
		padding: 0;
		list-style: none;
	}

	.link {
		display: block;
		margin-top: var(--space-1);
		border: 0;
		background: none;
		color: var(--fg-muted);
		font: inherit;
		font-size: var(--text-xs);
		cursor: pointer;
	}

	.link:hover {
		color: var(--danger);
	}

	.error {
		color: var(--danger);
	}
</style>
