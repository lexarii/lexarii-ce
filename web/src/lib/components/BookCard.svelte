<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<script lang="ts">
	import type { LibraryItem } from '$api/client';

	let { item }: { item: LibraryItem } = $props();

	/**
	 * A stable hue per book.
	 *
	 * Covers are rendered from a PDF's first page in the browser and uploaded,
	 * so most books get a real one eventually - but not instantly, and not the
	 * ones that fail to render. A grid of identical grey rectangles is
	 * unscannable; giving each book its own steady colour makes the shelf
	 * navigable by memory, the way a physical one is.
	 *
	 * Derived from the title so it never changes between loads or devices.
	 */
	function hueFor(title: string): number {
		let h = 0;
		for (let i = 0; i < title.length; i++) {
			h = (h * 31 + title.charCodeAt(i)) % 360;
		}
		return h;
	}

	const hue = $derived(hueFor(item.title));
	const authorLine = $derived(item.authors.join(', '));
	const unitLabel = $derived(
		item.unitCount && item.unitType
			? `${item.unitCount.toLocaleString()} ${item.unitType}${item.unitCount === 1 ? '' : 's'}`
			: ''
	);
	const untitled = $derived(item.title === 'Untitled');

	/**
	 * How far this book has been read, as a percentage, or null.
	 *
	 * Below one percent reads as zero and is not worth a mark on the card:
	 * opening a book and closing it again is not progress.
	 */
	const percentRead = $derived(
		item.progress && item.progress >= 0.01 ? Math.round(item.progress * 100) : null
	);

	/**
	 * The title is shown ONCE per card.
	 *
	 * Without a cover the title is the artwork, so repeating it underneath said
	 * everything twice - the single most obvious flaw on the old shelf. With a
	 * real cover the title goes underneath, because a rendered first page is
	 * often a half-legible title page and never a caption.
	 */
	const titleBelow = $derived(Boolean(item.coverUrl));

	const borrowedTitle = $derived(
		item.canDownload
			? `${item.lentBy} shared this book with you`
			: `${item.lentBy} shared this book with you to read. It cannot be downloaded.`
	);
</script>

