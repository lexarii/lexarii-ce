<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<script lang="ts">
	import BookCard from '$components/BookCard.svelte';
	import { offersAsk } from '$lib/auth/capabilities';
	import * as api from '$api/client';
	import { renderMissingCovers } from '$lib/covers';
	import type {
		ContinueItem,
		Job,
		LibraryItem,
		LibraryQuery,
		ShareActivity,
		Stats,
		UploadResult
	} from '$api/client';

	let items = $state<LibraryItem[]>([]);
	let cursor = $state<string | undefined>(undefined);
	let loading = $state(false);
	let loadError = $state<string | null>(null);
	let stats = $state<Stats | null>(null);

	let search = $state('');
	let sort = $state<NonNullable<LibraryQuery['sort']>>('title');
	let includeMissing = $state(false);

	// Whose books to show.
	//
	// Borrowed books sit on the same shelf as your own, because that is where
	// you want them when you are looking for something to read. This is for the
	// other question - "what did somebody lend me" - which the shelf could not
	// answer at all: a grantee saw borrowed books mixed in with no sign of whose
	// they were.
	let owned = $state<'all' | 'mine' | 'shared'>('all');

	// Reading of the books this person has LENT, when any of it is worth
	// interrupting them for.
	//
	// The audit log has always recorded faithfully and told nobody: an owner had
	// to open a book's share panel to discover that somebody pulled every page
	// of it last night. This is the difference between a record and a warning.
	let shareAlerts = $state<ShareActivity[]>([]);

	// Whether anything is shared with this person at all.
	//
	// The filter is hidden until it would do something. Most installations have
	// one person on them, and offering "Mine / Shared with me" to somebody who
	// has never been lent a book is three controls where none are needed.
	let hasShared = $state(false);

	// Whether this reader may change the shared library. Books, tags and
	// metadata are shared between everyone with an account, so adding and
	// re-scanning them is an administrator's job; reading, annotating and
	// asking are not. The server enforces this - the controls are hidden so
	// nobody is offered a button that will refuse them.
	let isAdmin = $state(false);
	// Whether this reader may add books by uploading them. Administrators
	// always may; everybody else when the installation's upload policy says
	// so, and then only to their own shelf.
	let canUpload = $state(false);
	// Asking needs a chat model; with none configured the link is not offered.
	let canAsk = $state(false);

	// Books that arrived twice under two filenames. Counted here so the shelf
	// can mention it; the deciding happens on its own page.
	let duplicateCount = $state(0);

	// Books started and not finished. Shown above the shelf because it is what
	// a reader opens the app to find, and because 168 covers is a lot to scan
	// for the one you put down last night.
	let reading = $state<ContinueItem[]>([]);

	// Cover rendering happens here, in this tab, and stops when the page is
	// left. Saying so is the difference between "still working" and "some of my
	// books just do not have covers".
	//
	// It was already counted and never shown, which is the same as not counting
	// it: a shelf half full of typographic covers looked like a shelf of books
	// that have no cover art, rather than like a job somebody interrupted.
	let coversLeft = $state(0);
	let coversRunning = $state(false);
	// Held so the button can stop a run somebody started by hand, and so
	// leaving the page still aborts it.
	let coverRun: AbortController | null = null;

	function startCovers() {
		coverRun?.abort();
		coverRun = new AbortController();
		coversRunning = true;
		void renderMissingCovers(
			(_done, remaining) => {
				coversLeft = remaining;
			},
			coverRun.signal
		).finally(() => {
			coversRunning = false;
		});
	}

	function stopCovers() {
		coverRun?.abort();
		coverRun = null;
		coversRunning = false;
	}

	let scanJob = $state<Job | null>(null);
	let uploadResult = $state<UploadResult | null>(null);
	let uploading = $state(false);
	let dragging = $state(false);

	let sentinel = $state<HTMLElement | null>(null);

	// The query the current list reflects. Changing any input resets the list
	// rather than appending to results from a different query.
	const query = $derived<LibraryQuery>({
		search: search.trim(),
		sort,
		includeMissing,
		owned: owned === 'all' ? undefined : owned,
		limit: 48
	});

	/** Loads the first page for the current query, replacing whatever is shown. */
	// Which shelf is on screen.
	//
	// Every load is stamped with this and its result discarded if it has moved
	// on. Without it, scrolling far enough to fetch page two and then typing in
	// the search box appended page two OF THE PREVIOUS QUERY underneath the new
	// results - the shelf then showed books that do not match what the search
	// box says, with no way to tell which were which.
	let generation = 0;

	async function reload() {
		const mine = ++generation;
		loading = true;
		loadError = null;
		try {
			const page = await api.library(query);
			if (mine !== generation) return;
			items = page.items;
			cursor = page.nextCursor;
		} catch (err) {
			if (mine !== generation) return;
			loadError = err instanceof Error ? err.message : String(err);
		} finally {
			if (mine === generation) loading = false;
		}
	}

	/** Appends the next page. Safe to call spuriously; it no-ops when exhausted. */
	async function loadMore() {
		if (loading || !cursor) return;
		const mine = generation;
		loading = true;
		try {
			const page = await api.library({ ...query, cursor });
			// Cursor pagination guarantees no overlap, so appending is correct -
			// but only onto the shelf it was fetched for.
			if (mine !== generation) return;
			items = [...items, ...page.items];
			cursor = page.nextCursor;
		} catch (err) {
			if (mine !== generation) return;
			loadError = err instanceof Error ? err.message : String(err);
		} finally {
			if (mine === generation) loading = false;
		}
	}

	async function refreshStats() {
		try {
			stats = await api.stats();
		} catch {
			/* the header is decorative; a failure here must not break the shelf */
		}
	}

	async function loadReading() {
		try {
			reading = (await api.continueReading()).items;
		} catch {
			/* the shelf is the page; this row is a convenience */
		}
	}

	/**
	 * Whether anybody has lent this reader a book.
	 *
	 * One row is enough to answer it, and the answer only decides whether to
	 * offer the filter. Asked once when the page opens rather than on every
	 * reload, because a book being shared while somebody scrolls their shelf is
	 * not worth a request per keystroke.
	 */
	async function loadShareAlerts() {
		try {
			const activity = await api.sharingActivity();
			// Only the notable rows reach the shelf. The rest are on the book's
			// own page, which is where somebody goes when they want the detail
			// rather than the alarm.
			shareAlerts = activity.items.filter((a) => a.notable);
		} catch {
			/* nothing lent, or not signed in: there is simply nothing to say */
		}
	}

	async function loadSharedWithMe() {
		try {
			hasShared = (await api.library({ owned: 'shared', limit: 1 })).items.length > 0;
		} catch {
			/* the filter simply stays hidden */
		}
	}

	async function loadPermissions() {
		try {
			const me = await api.me();
			isAdmin = me.isAdmin;
			canUpload = me.canUpload ?? false;
			canAsk = offersAsk(me);
		} catch {
			/* not signed in, or the check failed: show the reader's view, which
			   is the safe default - the server refuses either way */
		}
	}

	async function loadDuplicates() {
		try {
			duplicateCount = (await api.duplicates()).items.length;
		} catch {
			/* a shelf that cannot count duplicates is still a shelf */
			duplicateCount = 0;
		}
	}

	async function runScan() {
		try {
			const accepted = await api.startScan();
			await api.followJob(accepted.jobId, (job) => {
				scanJob = job;
			});
			// The scan queues extraction work, which finishes afterwards, so the
			// shelf is refreshed once more when the queue drains.
			await Promise.all([reload(), refreshStats()]);
			void waitForQueue();
		} catch (err) {
			loadError = err instanceof Error ? err.message : String(err);
		}
	}

	/**
	 * Keeps the shelf in step with a running ingest.
	 *
	 * This watches the queue rather than being driven by a scan started in this
	 * tab. A scan may have been started from another device, by a previous
	 * session, or on startup - and without this the shelf sits reading "empty"
	 * while the header cheerfully reports a hundred books still processing,
	 * which is exactly what it did the first time this page was opened during a
	 * real scan.
	 */
	let watching = $state(false);
	// Set when the shelf goes away, so the queue watcher stops with it.
	//
	// The loop runs for up to thirty minutes, two requests every two seconds,
	// and nothing could interrupt it: start a scan, go and read a book, and the
	// reader competed with a request pair every two seconds for half an hour
	// against a page that no longer existed.
	let gone = false;

	async function waitForQueue() {
		if (watching) return;
		watching = true;
		try {
			for (let i = 0; i < 900 && !gone; i++) {
				await refreshStats();
				if (gone || !stats || stats.queueBusy === 0) break;
				await reload();
				await new Promise((r) => setTimeout(r, 2000));
			}
		} finally {
			watching = false;
			scanJob = null;
			if (!gone) await Promise.all([reload(), refreshStats()]);
		}
	}

	async function handleFiles(files: FileList | null) {
		if (!files || files.length === 0) return;
		uploading = true;
		uploadResult = null;
		try {
			uploadResult = await api.upload(Array.from(files));
			void waitForQueue();
		} catch (err) {
			loadError = err instanceof Error ? err.message : String(err);
		} finally {
			uploading = false;
		}
	}

	// Re-run the query whenever the controls change. Debounced so typing does
	// not fire a request per keystroke.
	let debounce: ReturnType<typeof setTimeout>;
	$effect(() => {
		// Touch the inputs so the effect re-runs when any of them changes.
		void query.search;
		void query.sort;
		void query.includeMissing;
		void query.owned;

		clearTimeout(debounce);
		debounce = setTimeout(reload, 200);
		return () => clearTimeout(debounce);
	});

	$effect(() => {
		void loadPermissions();
		void loadDuplicates();
		void loadReading();
		void loadSharedWithMe();
		void loadShareAlerts();

		// Covers are rendered here, in the browser, because a PDF's first page
		// needs a rasteriser the server does not have. Started after the shelf
		// is on screen and abandoned when the page is left: it must never
		// compete with what someone came here to do.
		const started = setTimeout(startCovers, 1500);
		// On load, pick up an ingest that is already running - started from
		// another tab, another device, or before this page was opened.
		void refreshStats().then(() => {
			if (stats && stats.queueBusy > 0) void waitForQueue();
		});

		return () => {
			clearTimeout(started);
			stopCovers();
			// The queue watcher is a free-running loop, not a timer, so this is
			// the only thing that can stop it.
			gone = true;
		};
	});

	// Infinite scroll. An observer rather than a scroll handler: it fires only
	// when the sentinel is actually near the viewport.
	$effect(() => {
		if (!sentinel) return;
		const observer = new IntersectionObserver(
			(entries) => {
				if (entries[0]?.isIntersecting) void loadMore();
			},
			{ rootMargin: '600px' }
		);
		observer.observe(sentinel);
		return () => observer.disconnect();
	});
