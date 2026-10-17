<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<script lang="ts">
	import { fieldLabel } from '$lib/fields';
	import * as api from '$api/client';
	import type { Suggestion } from '$api/client';

	let items = $state<Suggestion[]>([]);
	let loading = $state(true);
	let error = $state<string | null>(null);
	let busy = $state<string | null>(null);
	let notice = $state<string | null>(null);
	let starting = $state(false);

	$effect(() => {
		void load();
	});

	async function load() {
		loading = true;
		try {
			items = (await api.suggestions()).items;
			error = null;
		} catch (err) {
			error = err instanceof Error ? err.message : String(err);
		} finally {
			loading = false;
		}
	}

	async function resolve(suggestion: Suggestion, accept: boolean) {
		busy = suggestion.id;
		try {
			await api.resolveSuggestion(suggestion.id, accept);
			items = items.filter((s) => s.id !== suggestion.id);
		} catch (err) {
			error = err instanceof Error ? err.message : String(err);
		} finally {
			busy = null;
		}
	}

	/** Decides a whole book at once, which is how these are actually read. */
	async function resolveBook(group: Group, accept: boolean) {
		busy = group.workId;
		try {
			for (const suggestion of group.items) {
				await api.resolveSuggestion(suggestion.id, accept);
			}
			const decided = new Set(group.items.map((s) => s.id));
			items = items.filter((s) => !decided.has(s.id));
		} catch (err) {
			error = err instanceof Error ? err.message : String(err);
		} finally {
			busy = null;
		}
	}

	async function start() {
		starting = true;
		notice = null;
		try {
			const result = await api.startEnrichment();
			notice = !result.modelConfigured
				? 'No model is configured, so nothing will happen until one is. Set LEXARII_LLM_BASE_URL and restart.'
				: result.eligible === 0
					? 'Every book has already been described by this prompt.'
					: `Queued ${result.queued} of ${result.eligible} books. Suggestions appear here as they finish.`;
		} catch (err) {
			error = err instanceof Error ? err.message : String(err);
		} finally {
			starting = false;
		}
	}

	interface Group {
		workId: string;
		workTitle: string;
		items: Suggestion[];
	}

	// Grouped by book, because a person reviews a book, not a field. Five
	// separate rows for one book is five decisions where there is really one.
	const groups = $derived.by(() => {
		const byWork = new Map<string, Group>();
		for (const item of items) {
			const group = byWork.get(item.workId) ?? {
				workId: item.workId,
				workTitle: item.workTitle,
				items: []
			};
			group.items.push(item);
			byWork.set(item.workId, group);
		}
		return [...byWork.values()];
	});

	function shown(value: unknown): string {
		if (Array.isArray(value)) return value.join(', ');
		if (value === null || value === undefined) return '';
		if (typeof value === 'string') return value;
		return JSON.stringify(value);
	}

	/** Confidence as the model reported it, rounded to something readable. */
	function percent(confidence: number | undefined): string {
		if (confidence === undefined) return '';
		return Math.round(confidence * 100) + '%';
	}
</script>

<svelte:head><title>Suggestions · Lexarii</title></svelte:head>