<a class="card" href="/works/{item.workId}" aria-label={item.title}>
	<div class="cover" style="--hue: {hue}">
		{#if item.coverUrl}
			<img src={item.coverUrl} alt="" loading="lazy" />
		{:else}
			<!-- Typographic fallback. The title is the artwork. -->
			<div class="placeholder" class:untitled>
				<span class="ph-title">{untitled ? 'No title yet' : item.title}</span>
				{#if authorLine}
					<span class="ph-author">{authorLine}</span>
				{/if}
			</div>
		{/if}

		{#if percentRead !== null}
			<!-- Along the foot of the cover, where a bookmark would sit. Not a
			     number: on a shelf of forty cards the shape is read, not the
			     value, and the value is on the book's own page. -->
			<div
				class="progress"
				style="--read: {percentRead}%"
				title="{percentRead}% read"
				aria-label="{percentRead} percent read"
			></div>
		{/if}

		{#if item.lentBy}
			<!-- Whose book this is, on the cover rather than in the metadata
			     line, because it changes what the card MEANS: a borrowed book
			     can be withdrawn by its owner and may not be downloadable, and
			     both are things to know before opening it rather than after. -->
			<span class="badge lent" title={borrowedTitle}>
				{item.canDownload ? item.lentBy : item.lentBy + ' · read only'}
			</span>
		{/if}

		{#if item.missing}
			<span class="badge missing" title="This file is not on disk right now">missing</span>
		{:else if item.status === 'needs_ocr'}
			<span class="badge" title="A scan with no text layer; it needs OCR before it can be searched">
				needs OCR
			</span>
		{:else if item.status === 'unsupported'}
			<span class="badge" title="Lexarii cannot read this format yet">unreadable</span>
		{:else if item.status === 'failed'}
			<span class="badge failed" title="This file could not be read">failed</span>
		{/if}
	</div>

	<div class="meta">
		{#if titleBelow}
			<span class="title" class:untitled>{untitled ? 'No title yet' : item.title}</span>
		{/if}
		<span class="author" class:lead={!titleBelow}>{authorLine || '—'}</span>
		<span class="detail">
			{#each item.formats as format (format)}
				<span class="format">{format}</span>
			{/each}
			{#if unitLabel}<span class="units">{unitLabel}</span>{/if}
		</span>
	</div>
</a>

<style>
	.card {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
		text-decoration: none;
		color: inherit;
		border-radius: var(--radius-md);
		/* The whole card is the target, and it is comfortably over 44px. */
		padding: var(--space-2);
		margin: calc(var(--space-2) * -1);
		transition: background var(--duration-fast) var(--ease-out);
	}

	.card:hover {
		background: var(--bg-sunken);
	}

	.cover {
		position: relative;
		aspect-ratio: var(--cover-aspect);
		border-radius: var(--radius-sm);
		overflow: hidden;
		background: hsl(var(--hue) 24% 92%);
		box-shadow: var(--shadow-book);
		/* Transform only: a grid of several hundred cards must not relayout. */
		transition: transform var(--duration-fast) var(--ease-out);
	}

	.card:hover .cover {
		transform: translateY(-2px);
	}

	.cover img {
		width: 100%;
		height: 100%;
		object-fit: cover;
	}

	.placeholder {
		height: 100%;
		display: flex;
		flex-direction: column;
		justify-content: space-between;
		padding: var(--space-4) var(--space-3);
		/* A hairline spine, so a coverless book still reads as a book. */
		border-left: 3px solid hsl(var(--hue) 30% 62%);
	}

	.ph-title {
		font-family: var(--font-serif);
		font-size: var(--text-base);
		line-height: 1.25;
		color: hsl(var(--hue) 35% 24%);
		/* Long technical titles are the norm; clamp rather than overflow. */
		display: -webkit-box;
		-webkit-line-clamp: 6;
		line-clamp: 6;
		-webkit-box-orient: vertical;
		overflow: hidden;
	}

	.ph-author {
		font-size: var(--text-xs);
		color: hsl(var(--hue) 20% 40%);
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}

	.placeholder.untitled .ph-title {
		font-style: italic;
		opacity: 0.6;
	}

	/* A bookmark along the foot of the cover.
	   It lies on top of arbitrary artwork - a real cover can be white paper or
	   a black photograph - so the contrast cannot come from the page palette.
	   A dark track under a light fill reads on both: on pale art the track
	   carries it, on dark art the fill does. */
	.progress {
		position: absolute;
		inset: auto 0 0 0;
		height: 4px;
		background: var(--overlay-track);
	}

	.progress::after {
		content: '';
		display: block;
		height: 100%;
		width: var(--read);
		background: var(--overlay-fill);
	}

	.badge {
		position: absolute;
		top: var(--space-2);
		right: var(--space-2);
		padding: 2px var(--space-2);
		border-radius: var(--radius-sm);
		background: var(--bg-raised);
		color: var(--fg-muted);
		font-size: var(--text-xs);
		box-shadow: var(--shadow-raised);
	}

	.badge.missing,
	.badge.failed {
		color: var(--danger);
	}

	/* The opposite corner from the status badges, which sit top-right: a
	   borrowed book can also be missing, and one badge on top of the other
	   reads as neither. Ownership on the left, condition on the right. */
	.badge.lent {
		right: auto;
		left: var(--space-2);
		max-width: calc(100% - var(--space-8));
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
		color: var(--accent);
	}

	.meta {
		display: flex;
		flex-direction: column;
		gap: 2px;
		min-width: 0;
	}

	.title {
		font-size: var(--text-sm);
		line-height: 1.3;
		color: var(--fg);
		display: -webkit-box;
		-webkit-line-clamp: 2;
		line-clamp: 2;
		-webkit-box-orient: vertical;
		overflow: hidden;
	}

	.title.untitled {
		font-style: italic;
		color: var(--fg-subtle);
	}

	.author,
	.detail {
		font-size: var(--text-xs);
		color: var(--fg-muted);
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}

	/* When the cover carries the title, the author is the first line below it
	   and is read as the card's own label rather than as a footnote. */
	.author.lead {
		font-size: var(--text-sm);
		color: var(--fg);
	}

	.detail {
		display: flex;
		flex-wrap: wrap;
		gap: 0 var(--space-2);
		align-items: baseline;
		color: var(--fg-subtle);
		/* A book held in three formats plus a unit count overflows a narrow
		   card. Wrapping is better than clipping: the formats are how a user
		   tells a readable copy from an unreadable one. */
		white-space: normal;
	}

	.format {
		text-transform: uppercase;
		letter-spacing: 0.04em;
		font-size: var(--text-xs);
	}
</style>