</script>

<svelte:head><title>Lexarii</title></svelte:head>

<div
	class="shell"
	class:dragging
	role="region"
	aria-label="Library"
	ondragover={(e) => {
		e.preventDefault();
		dragging = true;
	}}
	ondragleave={() => (dragging = false)}
	ondrop={(e) => {
		e.preventDefault();
		dragging = false;
		void handleFiles(e.dataTransfer?.files ?? null);
	}}
>
	<header>
		<div class="masthead">
			<h1>Lexarii</h1>
			{#if stats}
				<p class="stats">
					{stats.works.toLocaleString()} books · {stats.pages.toLocaleString()} pages · {stats.authors.toLocaleString()}
					authors
					{#if stats.queueBusy > 0}
						<span class="busy">· {stats.queueBusy} still processing</span>
					{/if}
				</p>
			{/if}
		</div>

		<div class="actions">
			{#if canUpload}
				<label class="button file">
					{uploading ? 'Uploading…' : 'Add books'}
					<input
						type="file"
						multiple
						accept=".epub,.pdf,.mobi,.azw3,.cbz,.cbr"
						onchange={(e) => void handleFiles(e.currentTarget.files)}
						disabled={uploading}
					/>
				</label>
			{/if}
			{#if isAdmin}
				<button class="button" onclick={runScan} disabled={!!scanJob}>
					{scanJob ? 'Scanning…' : 'Scan library'}
				</button>
			{/if}
			{#if canAsk}
				<a class="button" href="/ask">Ask</a>
			{/if}
			<a class="button" href="/subjects">Subjects</a>
			<a class="button" href="/collections">Collections</a>
			<a class="button" href="/notes">Notes</a>
			{#if isAdmin}
				<a class="button" href="/suggestions">Suggestions</a>
			{/if}
			<!--
				Offered only when there is something to see. A permanent
				"Duplicates" button on a library with none is a standing
				suggestion that something is wrong.
			-->
			{#if duplicateCount > 0}
				<a class="button attention" href="/duplicates">
					{duplicateCount} duplicate{duplicateCount === 1 ? '' : 's'}
				</a>
			{/if}
			<a class="button" href="/account">Account</a>
		</div>
	</header>

	{#if scanJob}
		<div class="notice" role="status" aria-live="polite">
			<strong>{scanJob.status}</strong>
			{scanJob.progressNote ?? 'working…'}
		</div>
	{/if}

	{#if coversLeft > 0}
		<div class="notice covers" role="status" aria-live="polite">
			<span>
				{#if coversRunning}
					Drawing covers · <strong>{coversLeft.toLocaleString()}</strong> to go. This happens in
					this tab, so leaving the page pauses it.
				{:else}
					<strong>{coversLeft.toLocaleString()}</strong>
					{coversLeft === 1 ? 'book has' : 'books have'} no cover yet.
				{/if}
			</span>
			{#if coversRunning}
				<button class="button quiet" onclick={stopCovers}>Pause</button>
			{:else}
				<button class="button quiet" onclick={startCovers}>Draw them now</button>
			{/if}
		</div>
	{/if}

	{#if uploadResult}
		<div class="notice" role="status" aria-live="polite">
			<strong>
				{uploadResult.summary.stored} added,
				{uploadResult.summary.duplicate} already here,
				{uploadResult.summary.rejected} rejected
			</strong>
			{#if uploadResult.message}<span>{uploadResult.message}</span>{/if}
			{#each uploadResult.files.filter((f) => f.status === 'rejected') as file (file.filename)}
				<span class="rejected">{file.filename}: {file.error}</span>
			{/each}
			<button class="dismiss" onclick={() => (uploadResult = null)}>Dismiss</button>
		</div>
	{/if}

	{#if reading.length > 0 && !search}
		<!-- Hidden while searching: someone who has typed a query is looking
		     for a specific book, not for where they left off. -->
		<section class="reading" aria-label="Continue reading">
			<h2>Continue reading</h2>
			<ul>
				{#each reading.slice(0, 6) as book (book.bookFileId)}
					<li>
						<a href="/read/{book.bookFileId}{book.format === 'pdf' ? '/pdf' : ''}">
							<span class="resume-title">{book.title}</span>
							<span class="resume-progress">
								<span class="bar" style="--read: {Math.round((book.percent ?? 0) * 100)}%"
								></span>
								<span class="pct">{Math.round((book.percent ?? 0) * 100)}%</span>
							</span>
						</a>
					</li>
				{/each}
			</ul>
		</section>
	{/if}

	<div class="controls">
		<input
			class="search"
			type="search"
			placeholder="Search titles, authors, filenames…"
			bind:value={search}
			aria-label="Search the library"
		/>
		<select bind:value={sort} aria-label="Sort by">
			<option value="title">Title</option>
			<option value="author">Author</option>
			<option value="added">Recently added</option>
			<option value="series">Series</option>
		</select>
		<label class="toggle">
			<input type="checkbox" bind:checked={includeMissing} />
			Show missing
		</label>
		{#if hasShared}
			<!-- Only once somebody has actually been lent a book. -->
			<div class="whose" role="group" aria-label="Whose books">
				<button class:active={owned === 'all'} onclick={() => (owned = 'all')}>All</button>
				<button class:active={owned === 'mine'} onclick={() => (owned = 'mine')}>Mine</button>
				<button class:active={owned === 'shared'} onclick={() => (owned = 'shared')}>
					Shared with me
				</button>
			</div>
		{/if}
	</div>

	{#if shareAlerts.length > 0}
		<!-- Above the shelf, and not dismissible: this says somebody took most of
		     a book you lent them, which is the thing the log exists for. It
		     links to the book, where the full record and the controls to
		     withdraw access already live. -->
		<aside class="alert" role="status">
			<strong>
				{shareAlerts.length === 1 ? 'A book you lent was' : 'Books you lent were'} read in bulk
			</strong>
			<ul>
				{#each shareAlerts.slice(0, 4) as a (a.bookFileId + a.day + a.kind)}
					<li>
						<a href="/works/{a.workId}">{a.workTitle}</a>
						— {a.username}
						{#if a.kind === 'file'}
							downloaded the whole file
						{:else}
							read {a.units}{a.of ? ` of ${a.of}` : ''}
							{a.kind === 'page' ? 'pages' : 'chapters'}
						{/if}
						on {a.day}
					</li>
				{/each}
			</ul>
			{#if shareAlerts.length > 4}
				<p class="more">and {shareAlerts.length - 4} more</p>
			{/if}
		</aside>
	{/if}

	{#if loadError}
		<p class="error" role="alert">{loadError}</p>
	{/if}

	{#if items.length > 0}
		<div class="grid">
			{#each items as item (item.workId)}
				<BookCard {item} />
			{/each}
		</div>
		<div bind:this={sentinel} class="sentinel" aria-hidden="true"></div>
		{#if loading}<p class="loading">Loading more…</p>{/if}
	{:else if loading}
		<p class="loading">Loading…</p>
	{:else if search.trim()}
		<p class="empty">Nothing matches “{search}”.</p>
	{:else}
		<div class="empty">
			<p>The shelf is empty.</p>
			<p class="hint">
				Drop books here, use <strong>Add books</strong>, or point
				<code>LEXARII_LIBRARY_DIR</code> at a folder and press <strong>Scan library</strong>.
			</p>
		</div>
	{/if}
</div>

<style>
	.shell {
		max-width: 1400px;
		margin-inline: auto;
		padding: var(--space-6) var(--space-4) var(--space-16);
	}

	.shell.dragging {
		outline: 2px dashed var(--accent);
		outline-offset: -8px;
		border-radius: var(--radius-lg);
	}

	header {
		display: flex;
		flex-wrap: wrap;
		gap: var(--space-4);
		align-items: flex-end;
		justify-content: space-between;
		margin-bottom: var(--space-6);
	}

	h1 {
		font-family: var(--font-serif);
		font-size: var(--text-2xl);
		line-height: var(--leading-tight);
		margin: 0;
	}

	.stats {
		margin: var(--space-1) 0 0;
		font-size: var(--text-sm);
		color: var(--fg-muted);
	}

	.busy {
		color: var(--accent);
	}

	.actions {
		display: flex;
		gap: var(--space-2);
		flex-wrap: wrap;
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

	.button.attention {
		/* Said quietly. A duplicate is worth mentioning and is not an error,
		   and a red badge on a library would read as one. */
		border-color: var(--accent);
		color: var(--accent);
	}

	.button:hover {
		background: var(--bg-sunken);
	}

	.button[disabled] {
		opacity: 0.6;
		cursor: default;
	}

	.file input {
		/* Visually hidden, still focusable and still a real control. */
		position: absolute;
		width: 1px;
		height: 1px;
		opacity: 0;
		pointer-events: none;
	}

	/* Continue reading. A horizontal strip rather than cards: these are books
	   already known to the reader, so a title and how far in is all that is
	   needed, and a second grid above the shelf would compete with it. */
	.reading {
		margin-bottom: var(--space-6);
	}

	.reading h2 {
		margin: 0 0 var(--space-2);
		font-size: var(--text-sm);
		font-weight: 500;
		color: var(--fg-muted);
		text-transform: uppercase;
		letter-spacing: 0.06em;
	}

	.reading ul {
		display: flex;
		gap: var(--space-2);
		margin: 0;
		padding: 0 0 var(--space-1);
		list-style: none;
		overflow-x: auto;
		/* Snap so a half-visible card does not look like a rendering fault. */
		scroll-snap-type: x proximity;
	}

	.reading li {
		scroll-snap-align: start;
		flex: 0 0 auto;
	}

	.reading a {
		display: flex;
		flex-direction: column;
		gap: var(--space-2);
		justify-content: space-between;
		width: 13rem;
		min-height: 44px;
		padding: var(--space-3);
		border: 1px solid var(--border);
		border-radius: var(--radius-md);
		background: var(--bg-raised);
		color: inherit;
		text-decoration: none;
		transition: border-color var(--duration-fast) var(--ease-out);
	}

	.reading a:hover {
		border-color: var(--fg-subtle);
	}

	.resume-title {
		font-size: var(--text-sm);
		line-height: 1.3;
		display: -webkit-box;
		-webkit-line-clamp: 2;
		line-clamp: 2;
		-webkit-box-orient: vertical;
		overflow: hidden;
	}

	.resume-progress {
		display: flex;
		align-items: center;
		gap: var(--space-2);
	}

	.resume-progress .bar {
		flex: 1;
		height: 3px;
		border-radius: 2px;
		background: var(--bg-sunken);
	}

	.resume-progress .bar::after {
		content: '';
		display: block;
		height: 100%;
		width: var(--read);
		border-radius: 2px;
		background: var(--accent);
	}

	.resume-progress .pct {
		font-size: var(--text-xs);
		color: var(--fg-muted);
		font-variant-numeric: tabular-nums;
	}

	.controls {
		display: flex;
		flex-wrap: wrap;
		gap: var(--space-2);
		margin-bottom: var(--space-6);
	}

	.search {
		flex: 1 1 18rem;
		min-height: 44px;
		padding: 0 var(--space-3);
		border: 1px solid var(--border);
		border-radius: var(--radius-md);
		background: var(--bg-raised);
		color: var(--fg);
		font: inherit;
	}

	select {
		min-height: 44px;
		padding: 0 var(--space-2);
		border: 1px solid var(--border);
		border-radius: var(--radius-md);
		background: var(--bg-raised);
		color: var(--fg);
		font: inherit;
	}

	.toggle {
		display: inline-flex;
		align-items: center;
		gap: var(--space-2);
		min-height: 44px;
		font-size: var(--text-sm);
		color: var(--fg-muted);
	}

	.alert {
		margin-bottom: var(--space-4);
		padding: var(--space-3) var(--space-4);
		border: 1px solid var(--border);
		border-left: 3px solid var(--danger);
		border-radius: var(--radius-md);
		background: var(--bg-raised);
		font-size: var(--text-sm);
	}

	.alert strong {
		display: block;
		margin-bottom: var(--space-2);
		color: var(--fg);
	}

	.alert ul {
		margin: 0;
		padding-left: var(--space-4);
		color: var(--fg-muted);
	}

	.alert li {
		margin-bottom: var(--space-1);
	}

	.alert a {
		color: var(--fg);
	}

	.alert .more {
		margin: var(--space-2) 0 0;
		color: var(--fg-subtle);
	}

	/* A segmented control rather than a fourth dropdown: three options, one of
	   which is on at all times, and the current one has to be readable at a
	   glance from across the page. */
	.whose {
		display: inline-flex;
		border: 1px solid var(--border);
		border-radius: var(--radius-md);
		overflow: hidden;
	}

	.whose button {
		min-height: 44px;
		padding: 0 var(--space-3);
		border: 0;
		border-right: 1px solid var(--border);
		background: var(--bg-raised);
		color: var(--fg-muted);
		font: inherit;
		font-size: var(--text-sm);
		cursor: pointer;
	}

	.whose button:last-child {
		border-right: 0;
	}

	.whose button.active {
		background: var(--accent);
		color: var(--accent-fg);
	}

	.grid {
		display: grid;
		grid-template-columns: repeat(auto-fill, minmax(150px, 1fr));
		gap: var(--space-6) var(--space-4);
	}

	/*
	 * Browser-native virtualisation. A shelf of several thousand cards would
	 * otherwise pay full layout and paint for everything off-screen; this skips
	 * both, while contain-intrinsic-size keeps the scrollbar honest.
	 *
	 * Preferred over hand-rolled windowing because it survives a responsive
	 * grid whose column count changes with the viewport.
	 */
	.grid > :global(*) {
		content-visibility: auto;
		contain-intrinsic-size: auto 280px;
	}

	.sentinel {
		height: 1px;
	}

	.notice {
		display: flex;
		flex-wrap: wrap;
		gap: var(--space-2);
		align-items: center;
		margin-bottom: var(--space-4);
		padding: var(--space-3) var(--space-4);
		border: 1px solid var(--border);
		border-radius: var(--radius-md);
		background: var(--bg-raised);
		font-size: var(--text-sm);
	}

	/* The sentence takes the room and the control sits at the end of it, until
	   the line wraps on a phone and the button goes underneath. */
	.covers span {
		flex: 1 1 16rem;
	}

	/* Smaller than a header action, because this is an offer rather than
	   something anybody came to the page to press. It keeps the 44px target:
	   the smaller thing is the text, not the tap area. */
	.button.quiet {
		padding: 0 var(--space-3);
		background: transparent;
		font-size: var(--text-xs);
	}

	.rejected {
		color: var(--danger);
	}

	.dismiss {
		margin-left: auto;
		border: 0;
		background: none;
		color: var(--fg-muted);
		cursor: pointer;
		font: inherit;
		min-height: 44px;
	}

	.error {
		color: var(--danger);
		font-size: var(--text-sm);
	}

	.loading,
	.empty {
		color: var(--fg-muted);
		font-size: var(--text-sm);
		padding: var(--space-8) 0;
	}

	.hint {
		color: var(--fg-subtle);
		max-width: var(--measure);
	}

	code {
		font-family: var(--font-mono);
		font-size: 0.9em;
	}

	@media (max-width: 480px) {
		.grid {
			grid-template-columns: repeat(auto-fill, minmax(130px, 1fr));
			gap: var(--space-4) var(--space-3);
		}

		/* Five buttons wrapped onto two rows and pushed the shelf below the
		   fold: on a phone the first thing seen was a toolbar. They scroll
		   sideways in one row instead, which costs one line and no books. */
		.actions {
			flex-wrap: nowrap;
			overflow-x: auto;
			/* The row is a scroll container, so its own padding is what keeps
			   the last button clear of the edge. */
			padding-bottom: var(--space-1);
			margin: 0 calc(var(--space-4) * -1);
			padding-inline: var(--space-4);
			scrollbar-width: none;
		}

		.actions::-webkit-scrollbar {
			display: none;
		}

		.actions > :global(*) {
			flex: 0 0 auto;
		}

		header {
			gap: var(--space-3);
			margin-bottom: var(--space-4);
		}

		/* A phone is held in one hand: a book already being read is worth more
		   of the screen than the rest of the shelf. */
		.reading a {
			width: 11rem;
		}
	}
</style>
