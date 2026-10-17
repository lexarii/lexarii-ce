<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<script lang="ts">
	import * as api from '$api/client';
	import type { RecentAnnotation } from '$api/client';
	import { appendPage } from '$lib/notes/paging';

	let items = $state<RecentAnnotation[]>([]);
	let loading = $state(true);
	let error = $state<string | null>(null);
	let filter = $state('');
	// Where the next page starts; empty once everything is shown.
	let nextCursor = $state('');
	let loadingMore = $state(false);
	let moreError = $state<string | null>(null);

	$effect(() => {
		void load();
	});

	async function load() {
		try {
			const page = await api.recentAnnotations();
			items = page.items;
			nextCursor = page.nextCursor ?? '';
		} catch (err) {
			error = err instanceof Error ? err.message : String(err);
		} finally {
			loading = false;
		}
	}

	async function loadMore() {
		if (!nextCursor || loadingMore) return;
		loadingMore = true;
		moreError = null;
		try {
			const page = await api.recentAnnotations({ cursor: nextCursor });
			items = appendPage(items, page.items, (i) => i.annotation.id);
			nextCursor = page.nextCursor ?? '';
		} catch (err) {
			moreError = err instanceof Error ? err.message : String(err);
		} finally {
			loadingMore = false;
		}
	}

	/**
	 * Where a mark lives, so following it lands on the passage rather than on
	 * page one.
	 *
	 * The same `?at=` the citations use, and the same split between the two
	 * readers: a PDF is its own route.
	 */
	function href(item: RecentAnnotation): string {
		const pdf = item.format === 'pdf';
		const at = `?at=${encodeURIComponent(item.annotation.start)}`;
		return `/read/${item.bookFileId}${pdf ? '/pdf' : ''}${at}`;
	}

	// Grouped by book, because that is how somebody looks for one: they
	// remember the book first and the sentence second.
	let books = $derived.by(() => {
		const needle = filter.trim().toLowerCase();
		const matching = needle
			? items.filter(
					(i) =>
						(i.annotation.selectedText ?? '').toLowerCase().includes(needle) ||
						(i.annotation.note ?? '').toLowerCase().includes(needle) ||
						i.title.toLowerCase().includes(needle)
				)
			: items;

		const grouped = new Map<string, { title: string; workId: string; marks: RecentAnnotation[] }>();
		for (const item of matching) {
			const seen = grouped.get(item.workId);
			if (seen) seen.marks.push(item);
			else grouped.set(item.workId, { title: item.title, workId: item.workId, marks: [item] });
		}
		return [...grouped.values()];
	});

	let total = $derived(books.reduce((n, b) => n + b.marks.length, 0));

	function when(iso: string): string {
		return new Date(iso).toLocaleDateString();
	}
</script>

<svelte:head><title>Your notes · Lexarii</title></svelte:head>

<main>
	<header>
		<a class="back" href="/">← Library</a>
		<h1>Your notes</h1>
		<p class="hint">
			Every passage you have marked, newest first. Following one opens the book at
			that page.
		</p>
	</header>

	{#if error}
		<p class="error" role="alert">{error}</p>
	{:else if loading}
		<p class="quiet">Looking…</p>
	{:else if items.length === 0}
		<p class="quiet">
			Nothing marked yet. Select a passage while reading and choose
			<strong>Highlight</strong> or <strong>Note</strong>.
		</p>
	{:else}
		<input
			class="filter"
			type="search"
			bind:value={filter}
			placeholder="Search your notes and the passages they mark…"
			aria-label="Search your notes"
		/>

		{#if total === 0}
			<p class="quiet">
				Nothing matches “{filter}”{nextCursor ? ' in the notes shown so far' : ''}.
			</p>
		{:else}
			{#each books as book (book.workId)}
				<section>
					<h2><a href="/works/{book.workId}">{book.title}</a></h2>
					<ul>
						{#each book.marks as item (item.annotation.id)}
							<li class:orphaned={item.annotation.orphaned}>
								<a class="passage" href={href(item)}>
									{#if item.annotation.selectedText}
										<q>{item.annotation.selectedText}</q>
									{:else}
										<span class="unquoted">a place in the book</span>
									{/if}
								</a>
								{#if item.annotation.note}
									<p class="note">{item.annotation.note}</p>
								{/if}
								<p class="meta">
									{when(item.annotation.createdAt)}
									{#if item.annotation.orphaned}
										· <span class="warn">the book changed; this may no longer line up</span>
									{/if}
								</p>
							</li>
						{/each}
					</ul>
				</section>
			{/each}
		{/if}

		{#if nextCursor}
			<div class="more">
				<button onclick={loadMore} disabled={loadingMore} aria-busy={loadingMore}>
					{loadingMore ? 'Loading…' : 'Load older notes'}
				</button>
				{#if moreError}
					<p class="error" role="alert">{moreError}</p>
				{/if}
			</div>
		{/if}
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

	.filter {
		width: 100%;
		min-height: 44px;
		margin-bottom: var(--space-6);
		padding: 0 var(--space-3);
		border: 1px solid var(--border);
		border-radius: var(--radius-md);
		background: var(--bg-raised);
		color: var(--fg);
		font: inherit;
	}

	section {
		margin-bottom: var(--space-8);
	}

	h2 {
		margin: 0 0 var(--space-3);
		font-size: var(--text-base);
	}

	h2 a {
		color: var(--fg);
		text-decoration: none;
	}

	h2 a:hover {
		text-decoration: underline;
	}

	ul {
		margin: 0;
		padding: 0;
		list-style: none;
	}

	li {
		margin-bottom: var(--space-4);
		padding-left: var(--space-3);
		border-left: 3px solid var(--accent);
	}

	/* A mark whose passage moved under it. Still shown - it is the reader's
	   own note - but not pretending to point at anything. */
	li.orphaned {
		border-left-color: var(--border);
	}

	.passage {
		color: var(--fg);
		font-family: var(--font-serif);
		line-height: var(--leading-reading);
		text-decoration: none;
	}

	.passage:hover q {
		background: color-mix(in srgb, var(--accent) 18%, transparent);
	}

	.unquoted {
		color: var(--fg-muted);
		font-style: italic;
	}

	.note {
		margin: var(--space-2) 0 0;
		font-size: var(--text-sm);
	}

	.meta {
		margin: var(--space-1) 0 0;
		color: var(--fg-subtle);
		font-size: var(--text-xs);
	}

	.warn {
		color: var(--fg-muted);
	}

	.error {
		color: var(--danger);
	}

	.more {
		margin: var(--space-6) 0;
		text-align: center;
	}

	.more button {
		padding: 0 var(--space-4);
		border: 1px solid var(--border);
		border-radius: var(--radius-md);
		background: var(--bg-raised);
		color: var(--fg);
		font-size: var(--text-sm);
		cursor: pointer;
	}

	.more button:hover {
		background: var(--bg-sunken);
	}

	.more button[disabled] {
		opacity: 0.6;
		cursor: default;
	}
</style>