<div class="shell">
	<header>
		<a class="back" href="/">← Library</a>
		<h1>Suggestions</h1>
		<p class="hint">
			What your model proposed. Nothing here has changed a book: these are the
			fields where the book already said something, or where the model was not sure
			enough to be taken at its word.
		</p>
		<div class="row">
			<button class="button" onclick={() => void start()} disabled={starting}>
				{starting ? 'Queueing…' : 'Describe more books'}
			</button>
			<button class="button" onclick={() => void load()} disabled={loading}>Refresh</button>
		</div>
		{#if notice}<p class="notice" role="status">{notice}</p>{/if}
		{#if error}<p class="error" role="alert">{error}</p>{/if}
	</header>

	{#if loading}
		<p class="loading">Loading…</p>
	{:else if groups.length === 0}
		<div class="empty">
			<p>Nothing waiting.</p>
			<p class="hint">
				Either no model has run yet, or everything it proposed was applied
				because the books had nothing there to protect.
			</p>
		</div>
	{:else}
		{#each groups as group (group.workId)}
			<section class="book">
				<div class="book-head">
					<h2><a href="/works/{group.workId}">{group.workTitle}</a></h2>
					<div class="row">
						<button
							class="button"
							onclick={() => void resolveBook(group, true)}
							disabled={busy === group.workId}
						>
							Accept all
						</button>
						<button
							class="button"
							onclick={() => void resolveBook(group, false)}
							disabled={busy === group.workId}
						>
							Reject all
						</button>
					</div>
				</div>

				<ul>
					{#each group.items as item (item.id)}
						<li>
							<div class="field">
								<span class="label">{fieldLabel(item.field)}</span>
								{#if item.confidence !== undefined}
									<span class="confidence" title="How well the sample supported this">
										{percent(item.confidence)}
									</span>
								{/if}
							</div>

							<div class="values">
								{#if item.current}
									<p class="current"><span class="tag">now</span>{item.current}</p>
								{/if}
								<p class="proposed"><span class="tag suggested">proposed</span>{shown(item.value)}</p>
							</div>

							<div class="row actions">
								<button
									class="button"
									onclick={() => void resolve(item, true)}
									disabled={busy === item.id || busy === group.workId}
								>
									Accept
								</button>
								<button
									class="button"
									onclick={() => void resolve(item, false)}
									disabled={busy === item.id || busy === group.workId}
								>
									Reject
								</button>
							</div>

							{#if item.model}
								<p class="provenance">{item.model} · {item.promptId}</p>
							{/if}
						</li>
					{/each}
				</ul>
			</section>
		{/each}
	{/if}
</div>

<style>
	.shell {
		max-width: 52rem;
		margin: 0 auto;
		padding: var(--space-6) var(--space-4) var(--space-12);
	}

	header {
		margin-bottom: var(--space-8);
	}

	.back {
		display: inline-block;
		margin-bottom: var(--space-4);
		color: var(--fg-muted);
		font-size: var(--text-sm);
		text-decoration: none;
	}

	.back:hover {
		color: var(--fg);
	}

	h1 {
		margin: 0 0 var(--space-2);
		font-family: var(--font-serif);
		font-size: var(--text-2xl);
	}

	.hint {
		margin: 0 0 var(--space-4);
		max-width: var(--measure);
		color: var(--fg-muted);
		font-size: var(--text-sm);
	}

	.row {
		display: flex;
		flex-wrap: wrap;
		gap: var(--space-3);
	}

	.book {
		margin-bottom: var(--space-6);
		border: 1px solid var(--border);
		border-radius: var(--radius-md);
		background: var(--bg-raised);
		overflow: hidden;
	}

	.book-head {
		display: flex;
		flex-wrap: wrap;
		align-items: center;
		justify-content: space-between;
		gap: var(--space-3);
		padding: var(--space-3) var(--space-4);
		border-bottom: 1px solid var(--border);
		background: var(--bg-sunken);
	}

	.book-head h2 {
		margin: 0;
		font-family: var(--font-serif);
		font-size: var(--text-lg);
		min-width: 0;
	}

	.book-head a {
		color: inherit;
		text-decoration: none;
	}

	.book-head a:hover {
		text-decoration: underline;
	}

	ul {
		list-style: none;
		margin: 0;
		padding: 0;
	}

	li {
		display: grid;
		grid-template-columns: 7rem 1fr auto;
		gap: var(--space-3) var(--space-4);
		align-items: start;
		padding: var(--space-4);
	}

	li + li {
		border-top: 1px solid var(--border);
	}

	.field {
		display: flex;
		flex-direction: column;
		gap: var(--space-1);
	}

	.label {
		font-size: var(--text-sm);
		font-weight: 600;
	}

	.confidence {
		font-size: var(--text-xs);
		color: var(--fg-muted);
		font-variant-numeric: tabular-nums;
	}

	.values {
		min-width: 0;
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
	}

	.values p {
		margin: 0;
		overflow-wrap: anywhere;
	}

	.current {
		color: var(--fg-muted);
	}

	/* The proposal is not emphasised over what is already there. Reviewing means
	   comparing, and styling one side as the answer is a thumb on the scale. */
	.tag {
		display: inline-block;
		margin-right: var(--space-2);
		padding: 0 var(--space-2);
		border: 1px solid var(--border);
		border-radius: var(--radius-sm);
		font-size: var(--text-xs);
		text-transform: uppercase;
		letter-spacing: 0.04em;
		color: var(--fg-muted);
	}

	.provenance {
		grid-column: 2 / -1;
		margin: 0;
		font-size: var(--text-xs);
		color: var(--fg-subtle);
	}

	.button {
		display: inline-flex;
		align-items: center;
		min-height: 44px;
		padding: 0 var(--space-4);
		border: 1px solid var(--border);
		border-radius: var(--radius-md);
		background: var(--bg-raised);
		color: var(--fg);
		font-size: var(--text-sm);
		cursor: pointer;
	}

	.button:hover:not(:disabled) {
		background: var(--bg-sunken);
	}

	.button:disabled {
		opacity: 0.6;
		cursor: default;
	}

	.notice,
	.error,
	.loading {
		margin: var(--space-3) 0 0;
		font-size: var(--text-sm);
	}

	.error {
		color: var(--danger);
	}

	.notice,
	.loading {
		color: var(--fg-muted);
	}

	.empty {
		padding: var(--space-8) 0;
		color: var(--fg-muted);
	}

	@media (max-width: 40rem) {
		li {
			grid-template-columns: 1fr;
		}

		.provenance {
			grid-column: 1;
		}

		.actions {
			justify-content: flex-start;
		}
	}
</style>
