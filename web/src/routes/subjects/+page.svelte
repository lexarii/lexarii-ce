<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<script lang="ts">
	import * as api from '$api/client';
	import type { Subject } from '$api/client';

	let subjects = $state<Subject[]>([]);
	let loading = $state(true);
	let error = $state<string | null>(null);
	let showRare = $state(false);

	$effect(() => {
		void load();
	});

	async function load() {
		loading = true;
		try {
			subjects = (await api.subjects()).items;
			error = null;
		} catch (err) {
			error = err instanceof Error ? err.message : String(err);
		} finally {
			loading = false;
		}
	}

	/**
	 * The subjects that name more than one book, and the ones that name exactly
	 * one.
	 *
	 * Split rather than filtered. On a real library 136 of 218 subjects name a
	 * single book - bayesian statistics, ansible, device drivers, econometrics -
	 * and every one of them is true and specific. Hiding them would lose the
	 * only route to a book whose subject nothing else shares; showing them all
	 * at once buries the shape of the library under a wall of ones.
	 */
	const common = $derived(subjects.filter((s) => s.books > 1));
	const rare = $derived(subjects.filter((s) => s.books === 1));

	// The largest count sets the scale, so the bars compare with each other
	// rather than with an arbitrary maximum.
	const largest = $derived(common[0]?.books ?? 1);
</script>

<svelte:head>
	<title>Subjects · Lexarii</title>
</svelte:head>

<div class="shell">
	<a class="back" href="/">← Library</a>
	<h1>Subjects</h1>

	{#if error}
		<p class="error" role="alert">{error}</p>
	{:else if loading}
		<p class="loading">Reading the shelves…</p>
	{:else if subjects.length === 0}
		<p class="empty">
			Nothing is tagged yet. Subjects are added when books are enriched.
		</p>
	{:else}
		<p class="hint">
			{subjects.length} subjects across your library. Each one filters the shelf.
		</p>

		<ul class="subjects">
			{#each common as subject (subject.slug)}
				<li>
					<a href="/?tag={encodeURIComponent(subject.slug)}">{subject.name}</a>
					<span class="bar" style="--share: {(subject.books / largest) * 100}%"></span>
					<span class="count">{subject.books}</span>
				</li>
			{/each}
		</ul>

		{#if rare.length > 0}
			<button class="more" onclick={() => (showRare = !showRare)}>
				{showRare ? 'Hide' : 'Show'}
				{rare.length} subjects with one book each
			</button>

			{#if showRare}
				<!-- A plain list, not bars: every one of them is a single book, and a
				     row of identical bars says nothing. -->
				<ul class="rare">
					{#each rare as subject (subject.slug)}
						<li><a href="/?tag={encodeURIComponent(subject.slug)}">{subject.name}</a></li>
					{/each}
				</ul>
			{/if}
		{/if}
	{/if}
</div>

<style>
	.shell {
		max-width: 44rem;
		margin: 0 auto;
		padding: var(--space-6) var(--space-4) var(--space-12);
	}

	.back {
		color: var(--fg-muted);
		font-size: var(--text-sm);
		text-decoration: none;
	}

	h1 {
		margin: var(--space-2) 0 var(--space-1);
		font-family: var(--font-serif);
		font-size: var(--text-3xl);
	}

	.hint,
	.loading,
	.empty {
		color: var(--fg-muted);
		font-size: var(--text-sm);
	}

	.error {
		color: var(--danger);
	}

	.subjects {
		list-style: none;
		margin: var(--space-6) 0 0;
		padding: 0;
	}

	.subjects li {
		display: grid;
		/* The name column is fixed so the bars share a baseline and can be
		   compared down the page rather than only within a row. */
		grid-template-columns: minmax(8rem, 14rem) 1fr auto;
		align-items: center;
		gap: var(--space-3);
		min-height: 44px;
		border-bottom: 1px solid var(--border);
	}

	.subjects a {
		color: var(--fg);
		text-decoration: none;
		/* "adversarial machine learning" in a 8rem column at phone width has to
		   wrap somewhere rather than push the bar off the screen. */
		overflow-wrap: anywhere;
	}

	.subjects a:hover {
		text-decoration: underline;
	}

	.bar {
		height: 6px;
		border-radius: 999px;
		background: linear-gradient(
			to right,
			var(--accent) 0 var(--share),
			var(--bg-sunken) var(--share) 100%
		);
	}

	.count {
		color: var(--fg-muted);
		font-size: var(--text-sm);
		font-variant-numeric: tabular-nums;
	}

	.more {
		min-height: 44px;
		margin-top: var(--space-4);
		padding: 0 var(--space-3);
		border: 1px solid var(--border);
		border-radius: var(--radius-md);
		background: var(--bg-raised);
		color: var(--fg-muted);
		font: inherit;
		font-size: var(--text-sm);
		cursor: pointer;
	}

	.rare {
		display: flex;
		flex-wrap: wrap;
		gap: var(--space-2);
		list-style: none;
		margin: var(--space-4) 0 0;
		padding: 0;
	}

	.rare a {
		display: inline-flex;
		align-items: center;
		min-height: 36px;
		padding: 0 var(--space-3);
		border: 1px solid var(--border);
		border-radius: var(--radius-md);
		background: var(--bg-raised);
		color: var(--fg-muted);
		font-size: var(--text-sm);
		text-decoration: none;
	}

	.rare a:hover {
		color: var(--fg);
	}
</style>
