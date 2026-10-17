<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<script lang="ts">
	import { fieldLabel } from '$lib/fields';
	import { page } from '$app/state';
	import { goto } from '$app/navigation';
	import * as api from '$api/client';
	import { ApiError } from '$api/client';
	import type { AccessRecord, Person, Recipient, Share, WorkDetail } from '$api/client';
	import { candidates, type Candidate } from '$lib/sharing/people';

	let detail = $state<WorkDetail | null>(null);
	let error = $state<string | null>(null);

	// Sharing is per FILE, not per work: someone can be given the EPUB and not
	// the 158MB PDF, and the two are different objects on disk.
	let sharingFile = $state<string | null>(null);

	// A book's own page is where somebody decides it belongs somewhere, so the
	// collections are loaded here rather than being a separate trip.
	let collections = $state<api.Collection[]>([]);

	// Correcting the book by hand.
	//
	// Opened deliberately rather than being an always-editable page: a book's
	// details are read far more often than they are changed, and a page of
	// input boxes reads as a form to fill in rather than as a book.
	let editing = $state(false);
	let saving = $state(false);
	let form = $state({
		title: '',
		subtitle: '',
		authors: '',
		series: '',
		seriesIndex: '',
		publisher: '',
		year: '',
		isbn13: '',
		language: '',
		description: ''
	});

	function startEditing() {
		if (!detail) return;
		const edition = detail.editions?.[0];
		form = {
			title: detail.title === 'Untitled' ? '' : detail.title,
			subtitle: detail.subtitle ?? '',
			// One line, comma separated. A repeatable row of inputs is the
			// "correct" control and is worse to use on a phone for the thing
			// people actually do, which is fixing one name.
			authors: detail.authors.map((a) => a.name).join(', '),
			series: detail.series ?? '',
			seriesIndex: detail.seriesIndex ? String(detail.seriesIndex) : '',
			publisher: edition?.publisher ?? '',
			year: edition?.publishedOn ? edition.publishedOn.slice(0, 4) : '',
			isbn13: edition?.isbn13 ?? '',
			language: detail.language ?? '',
			description: detail.description ?? ''
		};
		editing = true;
	}

	/**
	 * Builds the patch out of what actually CHANGED.
	 *
	 * The endpoint treats an absent field as "leave it" and an empty one as
	 * "clear it", so submitting every box would record every field as the
	 * user's - including the ones they never looked at, which would then be
	 * frozen against every future scan and lookup. Only what somebody touched
	 * becomes theirs.
	 */
	function patchOf(): api.UpdateWork {
		if (!detail) return {};
		const edition = detail.editions?.[0];
		const patch: api.UpdateWork = {};

		const was = {
			title: detail.title === 'Untitled' ? '' : detail.title,
			subtitle: detail.subtitle ?? '',
			series: detail.series ?? '',
			language: detail.language ?? '',
			description: detail.description ?? '',
			publisher: edition?.publisher ?? '',
			isbn13: edition?.isbn13 ?? ''
		};
		for (const key of Object.keys(was) as (keyof typeof was)[]) {
			const now = form[key].trim();
			if (now !== was[key]) patch[key] = now;
		}

		const authorsNow = form.authors
			.split(',')
			.map((name) => name.trim())
			.filter(Boolean);
		if (authorsNow.join(', ') !== detail.authors.map((a) => a.name).join(', ')) {
			patch.authors = authorsNow;
		}

		const yearWas = edition?.publishedOn ? edition.publishedOn.slice(0, 4) : '';
		if (form.year.trim() !== yearWas) {
			// Zero clears it, which is what an emptied box means.
			patch.year = form.year.trim() === '' ? 0 : Number(form.year.trim());
		}

		const indexWas = detail.seriesIndex ? String(detail.seriesIndex) : '';
		if (form.seriesIndex.trim() !== indexWas && form.seriesIndex.trim() !== '') {
			patch.seriesIndex = Number(form.seriesIndex.trim());
		}
		return patch;
	}

	async function saveEdit() {
		const id = page.params.id;
		if (!id) return;
		const patch = patchOf();
		if (Object.keys(patch).length === 0) {
			editing = false;
			return;
		}

		saving = true;
		try {
			detail = await api.editWork(id, patch);
			editing = false;
			error = null;
		} catch (err) {
			error = err instanceof Error ? err.message : String(err);
		} finally {
			saving = false;
		}
	}

	// Which other books in this library name this one. Loaded on its own,
	// after the page is already showing: most books have none, and a book's
	// page should not wait on a full-text search to render its title.
	let mentions = $state<api.Mention[]>([]);
	let holding = $state<Set<string>>(new Set());
	let filingInto = $state('');

	$effect(() => {
		void loadCollections();
	});

	$effect(() => {
		const id = page.params.id;
		if (!id) return;
		const abort = new AbortController();
		api
			.mentions(id, abort.signal)
			.then((list) => (mentions = list.items))
			// Silent: a backlink is a bonus, and an error banner about one
			// would sit above the book somebody came here to read.
			.catch(() => (mentions = []));
		return () => abort.abort();
	});

	/**
	 * Opens the mentioning book AT the mention rather than at its cover.
	 *
	 * The locator is the whole point: a backlink that lands on page one of a
	 * nine-hundred-page reference is a gesture, not a reference.
	 */
	function mentionHref(m: api.Mention): string {
		const pdf = m.locator.startsWith('pdf:');
		const at = m.locator ? `?at=${encodeURIComponent(m.locator)}` : '';
		return `/read/${m.bookFileId}${pdf ? '/pdf' : ''}${at}`;
	}

	async function loadCollections() {
		try {
			collections = (await api.collections()).items;
			// Which of them already hold this book, asked of each collection's
			// own listing rather than a fifth endpoint nobody else needs.
			const mine = new Set<string>();
			await Promise.all(
				collections.map(async (c) => {
					const full = await api.collection(c.id);
					if (full.items.some((item) => item.workId === page.params.id)) mine.add(c.id);
				})
			);
			holding = mine;
		} catch {
			// Collections are an extra. A book's page still shows the book.
			collections = [];
		}
	}

	async function file() {
		const id = page.params.id;
		if (!id || !filingInto) return;
		try {
			await api.addToCollection(filingInto, id);
			filingInto = '';
			await loadCollections();
		} catch (err) {
			error = err instanceof Error ? err.message : String(err);
		}
	}

	async function unfile(collectionId: string) {
		const id = page.params.id;
		if (!id) return;
		try {
			await api.removeFromCollection(collectionId, id);
			await loadCollections();
		} catch (err) {
			error = err instanceof Error ? err.message : String(err);
		}
	}

	// The three that the tools people actually use disagree about. Named by
	// the tool rather than the acronym, because "RIS" means nothing to
	// somebody who just wants this in Zotero.
	const citationFormats = [
		{ id: 'bibtex', label: 'BibTeX', tool: 'For LaTeX and BibLaTeX' },
		{ id: 'ris', label: 'RIS', tool: 'For Zotero, Mendeley and EndNote' },
		{ id: 'csl-json', label: 'CSL-JSON', tool: 'For Pandoc and most modern tools' }
	];
	let people = $state<Person[]>([]);
	let shares = $state<Share[]>([]);
	let shareError = $state<string | null>(null);
	let readLog = $state<AccessRecord[]>([]);
	let busy = $state<string | null>(null);

	// Whether the open panel belongs to this book's owner.
	//
	// Not asked directly: the share list is owner-only, so being refused IS the
	// answer. A borrower gets the half of the panel that is theirs - their own
	// notes - rather than an error about a list they were never going to see.
	let isOwner = $state(false);
	// Who this person shares their OWN marks on this file with.
	let noteShares = $state<string[]>([]);
	let noteSharePeople = $state<Recipient[]>([]);

	const shareFor = $derived(
		new Map(shares.map((s) => [s.userId, s]))
	);

	// Whether this person is shown everybody to lend to, or types a username:
	// an installation can keep its people unlisted (canListPeople on
	// /auth/me). Assumed listed until known, which is what every installation
	// did before the setting.
	let canListPeople = $state(true);
	// People named in the panel since it opened, kept so that unticking
	// somebody does not make their row vanish.
	let named = $state<Candidate[]>([]);
	let nameToAdd = $state('');
	// The panel could not be opened at all: only the error is shown.
	let panelFailed = $state(false);

	const pickable = $derived(
		candidates(
			canListPeople,
			people,
			shares.map((s) => ({ id: s.userId, username: s.username })),
			noteSharePeople,
			named
		)
	);

	/**
	 * Lends the book to - or, for somebody who does not hold it, shares their
	 * notes with - whoever has the username typed in.
	 */
	async function addByName(fileId: string, owner: boolean) {
		const name = nameToAdd.trim();
		if (!name) return;
		busy = 'by-name';
		shareError = null;
		try {
			let who: Candidate;
			if (owner) {
				const s = await api.shareByUsername(fileId, name, {
					canDownload: false,
					annotations: false
				});
				who = { id: s.userId, username: s.username };
				shares = (await api.shares(fileId)).items;
			} else {
				who = await api.shareNotesByUsername(fileId, name);
				const n = await api.noteShares(fileId);
				noteShares = n.items;
				noteSharePeople = n.people ?? [];
			}
			named = [...named.filter((p) => p.id !== who.id), who];
			nameToAdd = '';
		} catch (err) {
			shareError =
				err instanceof ApiError && err.code === 'user_not_found'
					? `Nobody here has the username “${name}”.`
					: err instanceof Error
						? err.message
						: String(err);
		} finally {
			busy = null;
		}
	}

	async function openSharing(fileId: string) {
		if (sharingFile === fileId) {
			sharingFile = null;
			return;
		}
		sharingFile = fileId;
		shareError = null;
		named = [];
		nameToAdd = '';
		panelFailed = false;
		try {
			// Everyone who can read the book may do these two: pick a person,
			// and say who sees their own notes.
			const [p, n] = await Promise.all([api.people(), api.noteShares(fileId)]);
			people = p.items;
			noteShares = n.items;
			noteSharePeople = n.people ?? [];
		} catch (err) {
			shareError = err instanceof Error ? err.message : String(err);
			people = [];
			noteShares = [];
			noteSharePeople = [];
			panelFailed = true;
			return;
		}

		// The owner's half. Being refused here is not an error to report: it is
		// how the server says "this is not your book", and a borrower opening
		// the panel wants their own notes, not a complaint about a list.
		try {
			const [s, a] = await Promise.all([
				api.shares(fileId),
				// Who has actually read it, which is a different question from
				// who was allowed to.
				api.accessLog(fileId).catch(() => ({ items: [] }))
			]);
			shares = s.items;
			readLog = a.items;
			isOwner = true;
		} catch {
			shares = [];
			readLog = [];
			isOwner = false;
		}
	}

	/** Shares this reader's own marks on the file, or stops. */
	async function setNoteShare(fileId: string, userId: string, on: boolean) {
		busy = userId;
		shareError = null;
		try {
			await api.setNoteShare(fileId, userId, on);
			const n = await api.noteShares(fileId);
			noteShares = n.items;
			noteSharePeople = n.people ?? [];
		} catch (err) {
			shareError = err instanceof Error ? err.message : String(err);
		} finally {
			busy = null;
		}
	}

	async function setAccess(
		fileId: string,
		userId: string,
		access: { canDownload: boolean; annotations: boolean } | null
	) {
		busy = userId;
		shareError = null;
		try {
			if (access) {
				await api.share(fileId, userId, access);
			} else {
				await api.unshare(fileId, userId);
			}
			shares = (await api.shares(fileId)).items;
			readLog = (await api.accessLog(fileId).catch(() => ({ items: [] }))).items;
		} catch (err) {
			shareError = err instanceof Error ? err.message : String(err);
		} finally {
			busy = null;
		}
	}

	$effect(() => {
		const id = page.params.id;
		if (!id) return;

		const controller = new AbortController();
		detail = null;
		error = null;

		api
			.work(id, controller.signal)
			.then((d) => (detail = d))
			.catch((err) => {
				if (err?.name !== 'AbortError') {
					error = err instanceof Error ? err.message : String(err);
				}
			});

		return () => controller.abort();
	});

	// Taking a book off your own shelf. A confirmation first: it is not undone
	// by a button, only by adding the same file again.
	let removeDialog = $state<HTMLDialogElement | null>(null);
	let removingFile = $state<string | null>(null);
	let removing = $state(false);
	let removeError = $state<string | null>(null);

	function askToRemove(fileId: string) {
		removingFile = fileId;
		removeError = null;
		removeDialog?.showModal();
	}

	async function confirmRemove() {
		const fileId = removingFile;
		const id = page.params.id;
		if (!fileId || !id) return;
		removing = true;
		removeError = null;
		try {
			await api.releaseHolding(fileId);
		} catch (err) {
			removeError = err instanceof Error ? err.message : String(err);
			removing = false;
			return;
		}
		removing = false;
		removeDialog?.close();
		try {
			// Still here through another copy, or lent by somebody: show what
			// is left.
			detail = await api.work(id);
		} catch {
			// Nothing of it is on this shelf any more.
			await goto('/');
		}
	}

	// Taking a file off the installation for everybody: an administrator's,
	// on any file of the book, whoever holds it. A reason is required - it is
	// kept on the blocklist, for whoever later wonders why - and the dialog
	// says plainly what goes and what stays.
	let isAdmin = $state(false);
	$effect(() => {
		api
			.me()
			.then((me) => {
				isAdmin = me.isAdmin;
				canListPeople = me.canListPeople ?? false;
			})
			.catch(() => {
				/* the reader's view, which is the safe default: the server refuses either way */
			});
	});

	let purgeDialog = $state<HTMLDialogElement | null>(null);
	let purgingFile = $state<string | null>(null);
	let purgeReason = $state('');
	let purgeQuotes = $state(false);
	let purging = $state(false);
	let purgeError = $state<string | null>(null);
	let purgeNote = $state<string | null>(null);

	function askToPurge(fileId: string) {
		purgingFile = fileId;
		purgeReason = '';
		purgeQuotes = false;
		purgeError = null;
		purgeDialog?.showModal();
	}

	async function confirmPurge() {
		const fileId = purgingFile;
		const id = page.params.id;
		const reason = purgeReason.trim();
		if (!fileId || !id || !reason) return;
		purging = true;
		purgeError = null;
		let removedFor = 0;
		try {
			removedFor = (await api.removeForEveryone(fileId, reason, purgeQuotes)).removedFor;
		} catch (err) {
			purgeError = err instanceof Error ? err.message : String(err);
			purging = false;
			return;
		}
		purging = false;
		purgeDialog?.close();
		purgeNote =
			removedFor === 1
				? 'Removed from 1 person’s shelf and blocked.'
				: `Removed from ${removedFor} people’s shelves and blocked.`;
		try {
			// Other files of the book may still be here.
			detail = await api.work(id);
		} catch {
			await goto('/blocked');
		}
	}

	function formatBytes(n: number): string {
		if (n < 1024) return `${n} B`;
		const units = ['KB', 'MB', 'GB'];
		let value = n / 1024;
		let unit = 0;
		while (value >= 1024 && unit < units.length - 1) {
			value /= 1024;
			unit++;
		}
		return `${value.toFixed(1)} ${units[unit]}`;
	}

	/** Explains a provenance source in the user's terms. */
	function sourceLabel(source: string): string {
		switch (source) {
			case 'embedded':
				return 'from the file';
			case 'user':
				return 'set by you';
			case 'llm':
				return 'suggested by your model';
			case 'filename':
				return 'guessed from the filename';
			case 'external':
				return 'from an online lookup';
			default:
				return source;
		}
	}

	function statusLabel(status: string): string {
		switch (status) {
			case 'ready':
				return 'readable';
			case 'needs_ocr':
				return 'needs OCR';
			case 'unsupported':
				return 'format not supported yet';
			case 'failed':
				return 'could not be read';
			default:
				return status;
		}
	}

	const untitled = $derived(detail?.title === 'Untitled');

	// Which file the Read button opens: the CHEAPEST that can render this book.
	//
	// EPUB first. It is reflowable text that adapts to the reader's
	// typography, and it is served as a manifest and one spine item at a time
	// - tens of kilobytes to turn a page. A PDF is a fixed layout that has to
	// be rendered page by page, and the largest in this library is 2.7GB; a
	// merged book that holds both should not open the expensive one because it
	// happened to be added first.
	//
	// A PDF is still perfectly readable and for most real libraries it is the
	// only option, so it is the fallback rather than a refusal.
	const readable = $derived(
		detail?.files.find((f) => f.format === 'epub' && f.status === 'ready' && !f.missing) ??
			detail?.files.find((f) => f.format === 'pdf' && f.status === 'ready' && !f.missing)
	);

	// Every file this person may actually have, for the download choice.
	//
	// A choice rather than one button: a book held as an EPUB and a PDF is two
	// different things to somebody with an e-reader, and the one this app
	// prefers to render is not always the one they want on their device.
	const downloadable = $derived(
		(detail?.files ?? []).filter((f) => f.canDownload && !f.missing)
	);
