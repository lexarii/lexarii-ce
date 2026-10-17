<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<script lang="ts">
	import * as api from '$api/client';
	import type { BlockedFile } from '$api/client';

	// Files an administrator removed for everybody, and the block that keeps
	// them off. Administrators only: the server refuses anybody else, and the
	// page says so rather than showing an empty list.
	let items = $state<BlockedFile[]>([]);
	let loading = $state(true);
	let error = $state<string | null>(null);
	let notice = $state<string | null>(null);
	let busy = $state<string | null>(null);
	// Lifting a block is asked twice, in place: one click should not undo a
	// decision somebody recorded a reason for.
	let confirming = $state<string | null>(null);

	$effect(() => {
		void load();
	});

	async function load() {
		loading = true;
		try {
			items = (await api.blockedFiles()).items;
			error = null;
		} catch (err) {
			error = err instanceof Error ? err.message : String(err);
		} finally {
			loading = false;
		}
	}

	async function lift(item: BlockedFile) {
		busy = item.contentHash;
		notice = null;
		try {
			await api.unblock(item.contentHash);
			items = items.filter((i) => i.contentHash !== item.contentHash);
			confirming = null;
			// Nothing comes back by itself. An upload's bytes were deleted, so
			// only adding them again brings it back; a library file is found
			// by the next scan and offered in the archive.
			notice =
				`${label(item)} can be added again. Nothing came back by itself: an uploaded ` +
				'file comes back when somebody uploads it again, and a file in the library ' +
				'folder appears in the archive after the next scan.';
		} catch (err) {
			error = err instanceof Error ? err.message : String(err);
		} finally {
			busy = null;
		}
	}

	// Finishing a removal whose stored copies could not all be deleted.
	async function finish(item: BlockedFile) {
		busy = item.contentHash;
		notice = null;
		try {
			const result = await api.discardCopies(item.contentHash);
			items = items.map((i) =>
				i.contentHash === item.contentHash ? { ...i, copiesRemain: result.copiesRemain } : i
			);
			notice = result.copiesRemain
				? `Some stored copies of ${label(item)} still could not be deleted. The server log says why.`
				: `Every stored copy of ${label(item)} is deleted.`;
		} catch (err) {
			error = err instanceof Error ? err.message : String(err);
		} finally {
			busy = null;
		}
	}

	function label(item: BlockedFile): string {
		return item.title ? `“${item.title}”` : 'The file';
	}
</script>

<svelte:head><title>Blocked files · Lexarii</title></svelte:head>

<div class="shell">
	<header>
		<a class="back" href="/account">← Account</a>
		<h1>Blocked files</h1>
		<p class="hint">
			Files removed from this library for everybody. Uploading one again is refused, and a
			scan passes over it. Lifting a block brings nothing back by itself: the file can simply
			be added again.
		</p>
		{#if notice}<p class="notice" role="status">{notice}</p>{/if}
		{#if error}<p class="error" role="alert">{error}</p>{/if}
	</header>

	{#if loading}
		<p class="loading">Loading…</p>
	{:else if error && items.length === 0}
		<!-- The error above says why; an empty list would say nothing is blocked. -->
	{:else if items.length === 0}
		<p class="empty">Nothing is blocked.</p>
	{:else}
		<ul>
			{#each items as item (item.contentHash)}
				<li>
					<div class="what">
						<span class="title">{item.title || 'A file no longer on record'}</span>
						<span class="reason">{item.reason}</span>
						<span class="meta">
							{new Date(item.blockedAt).toLocaleDateString()}
							{#if item.blockedBy}· by {item.blockedBy}{/if}
						</span>
						<code class="hash" title={item.contentHash}>{item.contentHash}</code>
						{#if item.copiesRemain}
							<span class="remain">Some stored copies could not be deleted yet.</span>
						{/if}
					</div>
					<div class="actions">
						{#if item.copiesRemain}
							<button
								class="button"
								disabled={busy === item.contentHash}
								onclick={() => void finish(item)}
							>
								{busy === item.contentHash ? 'Deleting…' : 'Finish removing files'}
							</button>
						{/if}
						{#if confirming === item.contentHash}
							<button
								class="button"
								disabled={busy === item.contentHash}
								onclick={() => void lift(item)}
							>
								{busy === item.contentHash ? 'Lifting…' : 'Yes, lift it'}
							</button>
							<button class="button" onclick={() => (confirming = null)}>Cancel</button>
						{:else}
							<button class="button" onclick={() => (confirming = item.contentHash)}>
								Lift the block
							</button>
						{/if}
					</div>
				</li>
			{/each}
		</ul>
	{/if}
</div>

<style>
	.shell {
		max-width: 52rem;
		margin: 0 auto;
		padding: var(--space-6) var(--space-4) var(--space-12);
	}

	header {
		margin-bottom: var(--space-6);
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
		margin: 0;
		max-width: var(--measure);
		color: var(--fg-muted);
		font-size: var(--text-sm);
	}

	ul {
		list-style: none;
		margin: 0;
		padding: 0;
		border: 1px solid var(--border);
		border-radius: var(--radius-md);
		background: var(--bg-raised);
	}

	li {
		display: flex;
		flex-wrap: wrap;
		align-items: start;
		justify-content: space-between;
		gap: var(--space-3);
		padding: var(--space-4);
	}

	li + li {
		border-top: 1px solid var(--border);
	}

	.what {
		display: flex;
		flex-direction: column;
		gap: var(--space-1);
		min-width: 0;
		flex: 1 1 18rem;
	}

	.title {
		font-weight: 600;
		overflow-wrap: anywhere;
	}

	.reason {
		overflow-wrap: anywhere;
	}

	.meta {
		color: var(--fg-muted);
		font-size: var(--text-sm);
	}

	/* The hash is the block's identity, and long: it wraps rather than
	   widening the page on a phone. */
	.hash {
		color: var(--fg-subtle);
		font-size: var(--text-xs);
		overflow-wrap: anywhere;
	}

	.remain {
		color: var(--danger);
		font-size: var(--text-sm);
	}

	.actions {
		display: flex;
		flex-wrap: wrap;
		gap: var(--space-2);
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
</style>
