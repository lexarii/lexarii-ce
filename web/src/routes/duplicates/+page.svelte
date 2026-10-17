<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<script lang="ts">
	import * as api from '$api/client';
	import type { DuplicateGroup, DuplicateWork } from '$api/client';

	let groups = $state<DuplicateGroup[] | null>(null);
	let error = $state<string | null>(null);
	let busy = $state<string | null>(null);
	let done = $state<string[]>([]);

	// Which copy to keep, per group. Defaults to the one that has been read:
	// nothing is lost either way, but somebody's highlights and their place in
	// the book are on one of these, and that is the one they mean.
	let keeping = $state<Record<number, string>>({});

	$effect(() => {
		void load();
	});

	async function load() {
		try {
			const list = await api.duplicates();
			groups = list.items;
			const picks: Record<number, string> = {};
			list.items.forEach((group, i) => {
				// The server only reports a group of two or more, but the type
				// cannot say so, and a default of "" is a disabled button rather
				// than a crash.
				const preferred = group.works.find(read) ?? group.works[0];
				picks[i] = preferred?.workId ?? '';
			});
			keeping = picks;
			error = null;
		} catch (err) {
			error = err instanceof Error ? err.message : String(err);
			groups = [];
		}
	}

	function read(work: DuplicateWork): boolean {
		return work.hasProgress || work.annotations > 0;
	}

	async function merge(group: DuplicateGroup, index: number) {
		const keep = keeping[index];
		if (!keep) return;
		const fold = group.works.filter((w) => w.workId !== keep).map((w) => w.workId);
		if (fold.length === 0) return;

		busy = keep;
		try {
			await api.mergeWorks(keep, fold);
			done = [...done, keep];
			await load();
		} catch (err) {
			error = err instanceof Error ? err.message : String(err);
		} finally {
			busy = null;
		}
	}

	function describe(work: DuplicateWork): string {
		const parts: string[] = [];
		if (work.formats?.length) parts.push(work.formats.join(', ').toUpperCase());
		parts.push(`${work.files} file${work.files === 1 ? '' : 's'}`);
		if (work.annotations > 0) {
			parts.push(`${work.annotations} highlight${work.annotations === 1 ? '' : 's'}`);
		}
		if (work.hasProgress) parts.push('you have read this one');
		return parts.join(' · ');
	}
</script>

<svelte:head><title>Duplicates · Lexarii</title></svelte:head>

<article>
	<a class="back" href="/">← Library</a>
	<h1>Books that arrived twice</h1>

	<p class="hint">
		A book becomes two when its files have different names: the scanner matches a new
		file to an existing book by filename, so one book downloaded twice under two names
		is two books.
	</p>

	{#if error}
		<p class="error" role="alert">{error}</p>
	{/if}

	{#if groups === null}
		<p class="loading">Looking…</p>
	{:else if groups.length === 0}
		<p class="empty">
			{done.length > 0 ? 'Nothing else looks duplicated.' : 'No duplicates found.'}
		</p>
	{:else}
		{#each groups as group, i (group.works[0]?.workId ?? i)}
			<section>
				<h2>{group.works[0]?.title ?? 'This book'}</h2>
				<p class="reason">Found by {group.reason}.</p>

				<!--
					Choosing which to keep, rather than a Merge button that decides.
					Nothing is lost either way - highlights and positions belong to the
					FILE and move with it - but the copy somebody has read is the one
					whose title and description they recognise.
				-->
				<ul>
					{#each group.works as work (work.workId)}
						<li>
							<label>
								<input type="radio" bind:group={keeping[i]} value={work.workId} />
								<span class="what">
									<a href="/works/{work.workId}">{work.title}</a>
									{#if work.authors?.length}
										<span class="by">{work.authors.join(', ')}</span>
									{:else}
										<span class="by missing">no author</span>
									{/if}
									<span class="meta">{describe(work)}</span>
								</span>
							</label>
						</li>
					{/each}
				</ul>

				<div class="actions">
					<button class="button" disabled={busy !== null} onclick={() => void merge(group, i)}>
						{busy === keeping[i] ? 'Merging…' : 'Keep the selected one, fold in the rest'}
					</button>
				</div>
				<p class="hint">
					Every file, highlight and reading position moves to the book you keep. Nothing
					is deleted except the duplicate's title.
				</p>
			</section>
		{/each}
	{/if}
</article>

<style>
	article {
		max-width: 46rem;
		margin-inline: auto;
		padding: var(--space-6) var(--space-4) var(--space-16);
	}

	.back {
		display: inline-flex;
		min-height: 44px;
		align-items: center;
		color: var(--fg-muted);
		text-decoration: none;
		font-size: var(--text-sm);
	}

	h1 {
		margin: var(--space-2) 0 var(--space-3);
	}

	h2 {
		margin: 0 0 var(--space-1);
		font-size: var(--text-lg);
	}

	.hint,
	.reason {
		color: var(--fg-muted);
		font-size: var(--text-sm);
		margin: 0 0 var(--space-4);
	}

	.error {
		color: var(--danger);
	}

	section {
		border: 1px solid var(--border);
		border-radius: var(--radius-md);
		padding: var(--space-4);
		margin-bottom: var(--space-6);
	}

	ul {
		list-style: none;
		margin: 0 0 var(--space-4);
		padding: 0;
		display: grid;
		gap: var(--space-2);
	}

	label {
		display: flex;
		gap: var(--space-3);
		align-items: flex-start;
		/* The whole row is the target, not the dot: this is a phone-sized
		   decision as much as a desktop one. */
		min-height: 44px;
		padding: var(--space-2);
		border-radius: var(--radius-md);
		cursor: pointer;
	}

	label:hover {
		background: var(--bg-sunken);
	}

	input[type='radio'] {
		margin-top: var(--space-1);
		width: 20px;
		height: 20px;
		flex: none;
	}

	.what {
		display: grid;
		gap: var(--space-1);
		min-width: 0;
	}

	.what a {
		color: var(--fg);
	}

	.by,
	.meta {
		font-size: var(--text-sm);
		color: var(--fg-muted);
	}

	.by.missing {
		font-style: italic;
	}

	.actions {
		display: flex;
		gap: var(--space-3);
		flex-wrap: wrap;
		margin-bottom: var(--space-3);
	}
</style>