</script>

<svelte:head><title>{detail?.title ?? 'Book'} · Lexarii</title></svelte:head>

<article>
	<a class="back" href="/">← Library</a>

	{#if error}
		<p class="error" role="alert">{error}</p>
	{:else if !detail}
		<p class="loading">Loading…</p>
	{:else}
		<header>
			<h1 class:untitled>{untitled ? 'No title yet' : detail.title}</h1>
			{#if detail.subtitle}<p class="subtitle">{detail.subtitle}</p>{/if}

			{#if detail.authors.length > 0}
				<!--
					The separator is an expression, not literal whitespace: Svelte
					collapses whitespace between elements, which silently produced
					"Santos,Davis" for a two-author book.
				-->
				<p class="authors">
					{#each detail.authors as author, i (author.id)}
						<a href="/?search={encodeURIComponent(author.name)}">{author.name}</a>
						{#if author.role !== 'author'}<span class="role">({author.role})</span>{/if}
						{#if i < detail.authors.length - 1}{', '}{/if}
					{/each}
				</p>
			{/if}

			{#if detail.series}
				<p class="series">
					{detail.series}{#if detail.seriesIndex}, book {detail.seriesIndex}{/if}
				</p>
			{/if}
		</header>

		{#if detail.description || detail.summary}
			<p class="description">{detail.description || detail.summary}</p>
		{/if}

		{#if detail.tags && detail.tags.length > 0}
			<ul class="tags">
				{#each detail.tags as tag (tag.id)}
					<li>
						<a href="/?tag={encodeURIComponent(tag.slug)}" title={tag.source ? sourceLabel(tag.source) : ''}>
							{tag.name}
						</a>
					</li>
				{/each}
			</ul>
		{/if}

		<div class="open">
			{#if readable}
				<a class="read" href="/read/{readable.id}">
					Read{readable.format === 'pdf' ? ' (PDF)' : ''}
				</a>
			{/if}

			<!--
				One link per format, because the format this app prefers to
				render is not always the one somebody wants on their device.
				The size is on each: the difference between a 3MB EPUB and a
				2.7GB PDF decides the answer on a phone.
			-->
			{#each downloadable as file (file.id)}
				<a
					class="download"
					href="/api/v1/books/{file.id}/file"
					download
					title="Download the {file.format.toUpperCase()}, {formatBytes(file.sizeBytes)}"
				>
					{file.format.toUpperCase()}
					<span class="size">{formatBytes(file.sizeBytes)}</span>
				</a>
			{/each}
		</div>

		<!--
			Correcting what the file got wrong.
			
			Most books arrive described by whatever was in the file, and in a
			real library that is often nothing: 70 of 168 books here had no
			author at all, and three had a title taken off the wrong line of a
			title page. What is corrected here is recorded as the reader's own,
			which is what stops the next scan putting the wrong thing back.
		-->
		<section class="details">
			<h2>
				Book details
				{#if !editing}
					<button class="link" onclick={startEditing}>Correct these</button>
				{/if}
			</h2>

			{#if !editing}
				{@const edition = detail.editions?.[0]}
				<dl class="facts">
					{#if edition?.publisher}
						<div><dt>Publisher</dt><dd>{edition.publisher}</dd></div>
					{/if}
					{#if edition?.publishedOn}
						<div><dt>Published</dt><dd>{edition.publishedOn.slice(0, 4)}</dd></div>
					{/if}
					{#if edition?.isbn13 || edition?.isbn10}
						<div><dt>ISBN</dt><dd>{edition.isbn13 || edition.isbn10}</dd></div>
					{/if}
					{#if detail.language}
						<div><dt>Language</dt><dd>{detail.language}</dd></div>
					{/if}
				</dl>
				{#if !edition?.publisher && !edition?.publishedOn && !edition?.isbn13 && !edition?.isbn10}
					<!--
						Said plainly rather than shown as an empty list. Most books in a
						real library arrive with none of this, and a citation exported
						from one comes out as a title and an author.
					-->
					<p class="hint">
						Nothing here came with a publisher, a year or an ISBN. A citation
						exported from this book will be missing them.
					</p>
				{/if}
			{/if}

			{#if editing}
				<form
					onsubmit={(e) => {
						e.preventDefault();
						void saveEdit();
					}}
				>
					<label>
						Title
						<input bind:value={form.title} required />
					</label>
					<label>
						Subtitle
						<input bind:value={form.subtitle} />
					</label>
					<label>
						Authors
						<input bind:value={form.authors} placeholder="Separated by commas" />
					</label>
					<div class="pair">
						<label>
							Series
							<input bind:value={form.series} />
						</label>
						<label class="narrow">
							Number
							<input bind:value={form.seriesIndex} inputmode="decimal" />
						</label>
					</div>
					<div class="pair">
						<label>
							Publisher
							<input bind:value={form.publisher} />
						</label>
						<label class="narrow">
							Year
							<input bind:value={form.year} inputmode="numeric" placeholder="1985" />
						</label>
					</div>
					<div class="pair">
						<label>
							ISBN
							<input bind:value={form.isbn13} inputmode="numeric" />
						</label>
						<label class="narrow">
							Language
							<input bind:value={form.language} placeholder="en" />
						</label>
					</div>
					<label>
						Description
						<textarea bind:value={form.description} rows="4"></textarea>
					</label>

					<p class="hint">
						An emptied box clears that field. Anything you change here is kept as yours
						and is not overwritten by a later scan or lookup.
					</p>

					<div class="actions">
						<button class="button" type="submit" disabled={saving}>
							{saving ? 'Saving…' : 'Save'}
						</button>
						<button class="button quiet" type="button" onclick={() => (editing = false)}>
							Cancel
						</button>
					</div>
				</form>
			{/if}
		</section>

		<!--
			Filing a book is a two-step gesture on purpose: pick the collection,
			then add. A one-click "add to my last collection" would be faster
			and would put books in the wrong place.
		-->
		<section class="filing">
			<h2>Collections</h2>
			{#if collections.length === 0}
				<p class="hint">
					No collections yet. <a class="inline-link" href="/collections">Make one</a> and this book
					can go in it.
				</p>
			{:else}
				<p class="in">
					{#each collections as c (c.id)}
						{#if holding.has(c.id)}
							<span class="chip">
								<a href="/collections/{c.id}">{c.name}</a>
								<button title="Take it out of {c.name}" onclick={() => void unfile(c.id)}>×</button>
							</span>
						{/if}
					{/each}
				</p>
				<div class="file">
					<label class="sr-only" for="collection-picker">Add to a collection</label>
					<select id="collection-picker" bind:value={filingInto}>
						<option value="">Choose a collection…</option>
						{#each collections as c (c.id)}
							{#if !holding.has(c.id)}<option value={c.id}>{c.name}</option>{/if}
						{/each}
					</select>
					<button class="button" disabled={!filingInto} onclick={() => void file()}>Add</button>
				</div>
			{/if}
		</section>

		<!--
			The citation is a link rather than a fetch-and-copy button: the
			server renders it, so the browser can show it, and a middle click
			opens it in a tab like any other address. Copy takes one more
			click than a clipboard button would and works when the clipboard
			API does not, which on a page served over plain HTTP is always.
		-->
		<section class="cite">
			<h2>Cite this</h2>
			<p class="formats">
				{#each citationFormats as format (format.id)}
					<a href="/api/v1/works/{detail.id}/citation?format={format.id}" title={format.tool}>
						{format.label}
					</a>
				{/each}
			</p>
		</section>

		<!--
			Found, not declared. Nobody typed these links: they are this book's
			title and author turning up in another book's bibliography, index
			or further-reading list, which is what a backlink between books
			actually is.
		-->
		{#if mentions.length > 0}
			<section class="mentions">
				<h2>Mentioned in</h2>
				<ul>
					{#each mentions as m (m.workId + m.bookFileId)}
						<li>
							<a class="where" href={mentionHref(m)}>{m.title}</a>
							{#if m.authors?.length}<span class="by">{m.authors.join(', ')}</span>{/if}
							{#if m.places > 1}<span class="places">{m.places} places</span>{/if}
							<p class="excerpt">{m.excerpt}</p>
							<a class="about" href="/works/{m.workId}">About that book</a>
						</li>
					{/each}
				</ul>
			</section>
		{/if}

		<section>
			<h2>Files</h2>
			{#if purgeNote}<p class="purge-note" role="status">{purgeNote}</p>{/if}
			<ul class="files">
				{#each detail.files as file (file.id)}
					<li class:unreadable={file.status !== 'ready'}>
						<span class="format">{file.format}</span>
						<span class="status">{statusLabel(file.status)}</span>
						<span class="size">{formatBytes(file.sizeBytes)}</span>
						{#if file.unitCount && file.unitType}
							<span class="units">
								{file.unitCount.toLocaleString()}
								{file.unitType}{file.unitCount === 1 ? '' : 's'}
							</span>
						{/if}
						{#if file.filename}
							<span class="path" title={file.filename}>{file.filename}</span>
						{:else if file.path}
							<span class="path" title={file.path}>{file.path}</span>
						{/if}
						{#if file.ingestError}<span class="file-error">{file.ingestError}</span>{/if}

						<button class="share-toggle" onclick={() => void openSharing(file.id)}>
							{sharingFile === file.id ? 'Done' : 'Share…'}
						</button>
						{#if file.held}
							<button class="remove-holding" onclick={() => askToRemove(file.id)}>
								Remove from my shelf
							</button>
						{/if}
						{#if isAdmin}
							<button class="remove-holding" onclick={() => askToPurge(file.id)}>
								Remove for everyone…
							</button>
						{/if}

						{#if sharingFile === file.id}
							<div class="sharing">
								{#if shareError}
									<p class="share-error" role="alert">{shareError}</p>
								{/if}
								{#if panelFailed}
									<!-- Nothing to offer: the error above says why. -->
								{:else}
								{#if !canListPeople}
									<!-- This installation does not list its people: the
									     recipient is named by their exact username. -->
									<form
										class="by-name"
										onsubmit={(e) => {
											e.preventDefault();
											void addByName(file.id, isOwner);
										}}
									>
										<label for="share-name-{file.id}">
											{isOwner ? 'Lend to' : 'Share my notes with'}
										</label>
										<input
											id="share-name-{file.id}"
											type="text"
											bind:value={nameToAdd}
											placeholder="their username"
											autocomplete="off"
											autocapitalize="none"
											spellcheck="false"
										/>
										<button type="submit" disabled={!nameToAdd.trim() || busy === 'by-name'}>
											{busy === 'by-name' ? 'Adding…' : 'Add'}
										</button>
									</form>
								{/if}
								{#if canListPeople && people.length === 0}
									<p class="share-none">
										Nobody else has an account here yet. Sharing needs someone to
										share with.
									</p>
								{:else if isOwner}
									<p class="share-note">
										Shared books open in the reader. Allowing a download hands over
										the file itself, which cannot be taken back.
									</p>
									<p class="share-note">
										Sharing is per FILE. Granting this one grants this one: a book
										held as both an EPUB and a PDF has two of these panels, and
										they are separate decisions.
									</p>
									<ul class="people">
										{#each pickable as person (person.id)}
											{@const current = shareFor.get(person.id)}
											<li>
												<span class="who">{person.username}</span>

												<label>
													<input
														type="checkbox"
														checked={!!current}
														disabled={busy === person.id}
														onchange={(e) =>
															void setAccess(
																file.id,
																person.id,
																e.currentTarget.checked
																	? { canDownload: false, annotations: false }
																	: null
															)}
													/>
													can read
												</label>

												<label class:disabled={!current}>
													<input
														type="checkbox"
														checked={current?.canDownload ?? false}
														disabled={!current || busy === person.id}
														onchange={(e) =>
															void setAccess(file.id, person.id, {
																canDownload: e.currentTarget.checked,
																annotations: current?.annotations ?? false
															})}
													/>
													can download
												</label>

												<label class:disabled={!current}>
													<input
														type="checkbox"
														checked={current?.annotations ?? false}
														disabled={!current || busy === person.id}
														onchange={(e) =>
															void setAccess(file.id, person.id, {
																canDownload: current?.canDownload ?? false,
																annotations: e.currentTarget.checked
															})}
													/>
													my notes
												</label>
											</li>
										{/each}
									</ul>

									{#if readLog.length > 0}
										<h3 class="read-heading">Who has read it</h3>
										<p class="share-note">
											Your own reading is never recorded. Everything here is
											somebody else.
										</p>
										<ul class="readlog">
											{#each readLog as entry (entry.userId + entry.day + entry.kind)}
												<li>
													<span class="who">{entry.username}</span>
													<span class="how-much">
														{#if entry.kind === 'file'}
															downloaded the file
														{:else if entry.of > 0}
															{entry.units} of {entry.of}
															{entry.kind === 'page' ? 'pages' : 'chapters'}
														{:else}
															{entry.units}
															{entry.kind === 'page' ? 'pages' : 'chapters'}
														{/if}
													</span>
													<span class="when">{entry.day}</span>
												</li>
											{/each}
										</ul>
									{/if}
								{:else}
									<!-- Not this person's book to lend. Their own notes on it are
									     theirs, though, and the store has always keyed note
									     sharing on whoever WROTE the marks - nothing could reach
									     it except the owner's panel, so a reader could be lent a
									     book and had no way to answer in it. -->
									<p class="share-note">
										This book is not yours to lend. Your own notes on it are:
										anyone you pick here sees your highlights when they open it,
										and nothing else changes.
									</p>
									<ul class="people">
										{#each pickable as person (person.id)}
											<li>
												<span class="who">{person.username}</span>
												<label>
													<input
														type="checkbox"
														checked={noteShares.includes(person.id)}
														disabled={busy === person.id}
														onchange={(e) =>
															void setNoteShare(
																file.id,
																person.id,
																e.currentTarget.checked
															)}
													/>
													sees my notes
												</label>
											</li>
										{/each}
									</ul>
								{/if}
								{/if}
							</div>
						{/if}
					</li>
				{/each}
			</ul>
		</section>

		{#if detail.provenance && Object.keys(detail.provenance).length > 0}
			<section>
				<h2>Where this came from</h2>
				<!--
					Provenance is shown, not hidden in a debug view. It is what lets a
					user tell a publisher's fact from a model's guess, and reject the
					guess. Nothing here should ever be presented as simply true.
				-->
				<dl class="provenance">
					{#each Object.entries(detail.provenance) as [field, source] (field)}
						<div>
							<dt>{fieldLabel(field)}</dt>
							<dd class:guess={source === 'llm' || source === 'filename'}>{sourceLabel(source)}</dd>
						</div>
					{/each}
				</dl>
			</section>
		{/if}

		{#if detail.warnings && detail.warnings.length > 0}
			<section>
				<h2>Notes from reading this file</h2>
				<ul class="warnings">
					{#each detail.warnings as warning, i (i)}
						<li>{warning}</li>
					{/each}
				</ul>
			</section>
		{/if}
	{/if}
</article>

<!--
	A native modal dialog: it traps focus, closes on Escape and hands focus back
	to the button that opened it, which is the whole accessibility contract for
	a confirmation and costs no JavaScript of ours.
-->
<dialog
	class="confirm-remove"
	bind:this={removeDialog}
	aria-labelledby="remove-title"
	aria-describedby="remove-body"
	onclose={() => (removingFile = null)}
>
	<h2 id="remove-title">Remove this book from your shelf?</h2>
	<div id="remove-body">
		<p>
			It stops being yours: it leaves your shelf, and anyone you lent it to loses the access
			you gave them.
		</p>
		<p>
			Your highlights, notes and reading position are kept. Add the same file again and they
			are all still there.
		</p>
	</div>
	{#if removeError}
		<p class="share-error" role="alert">{removeError}</p>
	{/if}
	<div class="dialog-actions">
		<button class="button quiet" type="button" onclick={() => removeDialog?.close()}>
			Keep it
		</button>
		<button
			class="button danger"
			type="button"
			disabled={removing}
			onclick={() => void confirmRemove()}
		>
			{removing ? 'Removing…' : 'Remove from my shelf'}
		</button>
	</div>
</dialog>

<dialog
	class="confirm-remove"
	bind:this={purgeDialog}
	aria-labelledby="purge-title"
	aria-describedby="purge-body"
	onclose={() => (purgingFile = null)}
>
	<h2 id="purge-title">Remove this file for everyone?</h2>
	<div id="purge-body">
		<p>
			It leaves every shelf in this library, whoever added it, and all lending of it ends. Its
			search index and cover are deleted, and so is every copy Lexarii stored. A file in the
			library folder stays where it is, but is no longer read.
		</p>
		<p>
			Uploading or scanning it again is refused until the block is lifted. People keep their
			own highlights, notes and reading positions on it - and with them the words their
			highlights quote, the passages their chat citations quote, and whatever the assistant
			quoted in its answers, unless you erase them below.
		</p>
	</div>
	<form
		onsubmit={(e) => {
			e.preventDefault();
			void confirmPurge();
		}}
	>
		<label class="reason">
			<span>Reason, kept with the block</span>
			<textarea bind:value={purgeReason} required maxlength="1000" rows="3"></textarea>
		</label>
		<label class="purge-quotes">
			<input type="checkbox" bind:checked={purgeQuotes} />
			<span>
				Also erase quoted passages: highlight quotes and chat citation excerpts from this file.
				The highlights, notes and citations stay; answers are not rewritten.
			</span>
		</label>
		{#if purgeError}
			<p class="share-error" role="alert">{purgeError}</p>
		{/if}
		<div class="dialog-actions">
			<button class="button quiet" type="button" onclick={() => purgeDialog?.close()}>
				Keep it
			</button>
			<button class="button danger" type="submit" disabled={purging || !purgeReason.trim()}>
				{purging ? 'Removing…' : 'Remove for everyone'}
			</button>
		</div>
	</form>
</dialog>

<style>
	.purge-note {
		margin: 0 0 var(--space-3);
		color: var(--fg-muted);
		font-size: var(--text-sm);
	}

	.reason {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
		font-size: var(--text-sm);
	}

	.purge-quotes {
		display: flex;
		align-items: flex-start;
		min-height: 44px;
		gap: var(--space-2);
		margin-top: var(--space-3);
		font-size: var(--text-sm);
	}

	.purge-quotes input {
		width: 1.25rem;
		height: 1.25rem;
		margin: 0.1rem 0 0;
		flex: none;
	}

	.reason textarea {
		padding: var(--space-2);
		border: 1px solid var(--border);
		border-radius: var(--radius-md);
		background: var(--bg);
		color: var(--fg);
		font: inherit;
		resize: vertical;
	}

	/* Its own control rather than a share-toggle: a destructive action gets a
	   full-size touch target, and the colour says what kind it is. */
	.remove-holding {
		align-self: start;
		min-height: 44px;
		padding: 0 var(--space-3);
		border: 1px solid color-mix(in srgb, var(--danger) 40%, var(--border));
		border-radius: var(--radius-md);
		background: none;
		color: var(--danger);
		font: inherit;
		font-size: var(--text-sm);
		cursor: pointer;
	}

	.remove-holding:hover {
		border-color: var(--danger);
	}

	.confirm-remove {
		width: min(28rem, calc(100vw - var(--space-8)));
		padding: var(--space-6);
		border: 1px solid var(--border);
		border-radius: var(--radius-md);
		background: var(--bg-raised);
		color: var(--fg);
	}

	.confirm-remove::backdrop {
		background: var(--backdrop);
	}

	.confirm-remove h2 {
		margin-top: 0;
		font-size: var(--text-lg);
	}

	.dialog-actions {
		display: flex;
		flex-wrap: wrap;
		justify-content: flex-end;
		gap: var(--space-3);
		margin-top: var(--space-6);
	}

	.dialog-actions .button {
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

	.dialog-actions .button:disabled {
		opacity: 0.6;
		cursor: default;
	}

	.dialog-actions .danger {
		color: var(--danger);
		border-color: color-mix(in srgb, var(--danger) 40%, var(--border));
	}

	article {
		max-width: 46rem;
		margin-inline: auto;
		padding: var(--space-6) var(--space-4) var(--space-16);
	}

	.back {
		display: inline-flex;
		align-items: center;
		min-height: 44px;
		color: var(--fg-muted);
		text-decoration: none;
		font-size: var(--text-sm);
	}

	.back:hover {
		color: var(--fg);
	}

	h1 {
		font-family: var(--font-serif);
		font-size: var(--text-3xl);
		line-height: var(--leading-tight);
		margin: var(--space-4) 0 var(--space-2);
	}

	h1.untitled {
		font-style: italic;
		color: var(--fg-subtle);
	}

	h2 {
		font-size: var(--text-sm);
		text-transform: uppercase;
		letter-spacing: 0.06em;
		color: var(--fg-subtle);
		margin: var(--space-8) 0 var(--space-3);
	}

	.sr-only {
		position: absolute;
		width: 1px;
		height: 1px;
		margin: -1px;
		overflow: hidden;
		clip-path: inset(50%);
		white-space: nowrap;
	}

	.filing .hint {
		margin: 0 0 var(--space-3);
		color: var(--fg-muted);
		font-size: var(--text-sm);
	}

	/* A link inside a sentence: the accent, underlined, so it reads as one
	   without the browser's default blue. */
	.inline-link {
		color: var(--accent);
		text-decoration: underline;
		text-underline-offset: 2px;
	}

	.inline-link:hover {
		color: var(--fg);
	}

	.filing .in {
		display: flex;
		flex-wrap: wrap;
		gap: var(--space-2);
		margin: 0 0 var(--space-3);
	}

	.chip {
		display: inline-flex;
		align-items: center;
		gap: var(--space-1);
		padding: var(--space-1) var(--space-2);
		border: 1px solid var(--border);
		border-radius: 999px;
		font-size: var(--text-sm);
	}

	.chip a {
		color: var(--fg);
		text-decoration: none;
	}

	.chip button {
		min-width: 24px;
		min-height: 24px;
		border: 0;
		border-radius: 999px;
		background: none;
		color: var(--fg-muted);
		font: inherit;
		cursor: pointer;
	}

	.chip button:hover {
		color: var(--danger);
	}

	.file {
		display: flex;
		flex-wrap: wrap;
		gap: var(--space-2);
	}

	.file select {
		min-height: 44px;
		padding: 0 var(--space-2);
		border: 1px solid var(--border);
		border-radius: var(--radius-md);
		background: var(--bg-raised);
		color: var(--fg);
		font: inherit;
		font-size: var(--text-sm);
	}

	.file .button {
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

	.file .button:disabled {
		opacity: 0.5;
		cursor: default;
	}

	.cite .formats {
		display: flex;
		flex-wrap: wrap;
		gap: var(--space-2);
		margin: 0;
	}

	.cite .formats a {
		display: inline-flex;
		align-items: center;
		min-height: 44px;
		padding: 0 var(--space-3);
		border: 1px solid var(--border);
		border-radius: var(--radius-md);
		color: var(--fg);
		font-size: var(--text-sm);
		text-decoration: none;
	}

	.cite .formats a:hover {
		background: var(--bg-sunken);
	}

	.details h2 {
		display: flex;
		align-items: baseline;
		justify-content: space-between;
		gap: var(--space-3);
		flex-wrap: wrap;
	}

	.link {
		background: none;
		border: none;
		padding: var(--space-2) 0;
		min-height: 44px;
		color: var(--accent);
		font-size: var(--text-sm);
		font-family: inherit;
		cursor: pointer;
		text-decoration: underline;
	}

	.facts {
		display: grid;
		gap: var(--space-2);
		margin: 0 0 var(--space-3);
		font-size: var(--text-sm);
	}

	.facts div {
		display: flex;
		gap: var(--space-2);
	}

	.facts dt {
		color: var(--fg-muted);
		min-width: 6rem;
	}

	.facts dd {
		margin: 0;
	}

	.details form {
		display: grid;
		gap: var(--space-3);
	}

	.details label {
		display: grid;
		gap: var(--space-1);
		font-size: var(--text-sm);
		color: var(--fg-muted);
	}

	.details input,
	.details textarea {
		min-height: 44px;
		padding: var(--space-2) var(--space-3);
		border: 1px solid var(--border);
		border-radius: var(--radius-md);
		background: var(--bg);
		color: var(--fg);
		font: inherit;
		/* Without this a long ISBN or title pushes the form wider than the
		   phone it is being typed on. */
		min-width: 0;
		width: 100%;
		box-sizing: border-box;
	}

	.details textarea {
		font-family: var(--font-serif);
		line-height: 1.5;
		resize: vertical;
	}

	.pair {
		display: grid;
		/* Two boxes side by side where there is room, stacked where there is
		   not, which at 380px there is not. */
		grid-template-columns: 1fr;
		gap: var(--space-3);
	}

	@media (min-width: 30rem) {
		.pair {
			grid-template-columns: 2fr 1fr;
		}
	}

	.details .actions {
		display: flex;
		gap: var(--space-3);
		flex-wrap: wrap;
	}

	.details .quiet {
		background: none;
		color: var(--fg-muted);
		border: 1px solid var(--border);
	}

	.open {
		display: flex;
		flex-wrap: wrap;
		align-items: center;
		gap: var(--space-2);
		margin-bottom: var(--space-6);
	}

	.download {
		display: inline-flex;
		align-items: center;
		gap: var(--space-2);
		min-height: 44px;
		padding: 0 var(--space-3);
		border: 1px solid var(--border);
		border-radius: var(--radius-md);
		color: var(--fg-muted);
		font-size: var(--text-sm);
		text-decoration: none;
	}

	.download:hover {
		background: var(--bg-sunken);
		color: var(--fg);
	}

	.download .size {
		color: var(--fg-muted);
		font-variant-numeric: tabular-nums;
	}

	.mentions ul {
		list-style: none;
		margin: 0;
		padding: 0;
		display: grid;
		gap: var(--space-4);
	}

	.mentions li {
		/* A quotation from somebody else's book, marked as one. */
		border-left: 2px solid var(--border);
		padding-left: var(--space-3);
	}

	.mentions .where {
		font-weight: 600;
		color: var(--fg);
		text-decoration: none;
	}

	.mentions .where:hover {
		text-decoration: underline;
	}

	.mentions .by,
	.mentions .places {
		color: var(--fg-muted);
		font-size: var(--text-sm);
		margin-left: var(--space-2);
	}

	.mentions .excerpt {
		font-family: var(--font-serif);
		color: var(--fg-muted);
		margin: var(--space-2) 0;
		line-height: 1.6;
	}

	.mentions .about {
		font-size: var(--text-sm);
		color: var(--fg-muted);
	}

	.subtitle {
		font-family: var(--font-serif);
		font-size: var(--text-lg);
		color: var(--fg-muted);
		margin: 0 0 var(--space-3);
	}

	.authors,
	.series {
		margin: 0 0 var(--space-2);
		color: var(--fg-muted);
	}

	.authors {
		/* The each-block puts the separator on its own line in the markup;
		   without this the comma would drift away from the name. */
		word-spacing: normal;
	}

	.authors a {
		color: var(--accent);
		text-decoration: none;
	}

	.authors a:hover {
		text-decoration: underline;
	}

	.role {
		color: var(--fg-subtle);
		font-size: var(--text-sm);
	}

	.read {
		display: inline-flex;
		align-items: center;
		min-height: 44px;
		padding: 0 var(--space-6);
		margin-top: var(--space-4);
		border-radius: var(--radius-md);
		background: var(--accent);
		color: var(--accent-fg);
		text-decoration: none;
		font-size: var(--text-sm);
	}

	.description {
		font-family: var(--font-serif);
		line-height: var(--leading-reading);
		max-width: var(--measure);
		margin: var(--space-6) 0;
	}

	.share-toggle {
		align-self: start;
		padding: 0.15rem 0.5rem;
		border: 1px solid var(--border);
		border-radius: 0.3rem;
		background: none;
		color: var(--fg-muted);
		font: inherit;
		font-size: 0.85rem;
		cursor: pointer;
	}

	.share-toggle:hover {
		color: var(--fg);
		border-color: var(--fg-muted);
	}

	.sharing {
		grid-column: 1 / -1;
		margin-top: 0.5rem;
		padding: 0.75rem;
		border: 1px solid var(--border);
		border-radius: 0.4rem;
	}

	.share-note,
	.share-none,
	.share-error {
		margin: 0 0 0.6rem;
		font-size: 0.85rem;
		color: var(--fg-muted);
	}

	.share-error {
		color: var(--danger, #b3261e);
	}

	.people {
		list-style: none;
		margin: 0;
		padding: 0;
		display: flex;
		flex-direction: column;
		gap: 0.4rem;
	}

	.people li {
		display: flex;
		flex-wrap: wrap;
		align-items: center;
		gap: 0.75rem;
	}

	.who {
		min-width: 8rem;
		font-weight: 500;
	}

	.people label {
		display: flex;
		align-items: center;
		gap: 0.3rem;
		font-size: 0.85rem;
		/* 44px targets on touch: a checkbox is small and these change who can
		   see a book. */
		min-height: 44px;
	}

	/* A permission that depends on access being granted first reads as
	   unavailable rather than merely unchecked. */
	.people label.disabled {
		opacity: 0.45;
	}

	.by-name {
		display: flex;
		flex-wrap: wrap;
		align-items: center;
		gap: var(--space-2);
		margin: 0 0 var(--space-3);
		font-size: var(--text-sm);
	}

	.by-name input {
		flex: 1 1 10rem;
		min-height: 44px;
		padding: 0 var(--space-3);
		border: 1px solid var(--border);
		border-radius: var(--radius-md);
		background: var(--bg-raised);
		color: var(--fg);
		font: inherit;
	}

	.by-name button {
		padding: 0 var(--space-4);
		border: 1px solid var(--border);
		border-radius: var(--radius-md);
		background: var(--bg-raised);
		color: var(--fg);
		cursor: pointer;
	}

	.by-name button[disabled] {
		opacity: 0.6;
		cursor: default;
	}

	.read-heading {
		margin: 1rem 0 0.25rem;
		font-size: 0.9rem;
	}

	.readlog {
		list-style: none;
		margin: 0;
		padding: 0;
		display: flex;
		flex-direction: column;
		gap: 0.2rem;
		font-size: 0.85rem;
	}

	.readlog li {
		display: flex;
		flex-wrap: wrap;
		gap: 0.75rem;
	}

	.how-much {
		color: var(--fg-muted);
	}

	.when {
		color: var(--fg-muted);
		font-variant-numeric: tabular-nums;
	}

	.tags {
		display: flex;
		flex-wrap: wrap;
		gap: var(--space-2);
		list-style: none;
		padding: 0;
		margin: var(--space-4) 0;
	}

	.tags a {
		display: inline-flex;
		align-items: center;
		padding: var(--space-1) var(--space-3);
		border: 1px solid var(--border);
		border-radius: var(--radius-lg);
		font-size: var(--text-sm);
		color: var(--fg-muted);
		text-decoration: none;
	}

	.files {
		list-style: none;
		padding: 0;
		margin: 0;
	}

	.files li {
		display: flex;
		flex-wrap: wrap;
		gap: var(--space-3);
		align-items: baseline;
		padding: var(--space-3) 0;
		border-bottom: 1px solid var(--border);
		font-size: var(--text-sm);
	}

	.files li.unreadable {
		color: var(--fg-subtle);
	}

	.format {
		text-transform: uppercase;
		letter-spacing: 0.04em;
		font-weight: 600;
		min-width: 3.5rem;
	}

	.path {
		flex: 1 1 100%;
		font-family: var(--font-mono);
		font-size: var(--text-xs);
		color: var(--fg-subtle);
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}

	.file-error {
		flex: 1 1 100%;
		color: var(--danger);
		font-size: var(--text-xs);
	}

	.provenance {
		margin: 0;
		display: grid;
		grid-template-columns: max-content 1fr;
		gap: var(--space-2) var(--space-4);
		font-size: var(--text-sm);
	}

	.provenance div {
		display: contents;
	}

	dt {
		color: var(--fg-muted);
	}

	dd {
		margin: 0;
		color: var(--fg);
	}

	dd.guess {
		color: var(--accent);
		font-style: italic;
	}

	.warnings {
		margin: 0;
		padding-left: var(--space-4);
		font-size: var(--text-sm);
		color: var(--fg-muted);
		max-width: var(--measure);
	}

	.warnings li {
		margin-bottom: var(--space-2);
	}

	.error {
		color: var(--danger);
	}

	.loading {
		color: var(--fg-muted);
	}
</style>
